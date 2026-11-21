package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

const attemptLabel = "preview-controller.attempt"

func (r dockerBackend) launch(ctx context.Context, a state.Attempt, cfg config.Project, result Result, progress Progress) (Result, error) {
	checkout := filepath.Join(result.Workspace, "source")
	// These files can be opened by the build client before it sends the context.
	for _, name := range []string{"Dockerfile", ".dockerignore", "Dockerfile.dockerignore"} {
		info, err := os.Lstat(filepath.Join(checkout, name))
		if errors.Is(err, os.ErrNotExist) && name != "Dockerfile" {
			continue
		}
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() {
			return result, fmt.Errorf("%s must be a regular file", name)
		}
	}
	name := projectNameForAttempt(a)
	label := attemptLabel + "=" + a.ID
	if err := progress(state.Building, result); err != nil {
		return result, err
	}
	excerpt, err := r.runDeploymentStep(ctx, false, "build", "--label", label, "--tag", name, checkout)
	if err != nil {
		result.Diagnostic = &state.Diagnostic{Stage: "Docker build", Summary: "Docker could not build the image.", Output: excerpt}
		return result, err
	}
	target := cfg.PublicPort
	if target == 0 {
		exposed, err := r.docker(ctx, 64<<10, "image", "inspect", "--format", "{{json .Config.ExposedPorts}}", name)
		if err != nil {
			return result, err
		}
		target, err = inferTCPPort(exposed)
		if err != nil {
			result.Diagnostic = &state.Diagnostic{Stage: state.PrepareStage, Summary: err.Error()}
			return result, err
		}
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	if _, err := r.docker(ctx, 1024, "network", "create", "--label", label, name); err != nil {
		return result, err
	}
	excerpt, err = r.runDeploymentStep(ctx, false, "run", "--detach", "--name", name, "--network", name, "--restart", "unless-stopped", "--label", label,
		"--label", "com.docker.compose.project=docker-"+name, "--publish", fmt.Sprintf("127.0.0.1::%d/tcp", target), name)
	if err != nil {
		result.Diagnostic = &state.Diagnostic{Stage: "Docker startup", Summary: "Docker could not start the web container.", Output: excerpt}
		return result, err
	}
	result.Port, err = r.endpoint(ctx, name)
	return result, err
}

func inferTCPPort(exposed string) (int, error) {
	var ports map[string]json.RawMessage
	if err := json.Unmarshal([]byte(exposed), &ports); err != nil {
		return 0, err
	}
	var selected int
	for key := range ports {
		number, tcp := strings.CutSuffix(key, "/tcp")
		if !tcp {
			continue
		}
		port, err := strconv.Atoi(number)
		if err != nil || port < 1 || port > 65535 || selected != 0 {
			return 0, errPublicPort
		}
		selected = port
	}
	if selected == 0 {
		return 0, errPublicPort
	}
	return selected, nil
}

var errPublicPort = errors.New("set public_port when the built image does not expose exactly one TCP port")

// Only one TCP port is published by launch. Its mapping can change on restart.
func (r dockerBackend) endpoint(ctx context.Context, id string) (int, error) {
	out, err := r.docker(ctx, 1024, "port", id)
	if err != nil {
		return 0, err
	}
	_, binding, ok := strings.Cut(strings.TrimSpace(out), "/tcp -> ")
	if !ok {
		return 0, errors.New("missing published TCP port")
	}
	return publishedPort(binding)
}

func (r dockerBackend) container(ctx context.Context, a state.Attempt) (string, error) {
	out, err := r.docker(ctx, 1024, "container", "ls", "-aq", "--no-trunc", "--filter", "label="+attemptLabel+"="+a.ID, "--filter", "name=^/"+projectNameForAttempt(a)+"$")
	return strings.TrimSpace(out), err
}

func (r dockerBackend) running(ctx context.Context, a state.Attempt) (bool, error) {
	id, err := r.container(ctx, a)
	if err != nil || id == "" {
		return false, err
	}
	status, err := r.docker(ctx, 1024, "inspect", "--format", "{{.State.Running}}", id)
	if err != nil || strings.TrimSpace(status) != "true" {
		return false, err
	}
	port, err := r.endpoint(ctx, id)
	return err == nil && port == a.Port, err
}

func (r dockerBackend) stop(ctx context.Context, a state.Attempt) error {
	id, err := r.container(ctx, a)
	if err != nil || id == "" {
		return err
	}
	_, err = r.docker(ctx, 1024, "stop", id)
	return err
}

func (r dockerBackend) restore(ctx context.Context, _ string, _ int64, cfg config.Project, a state.Attempt) (int, error) {
	id, err := r.container(ctx, a)
	if err != nil {
		return 0, err
	}
	if id == "" {
		return 0, errors.New("archived container unavailable")
	}
	if _, err := r.docker(ctx, 1024, "start", id); err != nil {
		return 0, err
	}
	port, err := r.endpoint(ctx, id)
	if err != nil {
		return 0, err
	}
	if err := r.probe(ctx, port, cfg.HealthPath); err != nil {
		return 0, errStartupHealth
	}
	return port, nil
}

func (r dockerBackend) logs(ctx context.Context, a state.Attempt) (func(io.Writer) error, error) {
	id, err := r.container(ctx, a)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrLogsUnavailable
	}
	return r.followLogs(ctx, true, "logs", "--tail", "100", "--follow", id), nil
}

func (r dockerBackend) remove(ctx context.Context, a state.Attempt) error {
	id, err := r.container(ctx, a)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := r.docker(ctx, 1024, "container", "rm", "-fv", id); err != nil {
			return err
		}
	}
	name := projectNameForAttempt(a)
	filter := "label=" + attemptLabel + "=" + a.ID
	networks, err := r.docker(ctx, 1024, "network", "ls", "--filter", filter, "--filter", "name=^"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return err
	}
	for _, network := range strings.Fields(networks) {
		if _, err := r.docker(ctx, 1024, "network", "rm", network); err != nil {
			return err
		}
	}
	tag := name + ":latest"
	images, err := r.docker(ctx, 1024, "image", "ls", "--filter", filter, "--filter", "reference="+tag, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(images) == tag {
		_, err = r.docker(ctx, 1024, "image", "rm", "--no-prune", tag)
	}
	return err
}
