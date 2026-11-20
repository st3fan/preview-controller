package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/runtime"
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

type Runner interface {
	Run(context.Context, string, int64, config.Project, state.Attempt, runtime.Progress) (runtime.Result, error)
	Cleanup(context.Context, string, int64, state.Attempt) error
	Check(context.Context, state.Attempt, config.Project) (bool, error)
}

type Service struct {
	cfg      config.Config
	store    *state.Store
	verifier Verifier
	runner   Runner
	mu       sync.Mutex
	sem      chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
	deleting map[string]bool
	running  map[string]bool
}

func New(cfg config.Config, store *state.Store, verifier Verifier, runners ...Runner) (*Service, error) {
	if store == nil || verifier == nil {
		return nil, errors.New("state store and verifier are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, store: store, verifier: verifier, sem: make(chan struct{}, 4), ctx: ctx, cancel: cancel, deleting: map[string]bool{}, running: map[string]bool{}}
	if len(runners) > 0 {
		s.runner = runners[0]
	}
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
	return s.SubmitWithIssueTitle(project, intent, commit, "")
}

func (s *Service) SubmitWithIssueTitle(project string, intent int64, commit, title string) (state.Attempt, state.Environment, error) {
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
	key := state.ID(project, intent)
	if s.deleting[key] || s.running[key] {
		return state.Attempt{}, state.Environment{}, ErrConflict
	}
	env, err := s.store.Load(project, intent)
	if errors.Is(err, state.ErrNotFound) {
		env = state.Environment{ID: state.ID(project, intent), Project: project, Intent: intent, Attempts: []state.Attempt{}}
	} else if err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	if env.Deleting {
		return state.Attempt{}, state.Environment{}, ErrConflict
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
	attempt := state.Attempt{ID: id, Commit: strings.ToLower(commit), IssueTitle: strings.TrimSpace(title), Status: state.Queued, CreatedAt: now, UpdatedAt: now}
	env.URL = url
	env.Attempts = append(env.Attempts, attempt)
	env.UpdatedAt = now
	if err := s.store.Save(env); err != nil {
		return state.Attempt{}, state.Environment{}, err
	}
	s.running[key] = true
	s.wg.Add(1)
	release = false
	slog.Debug("deployment queued", "project", project, "intent", intent, "deployment", id, "commit", attempt.Commit)
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
	env, err := s.store.Load(parts[0], intent)
	if err != nil {
		return state.Environment{}, err
	}
	s.refreshPreviewURL(&env)
	return env, nil
}

func (s *Service) ListEnvironments() ([]state.Environment, error) {
	envs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	for i := range envs {
		s.refreshPreviewURL(&envs[i])
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
				s.refreshPreviewURL(&env)
				return attempt, env, nil
			}
		}
	}
	return state.Attempt{}, state.Environment{}, state.ErrNotFound
}

// Derive the URL from current configuration so stored deployments need no migration.
func (s *Service) refreshPreviewURL(env *state.Environment) {
	if url, err := s.cfg.URL(env.Project, env.Intent); err == nil {
		env.URL = url
	}
}

func (s *Service) verify(project string, intent int64, id, repository, commit string) {
	defer func() {
		s.mu.Lock()
		delete(s.running, state.ID(project, intent))
		s.mu.Unlock()
		<-s.sem
		s.wg.Done()
	}()
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
		return
	}
	if s.runner == nil {
		return
	}
	env, err := s.store.Load(project, intent)
	if err != nil {
		slog.Error("load candidate for runtime", "deployment", id, "error", err)
		return
	}
	var candidate state.Attempt
	for _, item := range env.Attempts {
		if item.ID == id {
			candidate = item
			break
		}
	}
	result, err := s.runner.Run(s.ctx, project, intent, s.cfg.Projects[project], candidate, func(status state.Status, result runtime.Result) error {
		return s.updateWithResult(project, intent, id, status, "", &result)
	})
	if err != nil {
		if updateErr := s.update(project, intent, id, state.Failed, "deployment failed: "+bounded(err.Error())); updateErr != nil {
			slog.Error("record runtime failure", "deployment", id, "error", updateErr)
		}
		return
	}
	slog.Debug("checking candidate before route switch", "deployment", id, "port", result.Port)
	previous, err := s.activate(project, intent, candidate, result)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cleanupErr := s.runner.Cleanup(cleanup, project, intent, candidate)
		cancel()
		message := "proxy cutover failed: " + bounded(err.Error())
		if cleanupErr != nil {
			message += "; cleanup: " + bounded(cleanupErr.Error())
		}
		if updateErr := s.update(project, intent, id, state.Failed, message); updateErr != nil {
			slog.Error("record cutover failure", "deployment", id, "error", updateErr)
		}
		return
	}
	slog.Debug("preview route activated", "project", project, "intent", intent, "deployment", id, "commit", commit, "port", result.Port)
	if previous.ID != "" && previous.ID != id {
		slog.Debug("cleaning up replaced deployment", "deployment", previous.ID)
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := s.runner.Cleanup(cleanup, project, intent, previous); err != nil {
			slog.Error("retire previous attempt", "deployment", previous.ID, "error", err)
		} else if err := s.update(project, intent, previous.ID, state.Retired, ""); err != nil {
			slog.Error("record retired attempt", "deployment", previous.ID, "error", err)
		}
		cancel()
	}
}

