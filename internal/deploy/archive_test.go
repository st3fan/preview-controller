package deploy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

type retentionRunner struct {
	fakeRunner
	restoreErr error
	stopErr    error
	stopped    int
}

func (r *retentionRunner) Stop(context.Context, state.Attempt) error {
	r.stopped++
	return r.stopErr
}

func (r *retentionRunner) Restore(context.Context, string, int64, config.Project, state.Attempt) (int, error) {
	return 5678, r.restoreErr
}

func TestArchiveRestoreAndExpire(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := state.New(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(root)
	cfg.ArchiveAfterIdle = "48h"
	project := cfg.Projects["myproject"]
	project.DeleteAfterArchive = "72h"
	cfg.Projects["myproject"] = project
	id := state.ID("myproject", 45)
	attempt := state.Attempt{ID: "deployment-0123456789abcdef0123456789abcdef", Status: state.Healthy, Port: 1234}
	deadline := time.Now().UTC().Add(time.Hour)
	if err := store.Save(state.Environment{ID: id, Project: "myproject", Intent: 45, ActiveDeploymentID: attempt.ID, Attempts: []state.Attempt{attempt}, ArchiveAt: &deadline}); err != nil {
		t.Fatal(err)
	}

	runner := &retentionRunner{}
	s, err := New(cfg, store, fakeVerifier{}, runner.fakeRunner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.lifecycle = runner

	s.sweepLifecycle(deadline.Add(-time.Second))
	if runner.stopped != 0 {
		t.Fatal("preview stopped before its deadline")
	}
	s.running[id] = true
	s.sweepLifecycle(deadline)
	delete(s.running, id)
	if runner.stopped != 0 {
		t.Fatal("preview stopped during a replacement")
	}
	s.sweepLifecycle(deadline)
	archived, err := s.GetEnvironment(id)
	if err != nil || archived.DeleteAt == nil || archived.ArchiveAt != nil || time.Until(*archived.DeleteAt).Round(time.Minute) != 72*time.Hour {
		t.Fatalf("archive state = %+v, %v", archived, err)
	}
	if err := s.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivePort("myproject", 45); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("archived preview route error = %v", err)
	}
	runner.restoreErr = errors.New("unhealthy")
	if err := s.RestoreEnvironment(id); !errors.Is(err, runner.restoreErr) {
		t.Fatalf("restore error = %v", err)
	}
	failed, err := s.GetEnvironment(id)
	if err != nil || failed.LifecycleError == "" || failed.DeleteAt == nil || !failed.DeleteAt.Equal(*archived.DeleteAt) {
		t.Fatalf("failed restore state = %+v, %v", failed, err)
	}
	runner.restoreErr = nil
	if err := s.RestoreEnvironment(id); err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetEnvironment(id)
	if err != nil || restored.ArchiveAt == nil || restored.DeleteAt != nil || time.Until(*restored.ArchiveAt).Round(time.Minute) != 48*time.Hour {
		t.Fatalf("restored state = %+v, %v", restored, err)
	}
	if port, err := s.ActivePort("myproject", 45); err != nil || port != 5678 {
		t.Fatalf("restored route = %d, %v", port, err)
	}
	if err := s.deleteEnvironment(id, archived.DeleteAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveEnvironment(id); err != nil {
		t.Fatal(err)
	}
	archived, err = s.GetEnvironment(id)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepLifecycle(archived.DeleteAt.Add(-time.Second))
	if _, err := s.GetEnvironment(id); err != nil {
		t.Fatal(err)
	}
	s.sweepLifecycle(*archived.DeleteAt)
	if _, err := s.GetEnvironment(id); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expired preview lookup error = %v", err)
	}
}
