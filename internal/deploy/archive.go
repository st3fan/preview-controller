package deploy

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

const (
	archiving = "archiving"
	restoring = "restoring"
)

type lifecycleRunner interface {
	Stop(context.Context, state.Attempt) error
	Restore(context.Context, string, int64, config.Project, state.Attempt) (int, error)
}

func resetArchiveDeadline(env *state.Environment, cfg config.Config, now time.Time) {
	idle, _ := cfg.Lifecycle(env.Project)
	deadline := now.Add(idle)
	env.ArchiveAt = &deadline
	env.DeleteAt = nil
	env.LifecycleError = ""
	env.UpdatedAt = now
}

func (s *Service) ArchiveEnvironment(id string) error {
	return s.changeLifecycle(id, archiving, nil)
}

func (s *Service) RestoreEnvironment(id string) error {
	return s.changeLifecycle(id, restoring, nil)
}

func (s *Service) claimLifecycle(id, operation string, due *time.Time) (state.Environment, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.lifecycle == nil {
		return state.Environment{}, 0, ErrClosed
	}
	env, err := s.GetEnvironment(id)
	if err != nil {
		return state.Environment{}, 0, err
	}
	if s.running[env.ID] || s.deleting[env.ID] || env.Deleting {
		return state.Environment{}, 0, ErrConflict
	}
	for i, attempt := range env.Attempts {
		if attempt.ID != env.ActiveDeploymentID || attempt.Status != state.Healthy {
			continue
		}
		switch operation {
		case archiving:
			if env.DeleteAt != nil || env.LifecycleOperation == restoring ||
				(due != nil && env.LifecycleOperation != archiving && (env.ArchiveAt == nil || env.ArchiveAt.After(*due))) {
				return state.Environment{}, 0, ErrConflict
			}
		case restoring:
			if env.DeleteAt == nil || (due != nil && env.LifecycleOperation != restoring) {
				return state.Environment{}, 0, ErrConflict
			}
		}
		env.LifecycleOperation = operation
		if err := s.store.Save(env); err != nil {
			return state.Environment{}, 0, err
		}
		s.running[env.ID] = true
		s.wg.Add(1)
		return env, i, nil
	}
	return state.Environment{}, 0, ErrConflict
}

func (s *Service) changeLifecycle(id, operation string, due *time.Time) error {
	env, active, err := s.claimLifecycle(id, operation, due)
	if err != nil {
		return err
	}
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.running, env.ID)
		s.mu.Unlock()
	}()

	attempt := &env.Attempts[active]
	if operation == archiving {
		stop, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		for _, item := range env.Attempts {
			if item.Status != state.Retired {
				err = errors.Join(err, s.lifecycle.Stop(stop, item))
			}
		}
		cancel()
		if err == nil {
			env.LifecycleError = ""
			_, retention := s.cfg.Lifecycle(env.Project)
			deadline := time.Now().UTC().Add(retention)
			env.DeleteAt = &deadline
			env.ArchiveAt = nil
			env.LifecycleOperation = ""
		}
	} else {
		// A restore interrupted by shutdown returns to its archived state.
		if due == nil {
			start, cancel := context.WithTimeout(s.ctx, 4*time.Minute)
			var port int
			port, err = s.lifecycle.Restore(start, env.Project, env.Intent, s.cfg.Projects[env.Project], *attempt)
			cancel()
			if err == nil {
				attempt.Port = port
				resetArchiveDeadline(&env, s.cfg, time.Now().UTC())
				env.LifecycleOperation = ""
			}
		} else {
			err = errors.New("restore interrupted by controller restart")
		}
		if err != nil {
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			stopErr := s.lifecycle.Stop(stop, *attempt)
			cancel()
			if stopErr == nil {
				env.LifecycleOperation = ""
			}
			err = errors.Join(err, stopErr)
		}
	}
	if err != nil {
		env.LifecycleError = bounded(operation + " failed: " + err.Error())
	}
	env.UpdatedAt = time.Now().UTC()
	return errors.Join(err, s.store.Save(env))
}

func (s *Service) runLifecycle() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if s.ctx.Err() != nil {
			return
		}
		s.sweepLifecycle(time.Now().UTC())
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) sweepLifecycle(now time.Time) {
	envs, err := s.store.List()
	if err != nil {
		slog.Error("scan preview lifecycle", "error", err)
		return
	}
	for _, env := range envs {
		if _, ok := s.cfg.Projects[env.Project]; !ok {
			continue
		}
		switch {
		case env.LifecycleOperation == restoring:
			err = s.changeLifecycle(env.ID, restoring, &now)
		case env.DeleteAt != nil && !env.DeleteAt.After(now):
			err = s.deleteEnvironment(env.ID, &now)
		case env.LifecycleOperation == archiving || (env.ArchiveAt != nil && !env.ArchiveAt.After(now) && env.ActiveDeploymentID != ""):
			err = s.changeLifecycle(env.ID, archiving, &now)
		default:
			continue
		}
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, state.ErrNotFound) && !errors.Is(err, ErrClosed) {
			slog.Error("process preview lifecycle", "environment", env.ID, "error", err)
		}
	}
}
