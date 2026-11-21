package runtime

import (
	"context"
	"fmt"
	"io"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

// A deployment backend manages the resources of one checked-out attempt.
// The controller owns persisted state, readiness probes and route activation.
type deploymentBackend interface {
	launch(context.Context, state.Attempt, config.Project, Result, Progress) (Result, error)
	running(context.Context, state.Attempt) (bool, error)
	remove(context.Context, state.Attempt) error
	stop(context.Context, state.Attempt) error
	restore(context.Context, string, int64, config.Project, state.Attempt) (int, error)
	logs(context.Context, state.Attempt) (func(io.Writer) error, error)
}

type composeBackend struct{ *Runtime }
type dockerBackend struct{ *Runtime }

func (r *Runtime) backend(a state.Attempt) (deploymentBackend, error) {
	if !attemptID.MatchString(a.ID) {
		return nil, fmt.Errorf("invalid deployment identity")
	}
	switch a.Backend {
	case "", config.BackendCompose:
		return composeBackend{r}, nil
	case config.BackendDocker:
		return dockerBackend{r}, nil
	default:
		return nil, fmt.Errorf("unknown deployment backend %q", a.Backend)
	}
}

func (r *Runtime) Stop(ctx context.Context, a state.Attempt) error {
	b, err := r.backend(a)
	if err != nil {
		return err
	}
	return b.stop(ctx, a)
}

func (r *Runtime) Restore(ctx context.Context, project string, intent int64, cfg config.Project, a state.Attempt) (int, error) {
	b, err := r.backend(a)
	if err != nil {
		return 0, err
	}
	return b.restore(ctx, project, intent, cfg, a)
}

func (r *Runtime) OpenLogs(ctx context.Context, a state.Attempt) (func(io.Writer) error, error) {
	b, err := r.backend(a)
	if err != nil {
		return nil, ErrLogsUnavailable
	}
	return b.logs(ctx, a)
}
