package ingress

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

type stubRouter struct {
	port   int
	status state.Status
}

func (s stubRouter) PreviewRoute(project string, intent int64) (int, state.Status, error) {
	if project != "my-project-7" || intent != 123 || (s.port == 0 && s.status == "") {
		return 0, "", state.ErrNotFound
	}
	return s.port, s.status, nil
}

func TestRoutesOnlyConfiguredStableHost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "my-project-7-123.preview.example.com" || r.URL.Path != "/hello" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("forwarded request: host %q, path %q, proto %q", r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto"))
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"my-project-7": {}}}
	handler := Handler(cfg, stubRouter{port: port, status: state.Failed})
	for _, host := range []string{
		"other-123.preview.example.com",
		"my-project-7-0123.preview.example.com",
		"123-my-project-7.preview.example.com",
		"my-project-7-123.preview.example.com.evil.test",
		"my-project-7-123.preview.example.com:443",
		"my-project-7-123.other.example.com",
	} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/hello", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("host %q returned %d", host, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://my-project-7-123.preview.example.com/hello", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Fatalf("valid preview returned %d: %s", w.Code, w.Body.String())
	}
	missing := Handler(cfg, stubRouter{})
	w = httptest.NewRecorder()
	missing.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("inactive preview returned %d", w.Code)
	}
}

func TestPreviewDeploymentStatus(t *testing.T) {
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"my-project-7": {}}}
	for _, tc := range []struct {
		status  state.Status
		message string
		refresh bool
	}{
		{state.Queued, "Queued for deployment.", true},
		{state.Fetching, "Fetching source code.", true},
		{state.Verified, "Source verified.", true},
		{state.Building, "Building the preview.", true},
		{state.Starting, "Starting the preview and checking its health.", true},
		{state.Failed, "Preview deployment failed", false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			handler := Handler(cfg, stubRouter{status: tc.status})
			r := httptest.NewRequest(http.MethodGet, "https://my-project-7-123.preview.example.com/hello?x=1", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status response: %d %v", w.Code, w.Header())
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.message) || strings.Contains(body, `http-equiv="refresh" content="3"`) != tc.refresh {
				t.Fatalf("unexpected status page: %s", body)
			}
		})
	}
}
