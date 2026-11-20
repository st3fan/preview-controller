package dashboard

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

//go:embed dashboard.html assets
var files embed.FS

var templates = template.Must(template.ParseFS(files, "dashboard.html"))

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type environment struct {
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
	Status         state.Status
	Logs           []state.Attempt
}

type project struct {
	Name         string
	Environments []environment
}

type view struct {
	Unavailable bool
	Projects    []project
	Archived    []environment
}

func Register(mux *http.ServeMux, shutdown context.Context, projects map[string]config.Project, list func() ([]state.Environment, error), changes func() <-chan struct{}) {
	load := func() view {
		envs, err := list()
		if err != nil {
			return view{Unavailable: true}
		}
		var data view
		for _, env := range envs {
			row := environment{
				Project:        env.Project,
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
				if commitSHA.MatchString(env.ActiveCommit) {
					row.CommitURL = repository + "/commit/" + env.ActiveCommit
				}
			}
			if len(env.Attempts) > 0 {
				row.Status = env.Attempts[len(env.Attempts)-1].Status
			}
			for _, attempt := range slices.Backward(env.Attempts) {
				if attempt.IssueTitle != "" {
					row.IssueTitle = attempt.IssueTitle
					break
				}
			}
			if !env.Deleting && !row.Archived && env.LifecycleOperation == "" {
				for _, attempt := range env.Attempts {
					if hasLogs(attempt) {
						row.Logs = append(row.Logs, attempt)
					}
				}
			}
			if env.Deleting {
				row.Status = "deleting"
			} else if env.LifecycleOperation != "" {
				row.Status = state.Status(env.LifecycleOperation)
			} else if row.Archived {
				row.Status = "archived"
			}
			if row.Archived {
				row.Deadline = env.DeleteAt
				data.Archived = append(data.Archived, row)
				continue
			}
			if len(data.Projects) == 0 || data.Projects[len(data.Projects)-1].Name != env.Project {
				data.Projects = append(data.Projects, project{Name: env.Project})
			}
			group := &data.Projects[len(data.Projects)-1]
			group.Environments = append(group.Environments, row)
		}
		return data
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		var page bytes.Buffer
		data := load()
		if err := templates.ExecuteTemplate(&page, "page", data); err != nil {
			http.Error(w, "Could not render dashboard", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'")
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
		stream(w, r, shutdown, load, changes)
	})
	for _, name := range []string{"htmx-2.0.11.min.js", "sse-2.2.4.js", "dashboard.css", "theme.js", "delete.js", "trash.svg"} {
		mux.HandleFunc("GET /dashboard/assets/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, files, "assets/"+name)
		})
	}
}

func stream(w http.ResponseWriter, r *http.Request, shutdown context.Context, load func() view, changes func() <-chan struct{}) {
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
			case <-heartbeat.C:
				if err := write(": keepalive\n\n"); err != nil {
					return
				}
			}
		}
	}
}
