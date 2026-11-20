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

type stubRouter struct{ port int }

func (s stubRouter) ActivePort(project string, intent int64) (int, error) {
	if project != "myproject" || intent != 123 || s.port == 0 {
		return 0, state.ErrNotFound
	}
	return s.port, nil
}

func TestRoutesOnlyConfiguredStableHost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "123-myproject.preview.example.com" || r.URL.Path != "/hello" || r.Header.Get("X-Forwarded-Proto") != "https" {
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
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"myproject": {}}}
	handler := Handler(cfg, stubRouter{port: port})
	for _, host := range []string{"123-other.preview.example.com", "0123-myproject.preview.example.com", "123-myproject.preview.example.com.evil.test", "123-myproject.preview.example.com:443", "123-myproject.other.example.com"} {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/hello", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("host %q returned %d", host, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://123-myproject.preview.example.com/hello", nil)
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
