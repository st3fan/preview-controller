package state

import (
	"errors"
	"strconv"
	"testing"
)

func TestDiagnosticRetentionAndDeletion(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	env := Environment{ID: ID("demo", 47), Project: "demo", Intent: 47, Diagnostics: make(map[string]Diagnostic)}
	for i := range 11 {
		id := strconv.Itoa(i)
		env.Attempts = append(env.Attempts, Attempt{ID: id, Status: Failed})
		env.Diagnostics[id] = Diagnostic{Stage: BuildStage, Output: "excerpt " + id}
	}
	env.Attempts = append(env.Attempts, Attempt{ID: "healthy", Status: Healthy})
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(env.Project, env.Intent)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Attempts) != 12 || len(loaded.Diagnostics) != 10 ||
		loaded.Diagnostics["0"].Output != "" || loaded.Diagnostics["10"].Output != "excerpt 10" {
		t.Fatalf("unexpected retained diagnostics: %+v", loaded.Diagnostics)
	}
	if err := reopened.Delete(env.Project, env.Intent); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(env.Project, env.Intent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("environment survived deletion: %v", err)
	}
}
