package deploy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

type fakeRunner struct {
	err        error
	cleanupErr error
	checkFail  bool
}

func (f fakeRunner) Run(_ context.Context, _ string, _ int64, _ config.Project, _ state.Attempt, progress runtime.Progress) (runtime.Result, error) {
	result := runtime.Result{Port: 12345, Workspace: "/tmp/candidate", ComposeProject: "pvtest"}
	if err := progress(state.Building, result); err != nil {
		return result, err
	}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	return result, f.err
}

func (f fakeRunner) Cleanup(context.Context, string, int64, state.Attempt) error {
	return f.cleanupErr
}
func (f fakeRunner) Check(_ context.Context, attempt state.Attempt, _ config.Project) (bool, error) {
	return !f.checkFail || attempt.Port != 12345, nil
}

func TestFailedCutoverKeepsPreviousTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	old := state.Attempt{ID: "deployment-0123456789abcdef0123456789abcdef", Commit: strings.Repeat("b", 40), Status: state.Healthy, Port: 12344}
	if err := store.Save(state.Environment{ID: state.ID("myproject", 9), Project: "myproject", Intent: 9,
		ActiveDeploymentID: old.ID, ActiveCommit: old.Commit, Attempts: []state.Attempt{old}}); err != nil {
		t.Fatal(err)
	}
	s, err := New(testConfig(root), store, fakeVerifier{}, fakeRunner{checkFail: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _, err = s.Submit("myproject", 9, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env, err := s.GetEnvironment("myproject:9")
		if err != nil {
			t.Fatal(err)
		}
		if len(env.Attempts) == 2 && env.Attempts[1].Status == state.Failed {
			failure := env.Diagnostics[env.Attempts[1].ID]
			if failure.Stage != state.CutoverStage || failure.Output != "" {
				t.Fatalf("unexpected activation diagnostic: %+v", failure)
			}
			port, err := s.ActivePort("myproject", 9)
			if err != nil || port != old.Port || env.ActiveCommit != old.Commit || !strings.Contains(env.Attempts[1].Error, "cutover") {
				t.Fatalf("failed cutover state = %+v, port %d, error %v", env, port, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("candidate did not fail cutover")
}

func TestDeleteRemovesRouteAndSupportsRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	active := state.Attempt{ID: "deployment-0123456789abcdef0123456789abcdef", Commit: strings.Repeat("b", 40), Status: state.Healthy, Port: 12344}
	if err := store.Save(state.Environment{ID: state.ID("myproject", 9), Project: "myproject", Intent: 9,
		ActiveDeploymentID: active.ID, ActiveCommit: active.Commit, Attempts: []state.Attempt{active}}); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	s, err := New(cfg, store, fakeVerifier{}, fakeRunner{cleanupErr: errors.New("Docker unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnvironment("myproject:9"); err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	if _, err := s.ActivePort("myproject", 9); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("route survived deletion: %v", err)
	}
	s.Close()
	retry, err := New(cfg, store, fakeVerifier{}, fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if err := retry.DeleteEnvironment("myproject:9"); err != nil {
		t.Fatal(err)
	}
	if err := retry.DeleteEnvironment("myproject:9"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if _, err := store.Load("myproject", 9); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("state remained after deletion: %v", err)
	}
}

func TestFirstDeploymentAndRestartRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	s, err := New(cfg, store, fakeVerifier{}, fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	attempt, _, err := s.Submit("myproject", 11, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		env, err := s.GetEnvironment("myproject:11")
		if err != nil {
			t.Fatal(err)
		}
		if env.ActiveDeploymentID == attempt.ID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if port, err := s.ActivePort("myproject", 11); err != nil || port != 12345 {
		t.Fatalf("first active port = %d, %v", port, err)
	}
	activated, _, err := s.GetDeployment(attempt.ID)
	if err != nil || activated.ActivatedAt == nil || activated.ActivatedAt.Before(attempt.CreatedAt) {
		t.Fatalf("activation timing = %+v, %v", activated, err)
	}
	s.Close()
	restarted, err := New(cfg, store, fakeVerifier{}, fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if port, err := restarted.ActivePort("myproject", 11); err != nil || port != 12345 {
		t.Fatalf("recovered active port = %d, %v", port, err)
	}
	recovered, _, err := restarted.GetDeployment(attempt.ID)
	if err != nil || recovered.ActivatedAt == nil || !recovered.ActivatedAt.Equal(*activated.ActivatedAt) {
		t.Fatalf("recovered activation timing = %+v, %v", recovered, err)
	}
}

type fakeVerifier struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (f fakeVerifier) Verify(ctx context.Context, _, _ string) error {
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func testConfig(root string) config.Config {
	return config.Config{
		PreviewDomain: "preview.example.com",
		StateRoot:     root,
		Projects: map[string]config.Project{
			"myproject": {Repository: "https://github.com/example/myproject.git"},
		},
	}
}

func TestSubmitIsAsyncAndSerialPerEnvironment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeVerifier{started: make(chan struct{}), release: make(chan struct{})}
	s, err := New(testConfig(root), store, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	commit := strings.Repeat("a", 40)
	attempt, env, err := s.Submit("myproject", 123, commit)
	if err != nil || attempt.Status != state.Queued || env.URL != "https://myproject-123.preview.example.com" {
		t.Fatalf("submit = %+v, %+v, %v", attempt, env, err)
	}
	select {
	case <-fake.started:
	case <-time.After(3 * time.Second):
		t.Fatal("verifier did not start")
	}
	if _, _, err := s.Submit("myproject", 123, commit); !errors.Is(err, ErrConflict) {
		t.Fatalf("second submit error = %v", err)
	}
	close(fake.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := s.GetEnvironment("myproject:123")
		if err != nil {
			t.Fatal(err)
		}
		if current.Attempts[0].Status == state.Verified {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verification did not complete")
}

func TestVerificationFailureAndRestartRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	s, err := New(cfg, store, fakeVerifier{err: errors.New("missing commit")})
	if err != nil {
		t.Fatal(err)
	}
	attempt, _, err := s.Submit("myproject", 5, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := s.GetEnvironment("myproject:5")
		if err != nil {
			t.Fatal(err)
		}
		if current.Attempts[0].Status == state.Failed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.Close()
	failed, _, err := s.GetDeployment(attempt.ID)
	if err != nil || failed.Status != state.Failed || !strings.Contains(failed.Error, "missing commit") {
		t.Fatalf("failure = %+v, %v", failed, err)
	}
	if err := store.Save(state.Environment{
		ID: state.ID("myproject", 6), Project: "myproject", Intent: 6,
		Attempts: []state.Attempt{{ID: "interrupted", Status: state.Fetching}},
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, store, fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	current, err := restarted.GetEnvironment("myproject:6")
	if err != nil || current.Attempts[0].Status != state.Failed || !strings.Contains(current.Attempts[0].Error, "restarted") || current.Attempts[0].ActivatedAt != nil {
		t.Fatalf("recovered state = %+v, %v", current, err)
	}
}

func TestRejectsInvalidRequestWithoutState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(testConfig(root), store, fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Submit("other", 1, strings.Repeat("a", 40)); !errors.Is(err, ErrUnknownProject) {
		t.Fatal(err)
	}
	if _, _, err := s.Submit("myproject", 0, strings.Repeat("a", 40)); !errors.Is(err, ErrInvalidIntent) {
		t.Fatal(err)
	}
	if _, _, err := s.Submit("myproject", 1, "abc"); !errors.Is(err, ErrInvalidCommit) {
		t.Fatal(err)
	}
	envs, err := store.List()
	if err != nil || len(envs) != 0 {
		t.Fatalf("unexpected state = %+v, %v", envs, err)
	}
}

func TestConcurrentSubmissionsToSameEnvironment(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeVerifier{release: make(chan struct{})}
	s, err := New(testConfig(root), store, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Submit("myproject", 42, strings.Repeat("a", 40))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict), errors.Is(err, ErrBusy):
		default:
			t.Fatalf("unexpected submit error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful submissions = %d, want 1", successes)
	}
	close(fake.release)
}

func TestRuntimeCandidateTransitionsAndPreservesActiveOnFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			store, err := state.New(root)
			if err != nil {
				t.Fatal(err)
			}
			previousActivation := time.Now().UTC().Add(-time.Minute)
			previous := state.Environment{ID: state.ID("myproject", 123), Project: "myproject", Intent: 123,
				ActiveDeploymentID: "deployment-previous", ActiveCommit: strings.Repeat("c", 40), Attempts: []state.Attempt{{ID: "deployment-previous", Commit: strings.Repeat("c", 40), Status: state.Healthy, Port: 12344, ActivatedAt: &previousActivation}}}
			if err := store.Save(previous); err != nil {
				t.Fatal(err)
			}
			runner := fakeRunner{}
			if failure {
				runner.err = errors.New("health check failed")
			}
			s, err := New(testConfig(root), store, fakeVerifier{}, runner)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			attempt, _, err := s.Submit("myproject", 123, strings.Repeat("a", 40))
			if err != nil {
				t.Fatal(err)
			}
			if attempt.CreatedAt.IsZero() || attempt.ActivatedAt != nil {
				t.Fatalf("queued candidate timing = %+v", attempt)
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				env, err := s.GetEnvironment("myproject:123")
				if err != nil {
					t.Fatal(err)
				}
				candidate := env.Attempts[len(env.Attempts)-1]
				if candidate.Status == state.Healthy || candidate.Status == state.Failed {
					want := state.Healthy
					if failure {
						want = state.Failed
					}
					active := attempt.ID
					if failure {
						active = previous.ActiveDeploymentID
					}
					if candidate.ID != attempt.ID || candidate.Status != want || candidate.Port != 12345 || env.ActiveDeploymentID != active {
						t.Fatalf("candidate and active state = %+v, %+v", candidate, env)
					}
					if failure && candidate.ActivatedAt != nil {
						t.Fatalf("failed candidate recorded activation: %v", candidate.ActivatedAt)
					}
					if !failure && (candidate.ActivatedAt == nil || candidate.ActivatedAt.Before(attempt.CreatedAt)) {
						t.Fatalf("successful candidate activation = %v", candidate.ActivatedAt)
					}
					s.Close()
					saved, err := store.Load("myproject", 123)
					if err != nil {
						t.Fatal(err)
					}
					old := saved.Attempts[0]
					if old.ActivatedAt == nil || !old.ActivatedAt.Equal(previousActivation) || (!failure && old.Status != state.Retired) {
						t.Fatalf("previous attempt after replacement = %+v", old)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("runtime candidate did not finish")
		})
	}
}

func TestExistingDeploymentUsesProjectFirstURL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Attempt{
		ID: "deployment-0123456789abcdef0123456789abcdef", Commit: strings.Repeat("a", 40),
		Status: state.Healthy, Port: 12344,
	}
	stored := state.Environment{
		ID: state.ID("myproject", 123), Project: "myproject", Intent: 123,
		URL: "https://123-myproject.preview.example.com", ActiveDeploymentID: attempt.ID,
		ActiveCommit: attempt.Commit, Attempts: []state.Attempt{attempt},
	}
	if err := store.Save(stored); err != nil {
		t.Fatal(err)
	}
	s, err := New(testConfig(root), store, fakeVerifier{}, fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const want = "https://myproject-123.preview.example.com"
	env, err := s.GetEnvironment(stored.ID)
	if err != nil || env.URL != want {
		t.Fatalf("environment URL = %q, %v", env.URL, err)
	}
	envs, err := s.ListEnvironments()
	if err != nil || len(envs) != 1 || envs[0].URL != want {
		t.Fatalf("listed environments = %+v, %v", envs, err)
	}
	_, env, err = s.GetDeployment(attempt.ID)
	if err != nil || env.URL != want {
		t.Fatalf("deployment URL = %q, %v", env.URL, err)
	}
	port, err := s.ActivePort(stored.Project, stored.Intent)
	if err != nil || port != attempt.Port {
		t.Fatalf("active port = %d, %v", port, err)
	}
}

func TestPreviewRouteUsesLatestAttemptUntilCutover(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{store: store}
	env := state.Environment{
		ID: state.ID("myproject", 9), Project: "myproject", Intent: 9,
		Attempts: []state.Attempt{
			{ID: "first", Status: state.Failed, Error: "private failure detail"},
			{ID: "retry", Status: state.Building},
		},
	}
	check := func(wantPort int, wantStatus state.Status, wantTitle string) {
		t.Helper()
		if err := store.Save(env); err != nil {
			t.Fatal(err)
		}
		attempt, status, err := s.PreviewDeployment(env.Project, env.Intent)
		port := attempt.Port
		if err != nil || port != wantPort || status != wantStatus {
			t.Fatalf("route = %d, %q, %v; want %d, %q", port, status, err, wantPort, wantStatus)
		}
		if attempt.IssueTitle != wantTitle {
			t.Fatalf("title = %q; want %q", attempt.IssueTitle, wantTitle)
		}
	}
	check(0, state.Building, "")
	env.Attempts[1].Status = state.Failed
	check(0, state.Failed, "")

	env.Attempts[1].Status = state.Healthy
	env.Attempts[1].Port = 12345
	env.ActiveDeploymentID = "retry"
	env.Attempts[1].IssueTitle = "Active ticket"
	check(12345, "", "Active ticket")

	env.Attempts = append(env.Attempts, state.Attempt{ID: "replacement", Status: state.Building, IssueTitle: "Replacement title"})
	check(12345, "", "Active ticket")
	env.Attempts[2].Status = state.Failed
	check(12345, "", "Active ticket")
}
