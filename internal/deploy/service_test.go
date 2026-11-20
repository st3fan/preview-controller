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
	"github.com/st3fan/preview-controller/internal/state"
)

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
	if err != nil || attempt.Status != state.Queued || env.URL != "https://123-myproject.preview.example.com" {
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
	if err != nil || current.Attempts[0].Status != state.Failed || !strings.Contains(current.Attempts[0].Error, "restarted") {
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
