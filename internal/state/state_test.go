package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAtomicSaveLoadAndRecovery(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	env := Environment{ID: ID("myproject", 123), Project: "myproject", Intent: 123, URL: "https://123-myproject.preview.example.com", UpdatedAt: time.Now().UTC()}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	env.Attempts = append(env.Attempts, Attempt{ID: "deployment-1", Commit: "abc", Status: Queued})
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("myproject", 123)
	if err != nil || len(loaded.Attempts) != 1 {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	name := filepath.Join(store.root, "myproject", "123.json")
	info, err := os.Stat(name)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file mode = %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "myproject", ".state-interrupted.tmp"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.root)
	if err != nil {
		t.Fatal(err)
	}
	envs, err := reopened.List()
	if err != nil || len(envs) != 1 || envs[0].Attempts[0].ID != "deployment-1" {
		t.Fatalf("list = %+v, %v", envs, err)
	}
}

func TestStoreRejectsSymlinkAndTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Environment{ID: ID("../bad", 1), Project: "../bad", Intent: 1}); err == nil {
		t.Fatal("traversal was accepted")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Environment{ID: ID("linked", 1), Project: "linked", Intent: 1}); err == nil {
		t.Fatal("symlink project directory was accepted")
	}
	if _, err := store.Load("missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing state error = %v", err)
	}
}
