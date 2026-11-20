package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

var attemptID = regexp.MustCompile(`^deployment-[0-9a-f]{32}$`)

type Result struct {
	Port           int
	Workspace      string
	ComposeProject string
}

type Progress func(state.Status, Result) error

type Runtime struct {
	root         string
	dockerConfig string
	client       *http.Client
}

func New(root string) (*Runtime, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("workspace root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace root must be a real directory")
	}
	dockerConfig := filepath.Join(root, ".docker-config")
	if err := os.MkdirAll(dockerConfig, 0700); err != nil {
		return nil, err
	}
	info, err = os.Lstat(dockerConfig)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Docker configuration path must be a real directory")
	}
	return &Runtime{root: root, dockerConfig: dockerConfig, client: &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (r *Runtime) Run(ctx context.Context, projectName string, intent int64, cfg config.Project, attempt state.Attempt, progress Progress) (result Result, err error) {
	if !attemptID.MatchString(attempt.ID) || intent < 1 {
		return Result{}, errors.New("invalid deployment identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	result.ComposeProject = "pv" + strings.TrimPrefix(attempt.ID, "deployment-")
	result.Workspace = filepath.Join(r.root, projectName, strconv.FormatInt(intent, 10), attempt.ID)
	checkout := filepath.Join(result.Workspace, "source")
	if err := os.MkdirAll(result.Workspace, 0700); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
			defer done()
			if cleanupErr := r.Cleanup(cleanup, projectName, intent, attempt); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup candidate: %w", cleanupErr))
			}
		}
	}()
	repo, err := git.PlainCloneContext(ctx, checkout, &git.CloneOptions{URL: cfg.Repository, SingleBranch: false})
	if err != nil {
		return result, fmt.Errorf("clone public repository: %w", err)
	}
	hash, ok := plumbing.FromHex(attempt.Commit)
	if !ok {
		return result, errors.New("invalid commit hash")
	}
	if _, err := repo.CommitObject(hash); err != nil {
		return result, fmt.Errorf("commit object is unavailable: %w", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return result, err
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Hash: hash, Force: true}); err != nil {
		return result, fmt.Errorf("checkout exact commit: %w", err)
	}
	compose := filepath.Join(checkout, cfg.ComposeFile)
	if err := ValidateCompose(compose, checkout, cfg.PublicService); err != nil {
		return result, fmt.Errorf("Compose policy: %w", err)
	}
	overlay := filepath.Join(result.Workspace, "overlay.yaml")
	content := fmt.Sprintf("services:\n  %s:\n    ports:\n      - \"127.0.0.1::%d\"\n", cfg.PublicService, cfg.PublicPort)
	if err := os.WriteFile(overlay, []byte(content), 0600); err != nil {
		return result, err
	}
	if err := progress(state.Building, result); err != nil {
		return result, err
	}
	args := []string{"compose", "-p", result.ComposeProject, "-f", compose, "-f", overlay}
	output, err := r.docker(ctx, 2<<20, append(args, "config", "--format", "json")...)
	if err != nil {
		return result, fmt.Errorf("resolve Compose configuration: %w", err)
	}
	if err := validateResolved([]byte(output), cfg.PublicService, cfg.PublicPort, checkout); err != nil {
		return result, fmt.Errorf("resolved Compose policy: %w", err)
	}
	if _, err := r.docker(ctx, 2<<20, append(args, "build")...); err != nil {
		return result, fmt.Errorf("build Compose stack: %w", err)
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	if _, err := r.docker(ctx, 2<<20, append(args, "up", "-d", "--no-build", "--wait", "--wait-timeout", "180")...); err != nil {
		return result, fmt.Errorf("start Compose stack: %w", err)
	}
	portText, err := r.docker(ctx, 1024, append(args, "port", cfg.PublicService, strconv.Itoa(cfg.PublicPort))...)
	if err != nil {
		return result, fmt.Errorf("read public port: %w", err)
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(portText))
	if err != nil || host != "127.0.0.1" {
		return result, fmt.Errorf("unexpected public port mapping %q", strings.TrimSpace(portText))
	}
	result.Port, err = strconv.Atoi(port)
	if err != nil || result.Port < 1 || result.Port > 65535 {
		return result, errors.New("invalid assigned public port")
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	if err := r.probe(ctx, result.Port, cfg.HealthPath); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Runtime) Cleanup(ctx context.Context, projectName string, intent int64, attempt state.Attempt) error {
	if !attemptID.MatchString(attempt.ID) || intent < 1 || projectName == "" || strings.ContainsAny(projectName, "/\\.") {
		return errors.New("invalid cleanup identity")
	}
	name := "pv" + strings.TrimPrefix(attempt.ID, "deployment-")
	label := "label=com.docker.compose.project=" + name
	var problems []error
	for _, kind := range []struct {
		list   []string
		remove []string
	}{
		{[]string{"container", "ls", "-aq", "--filter", label}, []string{"container", "rm", "-f"}},
		{[]string{"volume", "ls", "-q", "--filter", label}, []string{"volume", "rm"}},
		{[]string{"network", "ls", "-q", "--filter", label}, []string{"network", "rm"}},
	} {
		output, err := r.docker(ctx, 64<<10, kind.list...)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		ids := strings.Fields(output)
		if len(ids) > 0 {
			if _, err := r.docker(ctx, 64<<10, append(kind.remove, ids...)...); err != nil {
				problems = append(problems, err)
			}
		}
	}
	workspace := filepath.Join(r.root, projectName, strconv.FormatInt(intent, 10), attempt.ID)
	if err := os.RemoveAll(workspace); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func (r *Runtime) Check(ctx context.Context, attempt state.Attempt, cfg config.Project) (bool, error) {
	if !attemptID.MatchString(attempt.ID) || attempt.Port < 1 {
		return false, nil
	}
	name := "pv" + strings.TrimPrefix(attempt.ID, "deployment-")
	output, err := r.docker(ctx, 64<<10, "container", "ls", "-q", "--filter", "label=com.docker.compose.project="+name, "--filter", "status=running")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(output) == "" {
		return false, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", attempt.Port, cfg.HealthPath), nil)
	if err != nil {
		return false, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode >= 200 && response.StatusCode < 300, nil
}

func (r *Runtime) docker(ctx context.Context, limit int, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DOCKER_CONFIG=" + r.dockerConfig}
	var output cappedOutput
	output.limit = limit
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	text := output.String()
	if output.truncated {
		return text, errors.New("Docker output exceeded limit")
	}
	if err != nil {
		return text, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

func (r *Runtime) probe(ctx context.Context, port int, path string) error {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	for {
		request, err := http.NewRequestWithContext(deadline, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := r.client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-deadline.Done():
			return errors.New("public service did not pass its health probe")
		case <-time.After(2 * time.Second):
		}
	}
}

type cappedOutput struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.data)+n > b.limit {
		b.truncated = true
		keep := b.limit - len(b.data)
		if keep > 0 {
			b.data = append(b.data, p[:keep]...)
		}
	} else {
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *cappedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func validateResolved(data []byte, publicService string, targetPort int, checkout string) error {
	var model struct {
		Services map[string]struct {
			Ports []struct {
				HostIP   string `json:"host_ip"`
				Target   int    `json:"target"`
				Protocol string `json:"protocol"`
			} `json:"ports"`
			Privileged  bool   `json:"privileged"`
			NetworkMode string `json:"network_mode"`
			Volumes     []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &model); err != nil {
		return err
	}
	if len(model.Services) == 0 {
		return errors.New("resolved Compose model has no services")
	}
	for name, service := range model.Services {
		if service.Privileged || service.NetworkMode == "host" {
			return fmt.Errorf("service %q has unsafe host access", name)
		}
		if name == publicService {
			if len(service.Ports) != 1 || service.Ports[0].HostIP != "127.0.0.1" || service.Ports[0].Target != targetPort || service.Ports[0].Protocol != "tcp" {
				return errors.New("public service must have one loopback TCP port")
			}
		} else if len(service.Ports) != 0 {
			return fmt.Errorf("service %q publishes a port", name)
		}
		for _, volume := range service.Volumes {
			if volume.Type == "bind" {
				if err := resolvedInsideCheckout(checkout, volume.Source); err != nil {
					return fmt.Errorf("service %q bind mount: %w", name, err)
				}
			}
		}
	}
	return nil
}

func resolvedInsideCheckout(checkout, source string) error {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return err
	}
	target, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes checkout")
	}
	return nil
}
