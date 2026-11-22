package dashboard

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/githubmeta"
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/state"
)

//go:embed *.html assets
var files embed.FS

var templates = template.Must(template.New("pages").Funcs(template.FuncMap{
	"controllerBuild": controllerBuild,
	"outcome":         attemptOutcome,
	"elapsedTime":     elapsedTime,
	"pullMergeTiming": pullMergeTiming,
	"isFragment":      func(url string) bool { return strings.HasPrefix(url, "#") },
}).ParseFS(files, "*.html"))

type environment struct {
	CanOperate     bool
	BackendName    string
	Project        string
	Archived       bool
	CanArchive     bool
	CanRestore     bool
	Deadline       *time.Time
	LifecycleError string
	Intent         int64
	IntentURL      string
	IssueTitle     string
	URL            string
	Commit         string
	ShortCommit    string
	CommitURL      string
	Operation      string
	Latest         *state.Attempt
	Health         health.Observation
	Logs           []state.Attempt
}

type project struct {
	Name         string
	Environments []environment
}

type view struct {
	SignedIn      bool
	Host          hostSnapshot
	Unavailable   bool
	Projects      []project
	Archived      []project
	LiveCount     int
	ArchivedCount int
}

func appendEnvironment(groups []project, row environment) []project {
	if len(groups) == 0 || groups[len(groups)-1].Name != row.Project {
		groups = append(groups, project{Name: row.Project})
	}
	group := &groups[len(groups)-1]
	group.Environments = append(group.Environments, row)
	return groups
}

func Register(mux *http.ServeMux, shutdown context.Context, cfg config.Config, detail func(string, int64) (state.Environment, error), list func() ([]state.Environment, error), changes func() <-chan struct{}, monitor *health.Monitor) {
	titles := newIssueTitles()
	if token := strings.TrimSpace(os.Getenv(cfg.GitHubTokenEnv)); token != "" {
		go titles.run(shutdown, cfg.Projects, list, changes, githubmeta.New(token).LookupTitle)
	}
	sample := newHostSampler(cfg.WorkspaceRoot)
	registerDetailPage(mux, sample, cfg, detail, monitor)
	registerDashboard(mux, sample, shutdown, cfg.Projects, list, changes, monitor, titles)
}

func registerDashboard(mux *http.ServeMux, sample func() hostSnapshot, shutdown context.Context, projects map[string]config.Project, list func() ([]state.Environment, error), changes func() <-chan struct{}, monitor *health.Monitor, titles *issueTitles) {
	load := func(r *http.Request) view {
		role := requestRole(r)
		data := view{SignedIn: role != ""}
		envs, err := list()
		if err != nil {
			data.Unavailable = true
			return data
		}

		for _, env := range envs {
			row := environment{
				CanOperate:     role == "" || role == roleOperate,
				BackendName:    backendName(env),
				Project:        env.Project,
				Health:         monitor.Lookup(env),
				Archived:       env.DeleteAt != nil,
				CanArchive:     !env.Deleting && env.DeleteAt == nil && env.ActiveDeploymentID != "",
				CanRestore:     !env.Deleting && env.DeleteAt != nil,
				Deadline:       env.ArchiveAt,
				LifecycleError: env.LifecycleError,
				Intent:         env.Intent, URL: env.URL, Commit: env.ActiveCommit,
				ShortCommit: env.ActiveCommit[:min(7, len(env.ActiveCommit))],
			}
			if project, ok := projects[env.Project]; ok {
				// Configuration has already validated the repository as a GitHub HTTPS URL.
				repository := strings.TrimSuffix(strings.TrimRight(project.Repository, "/"), ".git")
				row.IntentURL = fmt.Sprintf("%s/issues/%d", repository, env.Intent)
				if commitOID(env.ActiveCommit) {
					row.CommitURL = repository + "/commit/" + env.ActiveCommit
				}
			}
			if len(env.Attempts) > 0 {
				row.Latest = &env.Attempts[len(env.Attempts)-1]
			}
			row.IssueTitle = submittedTitle(env.Attempts)
			if row.IssueTitle == "" {
				row.IssueTitle = titles.get(state.ID(env.Project, env.Intent))
			}
			if liveActionsAvailable(env) {
				for _, attempt := range env.Attempts {
					if hasLogs(attempt) {
						row.Logs = append(row.Logs, attempt)
					}
				}
			}
			row.Operation = env.LifecycleOperation
			if env.Deleting {
				row.Operation = "deleting"
			}
			if row.Archived {
				row.Deadline = env.DeleteAt
				data.Archived = appendEnvironment(data.Archived, row)
				data.ArchivedCount++
			} else {
				data.Projects = appendEnvironment(data.Projects, row)
				data.LiveCount++
			}
		}
		return data
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var page bytes.Buffer
		data := load(r)
		data.Host = sample()
		if err := templates.ExecuteTemplate(&page, "page", data); err != nil {
			http.Error(w, "Could not render dashboard", http.StatusInternalServerError)
			return
		}
		setPageHeaders(w)
		if data.Unavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(page.Bytes())
	})
	mux.HandleFunc("GET /dashboard/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		stream(w, r, shutdown, func() view { return load(r) }, changes, monitor.Changes, titles.changes)
	})
	mux.HandleFunc("GET /dashboard/host", func(w http.ResponseWriter, r *http.Request) {
		setPageHeaders(w)
		var fragment bytes.Buffer
		if err := templates.ExecuteTemplate(&fragment, "host-values", sample()); err != nil {
			http.Error(w, "Could not render host readings", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(fragment.Bytes())
	})
	for _, name := range []string{"host.js", "htmx-2.0.11.min.js", "sse-2.2.4.js", "dashboard.css", "theme.js", "delete.js", "trash.svg", "icons.svg", "bootstrap-reboot-5.3.8.min.css"} {
		mux.HandleFunc("GET /dashboard/assets/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, files, "assets/"+name)
		})
	}
}

func stream(w http.ResponseWriter, r *http.Request, shutdown context.Context, load func() view, changes, healthChanges, titleChanges func() <-chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	write := func(message string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := io.WriteString(w, message); err != nil {
			return err
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		return controller.SetWriteDeadline(time.Time{})
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var previous string
	for {
		changed := changes()
		healthChanged := healthChanges()
		titlesChanged := titleChanges()
		var fragment bytes.Buffer
		if err := templates.ExecuteTemplate(&fragment, "environments", load()); err != nil {
			return
		}
		html := fragment.String()
		if html != previous {
			message := fmt.Sprintf("event: environments\ndata: %s\n\n", strings.ReplaceAll(strings.ReplaceAll(html, "\r", "&#13;"), "\n", "\ndata: "))
			if err := write(message); err != nil {
				return
			}
			previous = html
		}
		for waiting := true; waiting; {
			select {
			case <-r.Context().Done():
				return
			case <-shutdown.Done():
				return
			case <-changed:
				waiting = false
			case <-titlesChanged:
				waiting = false
			case <-healthChanged:
				waiting = false
			case <-heartbeat.C:
				if err := write(": keepalive\n\n"); err != nil {
					return
				}
			}
		}
	}
}
