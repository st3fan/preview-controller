package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

func hasLogs(attempt state.Attempt) bool {
	return attempt.ComposeProject != "" && (attempt.Status == state.Starting || attempt.Status == state.Healthy)
}

func RegisterLogs(mux *http.ServeMux, shutdown context.Context,
	load func(string, int64) (state.Environment, error),
	open func(context.Context, state.Attempt) (func(io.Writer) error, error),
) {
	mux.HandleFunc("GET /dashboard/logs/{project}/{intent}/{attempt}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		intent, err := strconv.ParseInt(r.PathValue("intent"), 10, 64)
		if err != nil || intent < 1 {
			http.NotFound(w, r)
			return
		}
		env, err := load(r.PathValue("project"), intent)
		if errors.Is(err, state.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Could not load deployment", http.StatusServiceUnavailable)
			return
		}
		for _, attempt := range env.Attempts {
			if attempt.ID != r.PathValue("attempt") {
				continue
			}
			if env.Deleting || !hasLogs(attempt) {
				http.Error(w, "Deployment logs are unavailable", http.StatusGone)
				return
			}

			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(shutdown, cancel)
			defer stop()
			follow, err := open(ctx, attempt)
			if errors.Is(err, runtime.ErrLogsUnavailable) {
				http.Error(w, "Deployment logs are unavailable", http.StatusGone)
				return
			}
			if err != nil {
				http.Error(w, "Could not open deployment logs", http.StatusServiceUnavailable)
				return
			}
			if r.Method == http.MethodHead {
				return
			}

			output := logStream{w, http.NewResponseController(w), cancel}
			if _, err := output.Write(nil); err != nil {
				return
			}
			if err := follow(output); err != nil && ctx.Err() == nil {
				_, _ = fmt.Fprintln(output, "\nLog stream ended because Docker Compose could not continue.")
			}
			return
		}
		http.NotFound(w, r)
	})
}

type logStream struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	cancel     context.CancelFunc
}

func (output logStream) Write(p []byte) (_ int, err error) {
	defer func() {
		if err != nil {
			output.cancel()
		}
	}()
	if err := output.controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	n, err := output.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := output.controller.Flush(); err != nil {
		return n, err
	}
	return n, output.controller.SetWriteDeadline(time.Time{})
}
