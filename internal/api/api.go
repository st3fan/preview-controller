package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/state"
)

type Service interface {
	Submit(string, int64, string) (state.Attempt, state.Environment, error)
	ListEnvironments() ([]state.Environment, error)
	GetEnvironment(string) (state.Environment, error)
	GetDeployment(string) (state.Attempt, state.Environment, error)
	DeleteEnvironment(string) error
}

func Handler(service Service, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/deployments", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r.Header.Get("Authorization"), token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
		var input struct {
			Project string `json:"project"`
			Intent  int64  `json:"intent"`
			Commit  string `json:"commit"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid deployment request")
			return
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "request must contain one JSON object")
			return
		}
		if input.Project == "" || input.Intent < 1 || input.Commit == "" {
			writeError(w, http.StatusBadRequest, "project, positive intent, and commit are required")
			return
		}
		attempt, env, err := service.Submit(input.Project, input.Intent, input.Commit)
		if err != nil {
			switch {
			case errors.Is(err, deploy.ErrUnknownProject):
				writeError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, deploy.ErrInvalidIntent), errors.Is(err, deploy.ErrInvalidCommit):
				writeError(w, http.StatusBadRequest, err.Error())
			case errors.Is(err, deploy.ErrConflict):
				writeError(w, http.StatusConflict, err.Error())
			case errors.Is(err, deploy.ErrBusy), errors.Is(err, deploy.ErrClosed):
				writeError(w, http.StatusServiceUnavailable, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "deployment request failed")
			}
			return
		}
		writeJSON(w, http.StatusAccepted, struct {
			ID     string       `json:"id"`
			Status state.Status `json:"status"`
			URL    string       `json:"url"`
		}{ID: attempt.ID, Status: attempt.Status, URL: env.URL})
	})
	mux.HandleFunc("GET /api/environments", func(w http.ResponseWriter, _ *http.Request) {
		envs, err := service.ListEnvironments()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not list environments")
			return
		}
		writeJSON(w, http.StatusOK, envs)
	})
	mux.HandleFunc("GET /api/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		env, err := service.GetEnvironment(r.PathValue("id"))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeError(w, http.StatusNotFound, "environment not found")
			} else {
				writeError(w, http.StatusInternalServerError, "could not load environment")
			}
			return
		}
		writeJSON(w, http.StatusOK, env)
	})
	mux.HandleFunc("DELETE /api/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r.Header.Get("Authorization"), token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err := service.DeleteEnvironment(r.PathValue("id")); err != nil {
			switch {
			case errors.Is(err, state.ErrNotFound):
				writeError(w, http.StatusNotFound, "environment not found")
			case errors.Is(err, deploy.ErrConflict):
				writeError(w, http.StatusConflict, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "could not delete environment")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/deployments/{id}", func(w http.ResponseWriter, r *http.Request) {
		attempt, env, err := service.GetDeployment(r.PathValue("id"))
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				writeError(w, http.StatusNotFound, "deployment not found")
			} else {
				writeError(w, http.StatusInternalServerError, "could not load deployment")
			}
			return
		}
		writeJSON(w, http.StatusOK, struct {
			state.Attempt
			EnvironmentID string `json:"environment_id"`
			URL           string `json:"url"`
		}{Attempt: attempt, EnvironmentID: env.ID, URL: env.URL})
	})
	return mux
}

func authorized(header, token string) bool {
	if token == "" || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(header, "Bearer ")
	if provided == "" || strings.ContainsAny(provided, " \t\r\n") {
		return false
	}
	expectedHash, providedHash := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
