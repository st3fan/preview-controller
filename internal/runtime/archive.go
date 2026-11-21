package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
	"gopkg.in/yaml.v3"
)

func (r composeBackend) stop(ctx context.Context, attempt state.Attempt) error {
	if !attemptID.MatchString(attempt.ID) {
		return errors.New("invalid deployment identity")
	}
	output, err := r.docker(ctx, 64<<10, "container", "ls", "-aq", "--filter", "label=com.docker.compose.project="+projectNameForAttempt(attempt))
	if err != nil {
		return err
	}
	ids := strings.Fields(output)
	if len(ids) == 0 {
		return nil
	}
	output, err = r.docker(ctx, 64<<10, append([]string{"container", "inspect", "--format", "{{.Id}} {{.HostConfig.RestartPolicy.Name}} {{.State.Running}}"}, ids...)...)
	if err != nil {
		return err
	}
	var always, running []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return errors.New("unexpected container status")
		}
		if fields[1] == "always" {
			always = append(always, fields[0])
		}
		if fields[2] == "true" {
			running = append(running, fields[0])
		}
	}
	// Keep normal daemon restart behavior for running services while honoring an explicit stop.
	if len(always) > 0 {
		if _, err := r.docker(ctx, 64<<10, append([]string{"container", "update", "--restart", "unless-stopped"}, always...)...); err != nil {
			return err
		}
	}
	if len(running) > 0 {
		_, err = r.docker(ctx, 64<<10, append([]string{"container", "stop"}, running...)...)
	}
	return err
}

func (r composeBackend) restore(ctx context.Context, project string, intent int64, cfg config.Project, attempt state.Attempt) (int, error) {
	if !attemptID.MatchString(attempt.ID) || intent < 1 || project == "" || strings.ContainsAny(project, "/\\.") {
		return 0, errors.New("invalid deployment identity")
	}
	workspace := filepath.Join(r.root, project, strconv.FormatInt(intent, 10), attempt.ID)
	// The prepared file records the services, dependencies and loopback port
	// selected for this attempt, including for attempts saved before backend
	// selection existed. Starting its existing containers needs no build,
	// environment interpolation or access to current Compose settings.
	manifest, service, target, err := restoreManifest(workspace)
	if err != nil {
		return 0, err
	}
	defer os.Remove(manifest)
	args := []string{"compose", "--env-file", os.DevNull, "-p", projectNameForAttempt(attempt), "-f", manifest}
	run := func(command ...string) (string, error) { return r.docker(ctx, 64<<10, append(args, command...)...) }
	if _, err := run("start", "--wait", "--wait-timeout", "180"); err != nil {
		return 0, fmt.Errorf("restart archived stack: %w", err)
	}
	output, err := run("port", service, target)
	if err != nil {
		return 0, fmt.Errorf("read restored public port: %w", err)
	}
	port, err := publishedPort(output)
	if err != nil {
		return 0, err
	}
	if err := r.probe(ctx, port, cfg.HealthPath); err != nil {
		return 0, errStartupHealth
	}
	return port, nil
}

// Project only the fields Compose needs to start an existing stack. The image
// placeholder satisfies Compose validation. The start command never creates,
// builds or pulls an image.
func restoreManifest(workspace string) (filename, service, target string, err error) {
	type serviceSpec struct {
		Image     string   `yaml:"image"`
		DependsOn any      `yaml:"depends_on,omitempty"`
		Ports     []string `yaml:"ports,omitempty"`
	}
	var model struct {
		Services map[string]*serviceSpec `yaml:"services"`
	}
	data, err := os.ReadFile(filepath.Join(workspace, "preview-compose.yaml"))
	if err != nil {
		return "", "", "", err
	}
	if err := yaml.Unmarshal(data, &model); err != nil {
		return "", "", "", err
	}
	for name, spec := range model.Services {
		if spec == nil {
			return "", "", "", errors.New("invalid saved Compose service")
		}
		spec.Image = "unused"
		if len(spec.Ports) == 0 {
			continue
		}
		if len(spec.Ports) != 1 || service != "" {
			return "", "", "", errors.New("ambiguous saved public service")
		}
		port, ok := strings.CutPrefix(spec.Ports[0], "127.0.0.1::")
		n, err := strconv.Atoi(port)
		if !ok || err != nil || n < 1 || n > 65535 {
			return "", "", "", errors.New("invalid saved public port")
		}
		service, target = name, port
	}
	if service == "" {
		return "", "", "", errors.New("missing saved public service")
	}
	data, err = yaml.Marshal(model)
	if err != nil {
		return "", "", "", err
	}
	f, err := os.CreateTemp(workspace, "restore-*.yaml")
	if err != nil {
		return "", "", "", err
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		os.Remove(f.Name())
		return "", "", "", err
	}
	return f.Name(), service, target, nil
}

func projectNameForAttempt(attempt state.Attempt) string {
	return "pv" + strings.TrimPrefix(attempt.ID, "deployment-")
}
