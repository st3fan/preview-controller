//go:build linux && integration

package runtime

import (
	"context"
	"os"
	"path/filepath"
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
		"compose.preview.yaml": "services:\n  web:\n    build: .\n    depends_on:\n      db:\n        condition: service_healthy\n  db:\n    image: redis:7-alpine\n    healthcheck:\n      test: [CMD, redis-cli, ping]\n",
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
}
