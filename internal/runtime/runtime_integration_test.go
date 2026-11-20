//go:build linux && integration

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

// Run only on a disposable Linux Docker host. The fixture pulls public images.
func TestComposeCandidateLifecycle(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	repo, err := git.PlainInit(source, false)
	if err != nil {
		t.Fatal(err)
	}
	gitCfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	gitCfg.Commit.GpgSign = gitconfig.NewOptBool(false)
	if err := repo.SetConfig(gitCfg); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Dockerfile":           "FROM python:3.12-alpine\nCMD [\"python\", \"-m\", \"http.server\", \"8080\"]\n",
		"compose.preview.yaml": "services:\n  web:\n    build: .\n    restart: always\n    stop_grace_period: 1s\n    depends_on:\n      db:\n        condition: service_healthy\n  db:\n    image: redis:7-alpine\n    volumes: [redis-data:/data]\n    healthcheck:\n      test: [CMD, redis-cli, ping]\n      interval: 1s\nvolumes:\n  redis-data: {}\n",
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Attempt{ID: "deployment-0123456789abcdef0123456789abcdef", Commit: hash.String()}
	project := config.Project{Repository: source, ComposeFile: "compose.preview.yaml", PublicService: "web", PublicPort: 8080, HealthPath: "/"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Minute)
		defer done()
		if err := runner.Cleanup(cleanup, "fixture", 1, attempt); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	result, err := runner.Run(ctx, "fixture", 1, project, attempt, func(state.Status, Result) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Port < 1 || result.ComposeProject == "" {
		t.Fatalf("invalid candidate result: %+v", result)
	}
	attempt.Port = result.Port
	alive, err := runner.Check(ctx, attempt, project)
	if err != nil || !alive {
		t.Fatalf("healthy candidate check = %v, %v", alive, err)
	}
	redis := func(command ...string) (string, error) {
		return runner.docker(ctx, 1024, append([]string{"exec", result.ComposeProject + "-db-1", "redis-cli"}, command...)...)
	}
	if output, err := redis("SET", "archive-test", "saved"); err != nil || strings.TrimSpace(output) != "OK" {
		t.Fatalf("write preview data = %q, %v", output, err)
	}
	if err := runner.Stop(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if alive, err := runner.Check(ctx, attempt, project); err != nil || alive {
		t.Fatalf("archived stack check = %v, %v", alive, err)
	}
	port, err := runner.Restore(ctx, "fixture", 1, project, attempt)
	if err != nil {
		t.Fatal(err)
	}
	attempt.Port = port
	if alive, err := runner.Check(ctx, attempt, project); err != nil || !alive {
		t.Fatalf("restored stack check = %v, %v", alive, err)
	}
	if value, err := redis("GET", "archive-test"); err != nil || strings.TrimSpace(value) != "saved" {
		t.Fatalf("restored preview data = %q, %v", value, err)
	}
	image := result.ComposeProject + "-web:latest"
	label, err := runner.docker(ctx, 1024, "image", "inspect", "--format", "{{index .Config.Labels \"com.docker.compose.project\"}}", image)
	if err != nil || strings.TrimSpace(label) != result.ComposeProject {
		t.Fatalf("build image ownership = %q, %v", label, err)
	}
	if err := runner.Cleanup(ctx, "fixture", 1, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.docker(ctx, 1024, "image", "inspect", image); err == nil {
		t.Fatal("preview build image survived cleanup")
	}

}

// This test renders Compose configuration without starting containers.
func TestComposeInterpolationAndPortReplacement(t *testing.T) {
	checkout := t.TempDir()
	composeDir := filepath.Join(checkout, "nested")
	if err := os.Mkdir(composeDir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(composeDir, "compose.yaml")
	for name, content := range map[string]string{
		"Dockerfile": "FROM scratch\n",
		".env":       "IGNORED=from-checkout\n",
		"compose.yaml": `services:
  web:
    build: .
    command: ["app", "${PREVIEW_PORT:?missing}"]
    environment:
      CONFIGURED: ${VALUE:?missing}
      SECRET: ${SECRET:?missing}
      IGNORED: ${IGNORED:-fallback}
    ports: ["${HOST_PORT:?unneeded}:8080"]
  db:
    image: alpine
    ports: ["5432:5432"]
`,
	} {
		if err := os.WriteFile(filepath.Join(composeDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	runner, err := New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_COMPOSE_SECRET", "quote'\"\\\nCOMPOSE_FILE=ignored\\")
	t.Setenv("IGNORED", "from-controller-environment")
	project := config.Project{
		PublicService:  "web",
		ComposeEnv:     map[string]string{"VALUE": "operator value"},
		ComposeEnvFrom: map[string]string{"SECRET": "TEST_COMPOSE_SECRET"},
	}
	port := project.ContainerPort()
	prepared, err := prepareCompose(source, checkout, project.PublicService, port)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, "preview-compose.yaml")
	if err := os.WriteFile(path, prepared, 0600); err != nil {
		t.Fatal(err)
	}
	envFile, err := writeComposeEnv(workspace, project, port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := runner.docker(ctx, 2<<20, "compose", "--project-directory", composeDir, "--env-file", envFile,
		"-p", "preview-render-test", "-f", path, "config", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResolved([]byte(output), project.PublicService, port, checkout); err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Services map[string]struct {
			Build       struct{ Context string }
			Command     []string
			Environment map[string]string
		}
	}
	if err := json.Unmarshal([]byte(output), &rendered); err != nil {
		t.Fatal(err)
	}
	web := rendered.Services["web"]
	if web.Build.Context != composeDir || !slices.Equal(web.Command, []string{"app", "8080"}) ||
		web.Environment["CONFIGURED"] != "operator value" || web.Environment["IGNORED"] != "fallback" ||
		web.Environment["SECRET"] != os.Getenv("TEST_COMPOSE_SECRET") {
		t.Fatal("unexpected resolved relative path, command, or interpolation values")
	}
}
