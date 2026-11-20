package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

var (
	ErrUnknownProject = errors.New("unknown project")
	ErrInvalidIntent  = errors.New("intent must be positive")
	ErrInvalidCommit  = errors.New("commit must be a full 40-character SHA")
	ErrConflict       = errors.New("environment already has an in-progress deployment")
	ErrBusy           = errors.New("deployment intake is busy")
	ErrClosed         = errors.New("deployment intake is closed")
	commitPattern     = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// Verifier checks that a commit object exists in the configured public repository.
type Verifier interface {
	Verify(context.Context, string, string) error
}

type Service struct {
	cfg      config.Config
	store    *state.Store
	verifier Verifier
	mu       sync.Mutex
	sem      chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

func New(cfg config.Config, store *state.Store, verifier Verifier) (*Service, error) {
	if store == nil || verifier == nil {
		return nil, errors.New("state store and verifier are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, store: store, verifier: verifier, sem: make(chan struct{}, 4), ctx: ctx, cancel: cancel}
	if err := s.reconcile(); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) Submit(project string, intent int64, commit string) (state.Attempt, state.Environment, error) {
	projectCfg, ok := s.cfg.Projects[project]
	if !ok {
		return state.Attempt{}, state.Environment{}, ErrUnknownProject
	}
	if intent < 1 {
		return state.Attempt{}, state.Environment{}, ErrInvalidIntent
	}
	if !commitPattern.MatchString(commit) {
		return state.Attempt{}, state.Environment{}, ErrInvalidCommit
	}
	url, err := s.cfg.URL(project, intent)
	if err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return state.Attempt{}, state.Environment{}, ErrClosed
	}
	env, err := s.store.Load(project, intent)
	if errors.Is(err, state.ErrNotFound) {
		env = state.Environment{ID: state.ID(project, intent), Project: project, Intent: intent, URL: url, Attempts: []state.Attempt{}}
	} else if err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	if len(env.Attempts) > 0 && inProgress(env.Attempts[len(env.Attempts)-1].Status) {
		return state.Attempt{}, state.Environment{}, ErrConflict
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return state.Attempt{}, state.Environment{}, ErrBusy
	}
	release := true
	defer func() {
		if release {
			<-s.sem
		}
	}()
	id, err := newID()
	if err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	now := time.Now().UTC()
	attempt := state.Attempt{ID: id, Commit: strings.ToLower(commit), Status: state.Queued, CreatedAt: now, UpdatedAt: now}
	env.Attempts = append(env.Attempts, attempt)
	env.UpdatedAt = now
	if err := s.store.Save(env); err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	s.wg.Add(1)
	release = false
	go s.verify(project, intent, id, projectCfg.Repository, attempt.Commit)
	return attempt, env, nil
}

func (s *Service) GetEnvironment(id string) (state.Environment, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return state.Environment{}, state.ErrNotFound
	}
	if _, ok := s.cfg.Projects[parts[0]]; !ok {
		return state.Environment{}, state.ErrNotFound
	}
	intent, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || intent < 1 {
		return state.Environment{}, state.ErrNotFound
	}
	return s.store.Load(parts[0], intent)
}

func (s *Service) ListEnvironments() ([]state.Environment, error) {
	envs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	sort.Slice(envs, func(i, j int) bool {
		if envs[i].Project != envs[j].Project {
			return envs[i].Project < envs[j].Project
		}
		return envs[i].Intent < envs[j].Intent
	})
	return envs, nil
}

func (s *Service) GetDeployment(id string) (state.Attempt, state.Environment, error) {
	envs, err := s.store.List()
	if err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	for _, env := range envs {
		for _, attempt := range env.Attempts {
			if attempt.ID == id {
				return attempt, env, nil
			}
		}
	}
	return state.Attempt{}, state.Environment{}, state.ErrNotFound
}

func (s *Service) verify(project string, intent int64, id, repository, commit string) {
	defer s.wg.Done()
	defer func() { <-s.sem }()
	if err := s.update(project, intent, id, state.Fetching, ""); err != nil {
		slog.Error("record fetching state", "deployment", id, "error", err)
		return
	}
	if err := s.verifier.Verify(s.ctx, repository, commit); err != nil {
		if updateErr := s.update(project, intent, id, state.Failed, "commit verification failed: "+bounded(err.Error())); updateErr != nil {
			slog.Error("record verification failure", "deployment", id, "error", updateErr)
		}
		return
	}
	if err := s.update(project, intent, id, state.Verified, ""); err != nil {
		slog.Error("record verified state", "deployment", id, "error", err)
	}
}

func (s *Service) update(project string, intent int64, id string, status state.Status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, err := s.store.Load(project, intent)
	if err != nil {
		return err
	}
	for i := range env.Attempts {
		if env.Attempts[i].ID == id {
			now := time.Now().UTC()
			env.Attempts[i].Status = status
			env.Attempts[i].Error = message
			env.Attempts[i].UpdatedAt = now
			env.UpdatedAt = now
			return s.store.Save(env)
		}
	}
	return state.ErrNotFound
}

func (s *Service) reconcile() error {
	envs, err := s.store.List()
	if err != nil {
		return err
	}
	for _, env := range envs {
		changed := false
		for i := range env.Attempts {
			if inProgress(env.Attempts[i].Status) {
				now := time.Now().UTC()
				env.Attempts[i].Status = state.Failed
				env.Attempts[i].Error = "controller restarted before deployment completed"
				env.Attempts[i].UpdatedAt = now
				env.UpdatedAt = now
				changed = true
			}
		}
		if changed {
			if err := s.store.Save(env); err != nil {
				return err
			}
		}
	}
	return nil
}

func inProgress(status state.Status) bool {
	switch status {
	case state.Queued, state.Fetching, state.Verified, state.Building, state.Starting:
		return true
	default:
		return false
	}
}

func newID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "deployment-" + hex.EncodeToString(random[:]), nil
}

func bounded(message string) string {
	if len(message) > 500 {
		return message[:500]
	}
	return message
}
