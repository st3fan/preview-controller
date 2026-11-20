package dashboard

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

func submittedTitle(attempts []state.Attempt) string {
	for _, attempt := range slices.Backward(attempts) {
		if title := strings.TrimSpace(attempt.IssueTitle); title != "" {
			return title
		}
	}
	return ""
}

// GitHub titles are operator metadata. Keep them out of deployment state, which
// also feeds the visitor-facing preview inspector.
type issueTitles struct {
	mu      sync.Mutex
	titles  map[string]string
	changed chan struct{}
}

func newIssueTitles() *issueTitles {
	return &issueTitles{titles: make(map[string]string), changed: make(chan struct{})}
}

func (c *issueTitles) get(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.titles[id]
}

func (c *issueTitles) changes() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func (c *issueTitles) set(id, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.titles[id] == title {
		return
	}
	if title == "" {
		delete(c.titles, id)
	} else {
		c.titles[id] = title
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *issueTitles) run(ctx context.Context, projects map[string]config.Project, list func() ([]state.Environment, error), changes func() <-chan struct{}, lookup func(context.Context, string, int64) string) {
	const retryInterval = 15 * time.Minute
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	nextLookup := make(map[string]time.Time)
	for ctx.Err() == nil {
		changed := changes()
		envs, err := list()
		if err == nil {
			needed := make(map[string]bool)
			for _, env := range envs {
				if ctx.Err() != nil {
					return
				}
				project, ok := projects[env.Project]
				if !ok || submittedTitle(env.Attempts) != "" {
					continue
				}
				id := state.ID(env.Project, env.Intent)
				needed[id] = true
				if time.Now().Before(nextLookup[id]) {
					continue
				}
				repo := strings.TrimSuffix(strings.TrimRight(strings.TrimPrefix(project.Repository, "https://github.com/"), "/"), ".git")
				title := lookup(ctx, repo, env.Intent)
				if title != "" {
					c.set(id, title)
					nextLookup[id] = time.Now().Add(time.Hour)
				} else {
					// Retain the last known title through a transient outage.
					nextLookup[id] = time.Now().Add(retryInterval)
				}
			}
			for id := range nextLookup {
				if !needed[id] {
					delete(nextLookup, id)
					c.set(id, "")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-ticker.C:
		}
	}
}
