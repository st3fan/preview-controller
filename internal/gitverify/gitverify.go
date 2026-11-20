package gitverify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

var ErrCommitNotFound = errors.New("commit is not present in the configured repository")

// Verifier uses go-git to confirm an exact commit in a public repository.
// The temporary bare clone is discarded; the runtime slice will create its
// own immutable checkout after verification.
type Verifier struct {
	Timeout time.Duration
}

func (v Verifier) Verify(ctx context.Context, repository, sha string) error {
	if len(sha) != 40 {
		return ErrCommitNotFound
	}
	hash, ok := plumbing.FromHex(sha)
	if !ok {
		return ErrCommitNotFound
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "preview-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	repo, err := git.PlainCloneContext(ctx, dir, &git.CloneOptions{
		URL:          repository,
		Bare:         true,
		NoCheckout:   true,
		SingleBranch: false,
		Tags:         git.AllTags,
	})
	if err != nil {
		return fmt.Errorf("clone public repository: %w", err)
	}
	if _, err := repo.CommitObject(hash); err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return ErrCommitNotFound
		}
		return fmt.Errorf("read commit: %w", err)
	}
	return nil
}
