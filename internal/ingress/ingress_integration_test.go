//go:build linux && integration

package ingress_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/gitverify"
	"github.com/st3fan/preview-controller/internal/ingress"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

type cutoverRunner struct {
	*runtime.Runtime
	failCommit string
}

func (r cutoverRunner) Check(ctx context.Context, attempt state.Attempt, project config.Project) (bool, error) {
	if attempt.Commit == r.failCommit {
		return false, nil
	}
	return r.Runtime.Check(ctx, attempt, project)
}

// Run with -tags=integration -timeout=30m on a disposable Linux Docker host.
func TestFullPreviewLifecycle(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is unavailable")
	}
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
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	fixture := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "preview-fixture", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	compose := fixture("compose.preview.yaml")
	dockerfile := fixture("Dockerfile")
	commit := func(message, page, composeText string) string {
		t.Helper()
		for name, content := range map[string]string{"Dockerfile": dockerfile, "index.html": page, "compose.preview.yaml": composeText} {
			if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := worktree.Add(name); err != nil {
				t.Fatal(err)
			}
		}
		hash, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return hash.String()
	}
	first := commit("first", fixture("index.html"), compose)
	second := commit("second", "second", compose)
	bad := commit("invalid Compose", "bad", compose+"    privileged: true\n")
	cutoverFailure := commit("cutover failure", "third", compose)
	root := t.TempDir()
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	baseRunner, err := runtime.New(filepath.Join(root, "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	runner := cutoverRunner{Runtime: baseRunner, failCommit: cutoverFailure}
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{
		"fixture": {Repository: source, ComposeFile: "compose.preview.yaml", PublicService: "web", PublicPort: 8080, HealthPath: "/"},
	}}
	var service *deploy.Service
	t.Cleanup(func() {
		if service != nil {
			service.Close()
		}
		envs, err := store.List()
		if err != nil {
			t.Logf("list remaining test stacks: %v", err)
			return
		}
		for _, env := range envs {
			for _, attempt := range env.Attempts {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if err := runner.Cleanup(ctx, env.Project, env.Intent, attempt); err != nil {
					t.Logf("cleanup %s: %v", attempt.ID, err)
				}
				cancel()
			}
		}
	})
	service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	submit := func(sha string) state.Attempt {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			attempt, _, err := service.Submit("fixture", 123, sha)
			if err == nil {
				return attempt
			}
			if !errors.Is(err, deploy.ErrConflict) || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait := func(id string, want state.Status) state.Environment {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for time.Now().Before(deadline) {
			env, err := service.GetEnvironment("fixture:123")
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range env.Attempts {
				if attempt.ID == id && (attempt.Status == state.Healthy || attempt.Status == state.Failed) {
					if attempt.Status != want {
						t.Fatalf("attempt %s: got %s, want %s: %s", id, attempt.Status, want, attempt.Error)
					}
					return env
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatal("deployment did not finish")
		return state.Environment{}
	}
	preview := func(want string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "http://fixture-123.preview.example.com/", nil)
		w := httptest.NewRecorder()
		ingress.Handler(cfg, service).ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("preview = %d %q, want %q", w.Code, w.Body.String(), want)
		}
	}
	firstAttempt := submit(first)
	if env := wait(firstAttempt.ID, state.Healthy); env.ActiveCommit != first {
		t.Fatalf("first active commit: %s", env.ActiveCommit)
	}
	preview("first")
	logRequest := httptest.NewRequest(http.MethodGet, "http://fixture-123.preview.example.com/dashboard/logs/fixture/123/"+firstAttempt.ID, nil)
	logResponse := httptest.NewRecorder()
	ingress.Handler(cfg, service).ServeHTTP(logResponse, logRequest)
	if logResponse.Code != http.StatusNotFound {
		t.Fatalf("preview hostname log path returned %d", logResponse.Code)
	}
	secondAttempt := submit(second)
	if env := wait(secondAttempt.ID, state.Healthy); env.ActiveCommit != second {
		t.Fatalf("replacement active commit: %s", env.ActiveCommit)
	}
	preview("second")
	service.Close()
	service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	preview("second")
	badAttempt := submit(bad)
	if env := wait(badAttempt.ID, state.Failed); env.ActiveCommit != second {
		t.Fatalf("failed candidate changed active commit: %s", env.ActiveCommit)
	}
	preview("second")
	cutoverAttempt := submit(cutoverFailure)
	if env := wait(cutoverAttempt.ID, state.Failed); env.ActiveCommit != second {
		t.Fatalf("failed cutover changed active commit: %s", env.ActiveCommit)
	}
	preview("second")
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := service.DeleteEnvironment("fixture:123")
		if err == nil {
			break
		}
		if !errors.Is(err, deploy.ErrConflict) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r := httptest.NewRequest(http.MethodGet, "http://fixture-123.preview.example.com/", nil)
	w := httptest.NewRecorder()
	ingress.Handler(cfg, service).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("deleted preview status = %d", w.Code)
	}
}
