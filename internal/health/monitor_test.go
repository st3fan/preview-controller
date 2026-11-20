package health

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

func TestHealthTransitions(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := savePreview(t, store, 1)
	projects := map[string]config.Project{"demo": {HealthPath: "/healthz"}}
	var failure error
	monitor := New(store, projects, func(_ context.Context, port int, path string) error {
		if port != 8080 || path != "/healthz" {
			t.Errorf("unexpected probe target %d %q", port, path)
		}
		return failure
	})
	if got := monitor.Lookup(env); got.Status != "Unknown" || !got.LastSuccess.IsZero() {
		t.Fatalf("initial health = %+v", got)
	}
	monitor.poll(t.Context())
	healthy := monitor.Lookup(env)
	if healthy.Status != "Healthy" || healthy.LastSuccess.IsZero() {
		t.Fatalf("first success = %+v", healthy)
	}
	failure = errors.New("HTTP 503")
	monitor.poll(t.Context())
	if got := monitor.Lookup(env); got.Status != "Healthy" || got.Failure != "HTTP 503" || got.LastSuccess != healthy.LastSuccess {
		t.Fatalf("first failure = %+v", got)
	}
	monitor.poll(t.Context())
	if got := monitor.Lookup(env); got.Status != "Unhealthy" || got.Failure != "HTTP 503" || got.LastSuccess != healthy.LastSuccess {
		t.Fatalf("second failure = %+v", got)
	}
	failure = nil
	monitor.poll(t.Context())
	if got := monitor.Lookup(env); got.Status != "Healthy" || got.Failure != "" || got.LastSuccess.Before(healthy.LastSuccess) {
		t.Fatalf("recovery = %+v", got)
	}
	failure = errors.New("HTTP 500")
	monitor.poll(t.Context())
	if got := monitor.Lookup(env); got.Status != "Healthy" || got.Failure != "HTTP 500" {
		t.Fatalf("first failure after recovery = %+v", got)
	}
	if got := New(store, projects, monitor.probe).Lookup(env); got.Status != "Unknown" {
		t.Fatalf("health after restart = %+v", got)
	}
}

func TestDiscardStaleSamples(t *testing.T) {
	for _, action := range []string{"replace", "archive", "restore", "delete"} {
		t.Run(action, func(t *testing.T) {
			store, err := state.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			env := savePreview(t, store, 1)
			monitor := New(store, map[string]config.Project{"demo": {}}, nil)
			key := activeTarget(env)
			monitor.record(env, key, nil)
			before := monitor.Lookup(env)
			want := "Inactive"
			switch action {
			case "replace":
				env.ActiveDeploymentID = "deployment-next"
				env.Attempts[0].ID = env.ActiveDeploymentID
				want = "Unknown"
			case "archive":
				now := time.Now().UTC()
				env.DeleteAt = &now
			case "restore":
				env.Attempts[0].UpdatedAt = time.Now().UTC()
				want = "Unknown"
			case "delete":
				if err := store.Delete(env.Project, env.Intent); err != nil {
					t.Fatal(err)
				}
				env.ActiveDeploymentID = ""
			}
			if action != "delete" {
				if err := store.Save(env); err != nil {
					t.Fatal(err)
				}
			}
			changes := monitor.Changes()
			monitor.record(env, key, errors.New("late failure"))
			select {
			case <-changes:
				t.Fatal("late sample published a change")
			default:
			}
			if got := monitor.Lookup(env); got.Status != want || got.Failure != "" || !got.LastSuccess.IsZero() {
				t.Fatalf("current health = %+v", got)
			}
			if got := monitor.samples[env.ID].observation; got != before {
				t.Fatalf("late sample changed observation to %+v", got)
			}
		})
	}
}

func TestProbeConcurrencyAndCancellation(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for intent := int64(1); intent <= 8; intent++ {
		savePreview(t, store, intent)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	started := make(chan struct{}, 8)
	monitor := New(store, map[string]config.Project{"demo": {}}, func(ctx context.Context, _ int, _ string) error {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 3*time.Second {
			t.Errorf("unexpected check deadline %v", deadline)
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitor.Run(ctx)
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("four probes did not start concurrently")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after cancellation")
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("started %d probes, want 4", got)
	}
}

func savePreview(t *testing.T, store *state.Store, intent int64) state.Environment {
	t.Helper()
	env := state.Environment{
		ID: state.ID("demo", intent), Project: "demo", Intent: intent,
		ActiveDeploymentID: "deployment-active",
		Attempts:           []state.Attempt{{ID: "deployment-active", Status: state.Healthy, Port: 8080}},
	}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	return env
}