func (s *Service) activate(project string, intent int64, candidate state.Attempt, result runtime.Result) (state.Attempt, error) {
	checked := candidate
	checked.Port = result.Port
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	alive, err := s.runner.Check(ctx, checked, s.cfg.Projects[project])
	cancel()
	if err != nil {
		return state.Attempt{}, err
	}
	if !alive {
		return state.Attempt{}, errors.New("candidate failed its cutover health check")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	env, err := s.store.Load(project, intent)
	if err != nil {
		return state.Attempt{}, err
	}
	if env.Deleting {
		return state.Attempt{}, ErrConflict
	}
	var previous state.Attempt
	for _, item := range env.Attempts {
		if item.ID == env.ActiveDeploymentID {
			previous = item
		}
	}
	found := false
	for i := range env.Attempts {
		if env.Attempts[i].ID == candidate.ID {
			if env.Attempts[i].Status != state.Starting {
				return state.Attempt{}, fmt.Errorf("candidate has unexpected status %s", env.Attempts[i].Status)
			}
			env.Attempts[i].Status = state.Healthy
			env.Attempts[i].Port = result.Port
			env.Attempts[i].Workspace = result.Workspace
			env.Attempts[i].ComposeProject = result.ComposeProject
			env.Attempts[i].UpdatedAt = time.Now().UTC()
			found = true
			break
		}
	}
	if !found {
		return state.Attempt{}, state.ErrNotFound
	}
	env.ActiveDeploymentID = candidate.ID
	env.ActiveCommit = candidate.Commit
	env.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(env); err != nil {
		return state.Attempt{}, err
	}
	return previous, nil
}

func (s *Service) ActivePort(project string, intent int64) (int, error) {
	port, _, err := s.PreviewRoute(project, intent)
	if err == nil && port == 0 {
		return 0, state.ErrNotFound
	}
	return port, err
}

// PreviewRoute resolves the active target and fallback status from the same snapshot.
func (s *Service) PreviewRoute(project string, intent int64) (int, state.Status, error) {
	attempt, status, err := s.PreviewDeployment(project, intent)
	return attempt.Port, status, err
}

// PreviewDeployment resolves the target and its metadata from the same snapshot.
func (s *Service) PreviewDeployment(project string, intent int64) (state.Attempt, state.Status, error) {
	env, err := s.store.Load(project, intent)
	if err != nil {
		return state.Attempt{}, "", err
	}
	if env.Deleting {
		return state.Attempt{}, "", state.ErrNotFound
	}
	if env.ActiveDeploymentID != "" {
		for _, attempt := range env.Attempts {
			if attempt.ID == env.ActiveDeploymentID && attempt.Status == state.Healthy && attempt.Port > 0 && attempt.Port <= 65535 {
				return attempt, "", nil
			}
		}
		return state.Attempt{}, "", state.ErrNotFound
	}
	if len(env.Attempts) > 0 {
		return state.Attempt{}, env.Attempts[len(env.Attempts)-1].Status, nil
	}
	return state.Attempt{}, "", state.ErrNotFound
}

func (s *Service) DeleteEnvironment(id string) error {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return state.ErrNotFound
	}
	project, intentText := parts[0], parts[1]
	if _, ok := s.cfg.Projects[project]; !ok {
		return state.ErrNotFound
	}
	intent, err := strconv.ParseInt(intentText, 10, 64)
	if err != nil || intent < 1 {
		return state.ErrNotFound
	}
	key := state.ID(project, intent)
	s.mu.Lock()
	if s.deleting[key] || s.running[key] {
		s.mu.Unlock()
		return ErrConflict
	}
	env, err := s.store.Load(project, intent)
	if errors.Is(err, state.ErrNotFound) {
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	for _, attempt := range env.Attempts {
		if inProgress(attempt.Status) {
			s.mu.Unlock()
			return ErrConflict
		}
	}
	env.Deleting = true
	env.ActiveDeploymentID = ""
	env.ActiveCommit = ""
	env.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(env); err != nil {
		s.mu.Unlock()
		return err
	}
	s.deleting[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.deleting, key)
		s.mu.Unlock()
	}()
	slog.Debug("removing preview environment", "project", project, "intent", intent, "attempts", len(env.Attempts))
	var problems []error
	if s.runner != nil {
		for _, attempt := range env.Attempts {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := s.runner.Cleanup(ctx, project, intent, attempt); err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", attempt.ID, err))
			}
			cancel()
		}
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	if err := s.store.Delete(project, intent); err != nil {
		return err
	}
	slog.Debug("preview environment removed", "project", project, "intent", intent)
	return nil
}

