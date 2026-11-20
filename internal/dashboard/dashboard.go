package dashboard

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/st3fan/preview-controller/internal/state"
)

//go:embed dashboard.html assets
var files embed.FS

var templates = template.Must(template.ParseFS(files, "dashboard.html"))

type environment struct {
	Intent int64
	URL    string
	Commit string
	Status state.Status
}

type project struct {
	Name         string
	Environments []environment
}

type view struct {
	Unavailable bool
	Projects    []project
}

func Register(mux *http.ServeMux, shutdown context.Context, list func() ([]state.Environment, error), changes func() <-chan struct{}) {
	load := func() view {
		envs, err := list()
		if err != nil {
			return view{Unavailable: true}
		}
		var data view
		for _, env := range envs {
			if len(data.Projects) == 0 || data.Projects[len(data.Projects)-1].Name != env.Project {
				data.Projects = append(data.Projects, project{Name: env.Project})
			}
			row := environment{Intent: env.Intent, URL: env.URL, Commit: env.ActiveCommit}
			if len(env.Attempts) > 0 {
				row.Status = env.Attempts[len(env.Attempts)-1].Status
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
	for _, name := range []string{"htmx-2.0.11.min.js", "sse-2.2.4.js", "dashboard.css"} {
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
