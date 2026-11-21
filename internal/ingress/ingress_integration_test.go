//go:build linux && integration

package ingress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	for _, backend := range []string{config.BackendCompose, config.BackendDocker} {
		t.Run(backend, func(t *testing.T) { testFullPreviewLifecycle(t, backend) })
	}
}

func testFullPreviewLifecycle(t *testing.T, backend string) {
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
		files := map[string]string{"Dockerfile": dockerfile, "index.html": page}
		if backend == config.BackendCompose {
			files["compose.preview.yaml"] = composeText
		} else if message == "invalid candidate" {
			files["Dockerfile"] += "EXPOSE 9000/tcp\n"
		}
		for name, content := range files {
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
	bad := commit("invalid candidate", "bad", compose+"    privileged: true\n")
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
	if backend == config.BackendDocker {
		cfg.Projects["fixture"] = config.Project{Backend: backend, Repository: source, HealthPath: "/"}
	}
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
	submit := func(intent int64, sha string) state.Attempt {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			attempt, _, err := service.Submit("fixture", intent, sha)
			if err == nil {
				return attempt
			}
			if !errors.Is(err, deploy.ErrConflict) || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait := func(intent int64, id string, want state.Status) state.Environment {
		t.Helper()
		deadline := time.Now().Add(8 * time.Minute)
		for time.Now().Before(deadline) {
			env, err := service.GetEnvironment(state.ID("fixture", intent))
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
	preview := func(intent int64, want string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://fixture-%d.preview.example.com/", intent), nil)
		w := httptest.NewRecorder()
		ingress.Handler(cfg, service).ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != want {
			t.Fatalf("preview = %d %q, want %q", w.Code, w.Body.String(), want)
		}
	}
	firstAttempt := submit(123, first)
	if backend == config.BackendDocker {
		other := submit(124, second)
		wait(124, other.ID, state.Healthy)
	}
	if env := wait(123, firstAttempt.ID, state.Healthy); env.ActiveCommit != first {
		t.Fatalf("first active commit: %s", env.ActiveCommit)
	}
	preview(123, "first")
	if backend == config.BackendDocker {
		preview(124, "second")
		firstPort, err := service.ActivePort("fixture", 123)
		if err != nil {
			t.Fatal(err)
		}
		otherPort, err := service.ActivePort("fixture", 124)
		if err != nil || otherPort == firstPort {
			t.Fatalf("concurrent mappings: %d, %d, %v", firstPort, otherPort, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		follow, err := runner.OpenLogs(ctx, firstAttempt)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		_ = follow(&output)
		if !strings.Contains(output.String(), "GET /") {
			t.Fatalf("expected HTTP access log, got %q", output.String())
		}
		if err := service.ArchiveEnvironment("fixture:123"); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ActivePort("fixture", 123); err == nil {
			t.Fatal("archived preview still has a route")
		}
		if err := service.RestoreEnvironment("fixture:123"); err != nil {
			t.Fatal(err)
		}
		preview(123, "first")
	}
	logRequest := httptest.NewRequest(http.MethodGet, "http://fixture-123.preview.example.com/dashboard/logs/fixture/123/"+firstAttempt.ID, nil)
	logResponse := httptest.NewRecorder()
	ingress.Handler(cfg, service).ServeHTTP(logResponse, logRequest)
	if logResponse.Code != http.StatusNotFound {
		t.Fatalf("preview hostname log path returned %d", logResponse.Code)
	}
	secondAttempt := submit(123, second)
	if env := wait(123, secondAttempt.ID, state.Healthy); env.ActiveCommit != second {
		t.Fatalf("replacement active commit: %s", env.ActiveCommit)
	}
	preview(123, "second")
	service.Close()
	service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	preview(123, "second")
	badAttempt := submit(123, bad)
	if env := wait(123, badAttempt.ID, state.Failed); env.ActiveCommit != second {
		t.Fatalf("failed candidate changed active commit: %s", env.ActiveCommit)
	}
	preview(123, "second")
	cutoverAttempt := submit(123, cutoverFailure)
	if env := wait(123, cutoverAttempt.ID, state.Failed); env.ActiveCommit != second {
		t.Fatalf("failed cutover changed active commit: %s", env.ActiveCommit)
	}
	preview(123, "second")
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
	if backend == config.BackendDocker {
		preview(124, "second")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("deleted preview status = %d", w.Code)
	}
}
