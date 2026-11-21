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
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/ingress"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

type cutoverRunner struct {
	*runtime.Runtime
	failCommit string
}

func (r cutoverRunner) HealthyPort(ctx context.Context, attempt state.Attempt, project config.Project) (int, error) {
	if attempt.Commit == r.failCommit {
		return 0, nil
	}
	return r.Runtime.HealthyPort(ctx, attempt, project)
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
	var neighbor state.Attempt
	firstAttempt := submit(123, first)
	if backend == config.BackendDocker {
		neighbor = submit(124, second)
		wait(124, neighbor.ID, state.Healthy)
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
	var stale state.Environment
	var correctPort int
	if backend == config.BackendDocker {
		stale, err = store.Load("fixture", 123)
		if err != nil {
			t.Fatal(err)
		}
		neighborEnv, err := store.Load("fixture", 124)
		if err != nil {
			t.Fatal(err)
		}
		other, ok := neighborEnv.ActivePreview()
		if !ok {
			t.Fatal("missing neighbor")
		}
		for i := range stale.Attempts {
			a := &stale.Attempts[i]
			if a.ID == secondAttempt.ID {
				correctPort = a.Port
				a.Port = other.Port
			}
		}
		if correctPort == 0 || correctPort == other.Port {
			t.Fatal("expected two distinct published ports")
		}
		if err := store.Save(stale); err != nil {
			t.Fatal(err)
		}
	}
	service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	preview(123, "second")
	if backend == config.BackendDocker {
		recovered, err := store.Load("fixture", 123)
		if err != nil {
			t.Fatal(err)
		}
		active, ok := recovered.ActivePreview()
		if !ok || active.ID != secondAttempt.ID || active.Port != correctPort {
			t.Fatalf("reconciled route = %+v, want %s at %d", active, secondAttempt.ID, correctPort)
		}
		checkPeriodicHealth(t, store, cfg, baseRunner, recovered, stale)
	}
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
	if backend == config.BackendDocker {
		// The ambiguous image fails after build; the rejected cutover fails after
		// both its container and network exist.
		verifyDockerCleanup(t, runner.Runtime, 123, badAttempt)
		verifyDockerCleanup(t, runner.Runtime, 123, cutoverAttempt)
		preview(124, "second")
	}
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
		service.Close()
		// Reconfigure the project, then restore/expire attempts created under its
		// previous settings. The override selects 8080 from the ambiguous image.
		project := cfg.Projects["fixture"]
		project.PublicPort = 8080
		cfg.Projects["fixture"] = project
		service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
		if err != nil {
			t.Fatal(err)
		}
		overridden := submit(123, bad)
		wait(123, overridden.ID, state.Healthy)
		preview(123, "bad")
		if err := service.ArchiveEnvironment("fixture:124"); err != nil {
			t.Fatal(err)
		}
		service.Close()
		env, err := store.Load("fixture", 124)
		if err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Minute)
		env.DeleteAt = &past
		if err := store.Save(env); err != nil {
			t.Fatal(err)
		}
		cfg.Projects["fixture"] = config.Project{Repository: source, ComposeFile: "compose.preview.yaml", PublicService: "web", HealthPath: "/"}
		service, err = deploy.New(cfg, store, gitverify.Verifier{}, runner)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Minute)
		for {
			_, err = store.Load("fixture", 124)
			if errors.Is(err, state.ErrNotFound) {
				break
			}
			if err != nil || time.Now().After(deadline) {
				t.Fatalf("archived Docker preview did not expire: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		verifyDockerCleanup(t, runner.Runtime, 124, neighbor)
		preview(123, "bad")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("deleted preview status = %d", w.Code)
	}
}

func verifyDockerCleanup(t *testing.T, runner *runtime.Runtime, intent int64, a state.Attempt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	assertGone := func() {
		t.Helper()
		for _, list := range [][]string{{"container", "ls", "-aq"}, {"network", "ls", "-q"}, {"image", "ls", "-aq"}} {
			out, err := exec.CommandContext(ctx, "docker", append(list, "--filter", "label=preview-controller.attempt="+a.ID)...).CombinedOutput()
			if err != nil || len(bytes.TrimSpace(out)) != 0 {
				t.Fatalf("%v after cleanup of %s: %s (%v)", list, a.ID, out, err)
			}
		}
	}
	assertGone()
	if err := runner.Cleanup(ctx, "fixture", intent, a); err != nil {
		t.Fatal(err)
	}
	assertGone()
}

func checkPeriodicHealth(t *testing.T, store *state.Store, cfg config.Config, r *runtime.Runtime, recovered, stale state.Environment) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	monitor := health.New(store, cfg.Projects, r.ProbeHealth)
	done := make(chan struct{})
	go func() { defer close(done); monitor.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var first time.Time
	for {
		changed := monitor.Changes()
		sample := monitor.Lookup(recovered)
		if sample.Status == "Healthy" {
			if monitor.Lookup(stale).Status != "Unknown" {
				t.Fatal("monitor reused the observation for the stale route")
			}
			if !first.IsZero() && sample.LastSuccess.After(first) {
				return
			}
			first = sample.LastSuccess
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("periodic monitor did not confirm the recovered route twice: %+v", sample)
		}
	}
}
