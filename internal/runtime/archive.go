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
	source := filepath.Join(workspace, "source", cfg.ComposeFile)
	targetPort := cfg.ContainerPort()
	envFile, err := writeComposeEnv(workspace, cfg, targetPort)
	if err != nil {
		return 0, err
	}
	defer os.Remove(envFile)
	args := []string{"compose", "--project-directory", filepath.Dir(source), "--env-file", envFile, "-p", projectNameForAttempt(attempt), "-f", filepath.Join(workspace, "preview-compose.yaml")}
	run := func(command ...string) (string, error) {
		return r.runDocker(ctx, 2<<20, len(cfg.ComposeEnv) > 0 || len(cfg.ComposeEnvFrom) > 0, append(args, command...)...)
	}
	if _, err := run("start", "--wait", "--wait-timeout", "180"); err != nil {
		return 0, fmt.Errorf("restart archived stack: %w", err)
	}
	output, err := run("port", cfg.PublicService, strconv.Itoa(targetPort))
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

func projectNameForAttempt(attempt state.Attempt) string {
	return "pv" + strings.TrimPrefix(attempt.ID, "deployment-")
}