func (s *Service) update(project string, intent int64, id string, status state.Status, message string) error {
	return s.updateWithResult(project, intent, id, status, message, nil)
}

func (s *Service) updateWithResult(project string, intent int64, id string, status state.Status, message string, result *runtime.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env, err := s.store.Load(project, intent)
	if err != nil {
		return err
	}
	for i := range env.Attempts {
		if env.Attempts[i].ID == id {
			now := time.Now().UTC()
			previous := env.Attempts[i].Status
			env.Attempts[i].Status = status
			env.Attempts[i].Error = message
			if result != nil {
				env.Attempts[i].Port = result.Port
				env.Attempts[i].Workspace = result.Workspace
				env.Attempts[i].ComposeProject = result.ComposeProject
			}
			env.Attempts[i].UpdatedAt = now
			env.UpdatedAt = now
			if err := s.store.Save(env); err != nil {
				return err
			}
			if previous != status {
				slog.Debug("deployment status changed", "project", project, "intent", intent, "deployment", id, "previous", previous, "status", status)
			}
			return nil
		}
	}
	return state.ErrNotFound
}

func (s *Service) reconcile() error {
	envs, err := s.store.List()
	if err != nil {
		return err
	}
	slog.Debug("reconciling saved preview environments", "environments", len(envs))
	for _, env := range envs {
		changed := false
		activeFound := false
		for i := range env.Attempts {
			attempt := &env.Attempts[i]
			if attempt.ID == env.ActiveDeploymentID && attempt.Status == state.Healthy {
				activeFound = true
			}
			if attempt.Status == state.Healthy && s.runner != nil && !env.Deleting && attempt.ID == env.ActiveDeploymentID {
				if projectCfg, ok := s.cfg.Projects[env.Project]; ok {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					alive, err := s.runner.Check(ctx, *attempt, projectCfg)
					cancel()
					if err != nil {
						slog.Error("inspect previous runtime", "deployment", attempt.ID, "error", err)
					}
					if err != nil || !alive {
						now := time.Now().UTC()
						attempt.Status = state.Failed
						attempt.Error = "runtime was not healthy after controller restart"
						attempt.UpdatedAt = now
						env.ActiveDeploymentID = ""
						env.ActiveCommit = ""
						env.UpdatedAt = now
						changed = true
					}
				}
			}
			if attempt.Status == state.Healthy && s.runner != nil && attempt.ID != env.ActiveDeploymentID {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				err := s.runner.Cleanup(ctx, env.Project, env.Intent, *attempt)
				cancel()
				if err != nil {
					slog.Error("cleanup inactive runtime", "deployment", attempt.ID, "error", err)
				} else {
					attempt.Status = state.Retired
					attempt.UpdatedAt = time.Now().UTC()
					env.UpdatedAt = attempt.UpdatedAt
					changed = true
				}
			}
			if inProgress(attempt.Status) {
				slog.Debug("recovering interrupted deployment", "deployment", attempt.ID, "status", attempt.Status)
				message := "controller restarted before deployment completed"
				if s.runner != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					if err := s.runner.Cleanup(ctx, env.Project, env.Intent, *attempt); err != nil {
						message += "; cleanup: " + bounded(err.Error())
					}
					cancel()
				}
				now := time.Now().UTC()
				attempt.Status = state.Failed
				attempt.Error = message
				attempt.UpdatedAt = now
				env.UpdatedAt = now
				changed = true
			}
		}
		if env.ActiveDeploymentID != "" && !activeFound {
			env.ActiveDeploymentID = ""
			env.ActiveCommit = ""
			env.UpdatedAt = time.Now().UTC()
			changed = true
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
