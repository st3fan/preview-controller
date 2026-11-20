package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/githubmeta"
	"github.com/st3fan/preview-controller/internal/state"
)

type detailAttempt struct {
	state.Attempt
	LiveLogs   bool
	Diagnostic state.Diagnostic
}

func (a detailAttempt) TimeToPreview() string {
	if a.CreatedAt.IsZero() || a.ActivatedAt == nil || a.ActivatedAt.Before(a.CreatedAt) {
		return ""
	}
	return a.ActivatedAt.Sub(a.CreatedAt).Round(time.Millisecond).String()
}

type detailView struct {
	state.Environment
	IssueURL                  string
	IssueTitle                string
	RepositoryURL             string
	GitHubEnabled             bool
	GitHub                    githubmeta.Result
	Active                    *detailAttempt
	Recent                    []detailAttempt
	Activity                  []activityEvent
	ActivityPartial           bool
	DeploymentActivityPartial bool
}

func liveLogsAvailable(env state.Environment) bool {
	return !env.Deleting && env.DeleteAt == nil && env.LifecycleOperation == ""
}

func RegisterDetail(mux *http.ServeMux, cfg config.Config, load func(string, int64) (state.Environment, error)) {
	var lookup func(context.Context, string, int64) githubmeta.Result
	if token := os.Getenv(cfg.GitHubTokenEnv); strings.TrimSpace(token) != "" {
		lookup = githubmeta.New(token).Lookup
	}
	registerDetail(mux, cfg, load, lookup)
}

func registerDetail(mux *http.ServeMux, cfg config.Config, load func(string, int64) (state.Environment, error), lookup func(context.Context, string, int64) githubmeta.Result) {
	mux.HandleFunc("GET /dashboard/previews/{project}/{intent}", func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		intent, err := strconv.ParseInt(r.PathValue("intent"), 10, 64)
		if err != nil || intent < 1 {
			http.NotFound(w, r)
			return
		}
		env, err := load(project, intent)
		if errors.Is(err, state.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Could not load preview", http.StatusServiceUnavailable)
			return
		}
		data := detailView{Environment: env}
		if projectConfig, ok := cfg.Projects[project]; ok {
			if repository, err := url.Parse(projectConfig.Repository); err == nil {
				name := strings.TrimSuffix(strings.Trim(repository.Path, "/"), ".git")
				data.RepositoryURL = "https://github.com/" + name
				data.IssueURL = fmt.Sprintf("%s/issues/%d", data.RepositoryURL, intent)
			}
			if url, err := cfg.URL(project, intent); err == nil {
				data.URL = url
			}
		}
		for _, attempt := range slices.Backward(env.Attempts) {
			item := detailAttempt{Attempt: attempt, LiveLogs: liveLogsAvailable(env) && hasLogs(attempt), Diagnostic: env.Diagnostics[attempt.ID]}
			if data.IssueTitle == "" {
				data.IssueTitle = attempt.IssueTitle
			}
			if env.ActiveDeploymentID != "" && attempt.ID == env.ActiveDeploymentID {
				data.Active = &item
			}
			if len(data.Recent) < 10 {
				data.Recent = append(data.Recent, item)
			}
		}
		if data.RepositoryURL != "" && cfg.GitHubTokenEnv != "" {
			data.GitHubEnabled = true
			if lookup != nil {
				repositoryName := strings.TrimPrefix(data.RepositoryURL, "https://github.com/")
				data.GitHub = lookup(r.Context(), repositoryName, intent)
				if issue := data.GitHub.Intent; issue != nil && issue.Title != "" {
					data.IssueTitle = issue.Title
				}
			}
		}
		data.buildActivity()
		var page bytes.Buffer
		if err := templates.ExecuteTemplate(&page, "detail", data); err != nil {
			http.Error(w, "Could not render preview", http.StatusInternalServerError)
			return
		}
		setPageHeaders(w)
		_, _ = w.Write(page.Bytes())
	})
}

func setPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'")
}
