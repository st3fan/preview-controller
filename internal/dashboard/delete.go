package dashboard

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/state"
)

func RegisterDelete(mux *http.ServeMux, remove func(string) error) {
	protect := http.NewCrossOriginProtection()
	mux.Handle("POST /dashboard/environments/{project}/{intent}/delete", protect.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("X-Preview-Action") != "delete" {
			http.Error(w, "Invalid dashboard action.", http.StatusForbidden)
			return
		}
		intent, err := strconv.ParseInt(r.PathValue("intent"), 10, 64)
		if err != nil || intent < 1 {
			http.NotFound(w, r)
			return
		}
		id := state.ID(r.PathValue("project"), intent)
		// Cleanup can exceed the management server's ordinary write timeout.
		// The service bounds every runtime cleanup call separately.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		switch err := remove(id); {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, deploy.ErrConflict):
			http.Error(w, "The preview is busy. Try again after the current operation finishes.", http.StatusConflict)
		case errors.Is(err, state.ErrNotFound):
			http.NotFound(w, r)
		default:
			slog.Error("dashboard deletion failed", "environment", id, "error", err)
			http.Error(w, "Cleanup failed. Try deleting the preview again.", http.StatusInternalServerError)
		}
	})))
}
