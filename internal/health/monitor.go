package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

const (
	interval         = 30 * time.Second
	timeout          = 3 * time.Second
	concurrency      = 4
	failureThreshold = 2
)

type Observation struct {
	Status      string
	LastSuccess time.Time
	Failure     string
}

type target struct {
	deployment string
	port       int
	updated    time.Time
}

type sample struct {
	target      target
	observation Observation
	failures    int
}

type Monitor struct {
	store    *state.Store
	projects map[string]config.Project
	probe    func(context.Context, int, string) error

	mu      sync.Mutex
	samples map[string]sample
	changed chan struct{}
}

func New(store *state.Store, projects map[string]config.Project, probe func(context.Context, int, string) error) *Monitor {
	return &Monitor{store: store, projects: projects, probe: probe, samples: make(map[string]sample), changed: make(chan struct{})}
}

func activeTarget(env state.Environment) target {
	attempt, ok := env.ActivePreview()
	if !ok {
		return target{}
	}
	return target{deployment: attempt.ID, port: attempt.Port, updated: attempt.UpdatedAt}
}

func (m *Monitor) Lookup(env state.Environment) Observation {
	key := activeTarget(env)
	if _, configured := m.projects[env.Project]; !configured || key == (target{}) {
		return Observation{Status: "Inactive"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if result := m.samples[env.ID]; result.target == key {
		return result.observation
	}
	return Observation{Status: "Unknown"}
}

func (m *Monitor) Changes() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		m.poll(ctx)
		// Discard any tick that arrived during the round.
		select {
		case <-ticker.C:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Monitor) poll(ctx context.Context) {
	envs, err := m.store.List()
	if err != nil {
		slog.Warn("list previews for health monitoring", "error", err)
		return
	}
	active := make(map[string]target)
	jobs := make(chan state.Environment, len(envs))
	for _, env := range envs {
		if key := activeTarget(env); key != (target{}) {
			if _, ok := m.projects[env.Project]; ok {
				active[env.ID] = key
				jobs <- env
			}
		}
	}
	close(jobs)

	m.mu.Lock()
	for id, result := range m.samples {
		if active[id] != result.target {
			delete(m.samples, id)
		}
	}
	m.mu.Unlock()

	var workers sync.WaitGroup
	for range min(concurrency, len(jobs)) {
		workers.Go(func() {
			for env := range jobs {
				if ctx.Err() != nil {
					return
				}
				key := active[env.ID]
				check, cancel := context.WithTimeout(ctx, timeout)
				err := m.probe(check, key.port, m.projects[env.Project].HealthPath)
				cancel()
				if ctx.Err() == nil {
					m.record(env, key, err)
				}
			}
		})
	}
	workers.Wait()
}

func (m *Monitor) record(env state.Environment, key target, failure error) {
	current, err := m.store.Load(env.Project, env.Intent)
	if err != nil || activeTarget(current) != key {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	result := m.samples[env.ID]
	if result.target != key {
		result = sample{target: key, observation: Observation{Status: "Unknown"}}
	}
	previous := result.observation
	if failure == nil {
		result.observation = Observation{Status: "Healthy", LastSuccess: time.Now().UTC()}
		result.failures = 0
	} else {
		result.failures = min(result.failures+1, failureThreshold)
		result.observation.Failure = failure.Error()
		if result.failures == failureThreshold {
			result.observation.Status = "Unhealthy"
		}
	}
	m.samples[env.ID] = result
	if previous != result.observation {
		close(m.changed)
		m.changed = make(chan struct{})
	}
}
