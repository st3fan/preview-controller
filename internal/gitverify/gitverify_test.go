package gitverify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestVerifyCommitObject(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	repo, err := git.PlainInit(source, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commit.GpgSign = gitconfig.NewOptBool(false)
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	verifier := Verifier{Timeout: 10 * time.Second}
	if err := verifier.Verify(context.Background(), source, hash.String()); err != nil {
		t.Fatalf("known commit: %v", err)
	}
	if err := verifier.Verify(context.Background(), source, strings.Repeat("0", 40)); !errors.Is(err, ErrCommitNotFound) {
		t.Fatalf("missing commit: %v", err)
	}
}
