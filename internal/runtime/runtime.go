package runtime

import (
	"context"

	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
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
	Diagnostic     *state.Diagnostic
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
	logger := slog.With("deployment", attempt.ID)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	backend, err := r.backend(attempt)
	if err != nil {
		return result, err
	}
	result.Workspace = filepath.Join(r.root, projectName, strconv.FormatInt(intent, 10), attempt.ID)
	defer func() {
		if err != nil && result.Diagnostic == nil {
			result.Diagnostic = &state.Diagnostic{Stage: state.PrepareStage, Summary: "Could not prepare the deployment."}
		}
	}()
	checkout := filepath.Join(result.Workspace, "source")
	if err := os.MkdirAll(result.Workspace, 0700); err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			logger.Debug("cleaning up failed candidate")
			cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
			defer done()
			if cleanupErr := r.Cleanup(cleanup, projectName, intent, attempt); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup candidate: %w", cleanupErr))
			}
		}
	}()
	logger.Debug("cloning candidate repository")
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
	result, err = backend.launch(ctx, attempt, cfg, result, progress)
	if err != nil {
		return result, err
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}

	logger.Debug("probing public service", "port", result.Port)
	if err := r.probe(ctx, result.Port, cfg.HealthPath); err != nil {
		result.Diagnostic = &state.Diagnostic{Stage: state.HealthStage, Summary: err.Error()}
		return result, errStartupHealth
	}
	return result, nil
}

func (r *Runtime) Cleanup(ctx context.Context, projectName string, intent int64, attempt state.Attempt) error {
	if !attemptID.MatchString(attempt.ID) || intent < 1 || projectName == "" || strings.ContainsAny(projectName, "/\\.") {
		return errors.New("invalid cleanup identity")
	}
	backend, err := r.backend(attempt)
	if err != nil {
		return err
	}
	workspace := filepath.Join(r.root, projectName, strconv.FormatInt(intent, 10), attempt.ID)
	return errors.Join(backend.remove(ctx, attempt), os.RemoveAll(workspace))
}

// HealthyPort resolves the attempt's current upstream and probes it before use.
func (r *Runtime) HealthyPort(ctx context.Context, attempt state.Attempt, cfg config.Project) (int, error) {
	backend, err := r.backend(attempt)
	if err != nil {
		return 0, err
	}
	port, err := backend.currentPort(ctx, attempt)
	if err != nil || port == 0 {
		return 0, err
	}
	if err := r.ProbeHealth(ctx, port, cfg.HealthPath); err != nil {
		return 0, err
	}
	return port, nil
}

func (r *Runtime) docker(ctx context.Context, limit int, args ...string) (string, error) {
	return r.runDocker(ctx, limit, false, args...)
}

func (r *Runtime) runDocker(ctx context.Context, limit int, confidential bool, args ...string) (string, error) {
	cmd := r.dockerCommand(ctx, args...)
	output := cappedOutput{limit: limit}
	stderr := cappedOutput{limit: limit}
	cmd.Stdout, cmd.Stderr = &output, &stderr
	err := cmd.Run()
	text := output.String()
	if output.truncated || stderr.truncated {
		return text, errors.New("Docker output exceeded limit")
	}
	if err != nil {
		if confidential {
			return "", fmt.Errorf("Docker Compose command failed: %w", err)
		}
		return text, fmt.Errorf("docker %s: %w: %s%s", strings.Join(args, " "), err, text, stderr.String())
	}
	return text, nil
}

var errStartupHealth = errors.New("public service did not pass its health probe")

func (r *Runtime) probe(ctx context.Context, port int, path string) error {
	deadline, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := r.ProbeHealth(deadline, port, path)
		if err == nil {
			return nil
		}
		select {
		case <-deadline.Done():
			return err
		case <-time.After(2 * time.Second):
		}
	}
}

// ProbeHealth checks one HTTP response from the preview's loopback endpoint.
func (r *Runtime) ProbeHealth(ctx context.Context, port int, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return errors.New("invalid health endpoint")
	}
	response, err := r.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("health check timed out")
		}
		return errors.New("health endpoint did not respond")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
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

func publishedPort(output string) (int, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(output))
	if err != nil || host != "127.0.0.1" {
		return 0, errors.New("unexpected public port mapping")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return 0, errors.New("invalid assigned public port")
	}
	return number, nil
}
