package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (r composeBackend) launch(ctx context.Context, attempt state.Attempt, cfg config.Project, initial Result, progress Progress) (result Result, err error) {
	result = initial
	result.ComposeProject = projectNameForAttempt(attempt)
	checkout := filepath.Join(result.Workspace, "source")
	logger := slog.With("deployment", attempt.ID)
	stage, summary := state.PrepareStage, "Could not prepare the deployment."
	var outputExcerpt string
	defer func() {
		if err != nil {
			result.Diagnostic = &state.Diagnostic{Stage: stage, Summary: summary, Output: outputExcerpt}
		}
	}()
	logger.Debug("checking candidate Compose configuration")
	sourceCompose := filepath.Join(checkout, cfg.ComposeFile)
	targetPort := cfg.ContainerPort()
	content, err := prepareCompose(sourceCompose, checkout, cfg.PublicService, targetPort)
	if err != nil {
		return result, fmt.Errorf("Compose policy: %w", err)
	}
	compose := filepath.Join(result.Workspace, "preview-compose.yaml")
	if err := os.WriteFile(compose, content, 0600); err != nil {
		return result, err
	}
	envFile, err := writeComposeEnv(result.Workspace, cfg, targetPort)
	if err != nil {
		return result, err
	}
	defer os.Remove(envFile)
	if err := progress(state.Building, result); err != nil {
		return result, err
	}
	args := []string{"compose", "--project-directory", filepath.Dir(sourceCompose), "--env-file", envFile, "-p", result.ComposeProject, "-f", compose}
	runCompose := func(limit int, command ...string) (string, error) {
		return r.runDocker(ctx, limit, len(cfg.ComposeEnv) > 0 || len(cfg.ComposeEnvFrom) > 0, append(args, command...)...)
	}
	logger.Debug("rendering candidate Compose configuration")
	output, err := r.runDocker(ctx, 2<<20, true, append(args, "config", "--format", "json")...)
	if err != nil {
		return result, fmt.Errorf("resolve Compose configuration: %w", err)
	}
	if err := validateResolved([]byte(output), cfg.PublicService, targetPort, checkout); err != nil {
		return result, fmt.Errorf("resolved Compose policy: %w", err)
	}
	confidential := len(cfg.ComposeEnv) > 0 || len(cfg.ComposeEnvFrom) > 0
	stage, summary = state.BuildStage, "Docker Compose could not build the stack."
	logger.Debug("building Compose stack")
	outputExcerpt, err = r.runDeploymentStep(ctx, confidential, append(args, "build")...)
	if err != nil {
		return result, fmt.Errorf("build Compose stack: %w", err)
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	stage, summary = state.StartStage, "Docker Compose could not start the stack."
	logger.Debug("starting Compose stack")
	outputExcerpt, err = r.runDeploymentStep(ctx, confidential, append(args, "up", "-d", "--no-build", "--wait", "--wait-timeout", "180")...)
	if err != nil {
		return result, fmt.Errorf("start Compose stack: %w", err)
	}
	portText, err := runCompose(1024, "port", cfg.PublicService, strconv.Itoa(targetPort))
	if err != nil {
		return result, fmt.Errorf("read public port: %w", err)
	}
	result.Port, err = publishedPort(portText)
	if err != nil {
		return result, err
	}
	return result, nil
}

func writeComposeEnv(workspace string, cfg config.Project, port int) (string, error) {
	var content strings.Builder
	fmt.Fprintf(&content, "PREVIEW_PORT=%d\n", port)
	for name, value := range cfg.ComposeEnv {
		if err := appendEnvValue(&content, name, value); err != nil {
			return "", err
		}
	}
	for name, source := range cfg.ComposeEnvFrom {
		value, ok := os.LookupEnv(source)
		if !ok {
			return "", fmt.Errorf("Compose variable %s requires controller environment variable %s", name, source)
		}
		if err := appendEnvValue(&content, name, value); err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp(workspace, ".compose-env-")
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(content.String())
	if err := errors.Join(writeErr, f.Close()); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

var composeEnvEscapes = strings.NewReplacer(
	`\`, `\\`, `"`, `\"`, "$", "$$", "\r", `\r`, "\n", `\n`, "\t", `\t`,
)

func appendEnvValue(content *strings.Builder, name, value string) error {
	for _, char := range value {
		if char < ' ' && char != '\r' && char != '\n' && char != '\t' {
			return fmt.Errorf("Compose variable %s contains an unsupported control character", name)
		}
	}
	fmt.Fprintf(content, "%s=\"%s\"\n", name, composeEnvEscapes.Replace(value))
	return nil
}

func (r composeBackend) remove(ctx context.Context, attempt state.Attempt) error {
	name := projectNameForAttempt(attempt)
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
	if len(problems) == 0 {
		if err := r.removeBuildImages(ctx, name); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
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
	if _, exists := model.Services[publicService]; !exists {
		return errors.New("resolved Compose model is missing the public service")
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

// Only remove Compose-generated tags for images labeled as builds of this project.
func (r *Runtime) removeBuildImages(ctx context.Context, project string) error {
	output, err := r.docker(ctx, 64<<10, "image", "ls", "--filter", "label=com.docker.compose.project="+project,
		"--filter", "reference="+project+"-*:latest", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return err
	}
	var problems []error
	for _, image := range strings.Fields(output) {
		users, err := r.docker(ctx, 1024, "container", "ls", "-aq", "--filter", "ancestor="+image)
		if err == nil && strings.TrimSpace(users) == "" {
			_, err = r.docker(ctx, 64<<10, "image", "rm", "--no-prune", image)
		}
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func (r composeBackend) running(ctx context.Context, attempt state.Attempt) (bool, error) {
	name := projectNameForAttempt(attempt)
	output, err := r.docker(ctx, 64<<10, "container", "ls", "-q", "--filter", "label=com.docker.compose.project="+name, "--filter", "status=running")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(output) == "" {
		return false, nil
	}
	return true, nil
}
