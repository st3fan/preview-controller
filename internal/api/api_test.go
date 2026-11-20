package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/st3fan/preview-controller/internal/state"
)

type stubService struct{ submits, deletes int }

func (s *stubService) Submit(project string, intent int64, commit string) (state.Attempt, state.Environment, error) {
	s.submits++
	return state.Attempt{ID: "deployment-1", Status: state.Queued}, state.Environment{ID: state.ID(project, intent), URL: "https://myproject-123.preview.example.com"}, nil
}
func (s *stubService) ListEnvironments() ([]state.Environment, error) {
	return []state.Environment{}, nil
}
func (s *stubService) GetEnvironment(id string) (state.Environment, error) {
	if id != "myproject:123" {
		return state.Environment{}, state.ErrNotFound
	}
	return state.Environment{ID: id}, nil
}
func (s *stubService) GetDeployment(id string) (state.Attempt, state.Environment, error) {
	if id != "deployment-1" {
		return state.Attempt{}, state.Environment{}, state.ErrNotFound
	}
	return state.Attempt{ID: id}, state.Environment{ID: "myproject:123"}, nil
}
func (s *stubService) DeleteEnvironment(id string) error {
	s.deletes++
	return nil
}

func TestDeleteRequiresBearerToken(t *testing.T) {
	service := &stubService{}
	handler := Handler(service, "shared-secret")
	request := httptest.NewRequest(http.MethodDelete, "/api/environments/myproject:123", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || service.deletes != 0 {
		t.Fatalf("unauthorized deletion: %d, %d", response.Code, service.deletes)
	}
	request.Header.Set("Authorization", "Bearer shared-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.deletes != 1 {
		t.Fatalf("authorized deletion: %d, %d", response.Code, service.deletes)
	}
}

func TestDeploymentRequestAuthenticationAndResponse(t *testing.T) {
	service := &stubService{}
	handler := Handler(service, "shared-secret")
	request := []byte(`{"project":"myproject","intent":123,"commit":"` + strings.Repeat("a", 40) + `"}`)
	for _, header := range []string{"", "Bearer wrong", "Bearer shared-secret extra"} {
		r := httptest.NewRequest(http.MethodPost, "/api/deployments", bytes.NewReader(request))
		r.Header.Set("Authorization", header)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || service.submits != 0 {
			t.Fatalf("header %q: status %d, submits %d", header, w.Code, service.submits)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/deployments", bytes.NewReader(request))
	r.Header.Set("Authorization", "Bearer shared-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || service.submits != 1 {
		t.Fatalf("accepted status %d, submits %d: %s", w.Code, service.submits, w.Body.String())
	}
	var response struct{ ID, Status, URL string }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "deployment-1" || response.Status != "queued" || response.URL != "https://myproject-123.preview.example.com" {
		t.Fatalf("response = %+v", response)
	}
}

func TestMalformedRequestsDoNotSubmit(t *testing.T) {
	service := &stubService{}
	handler := Handler(service, "shared-secret")
	for _, body := range []string{`{"unknown":true}`, `{}`, `{"project":"x"} {}`, `[`} {
		r := httptest.NewRequest(http.MethodPost, "/api/deployments", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer shared-secret")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status %d", body, w.Code)
		}
	}
	if service.submits != 0 {
		t.Fatalf("submits = %d", service.submits)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/deployments", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer shared-secret")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType || service.submits != 0 {
		t.Fatalf("wrong content type: status %d, submits %d", w.Code, service.submits)
	}
}

func TestReadOnlyStatus(t *testing.T) {
	handler := Handler(&stubService{}, "shared-secret")
	for path, want := range map[string]int{
		"/api/environments":               http.StatusOK,
		"/api/environments/myproject:123": http.StatusOK,
		"/api/environments/missing:1":     http.StatusNotFound,
		"/api/deployments/deployment-1":   http.StatusOK,
		"/api/deployments/other":          http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d", path, w.Code, want)
		}
	}
}
