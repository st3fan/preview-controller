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
	port    int
	status  state.Status
	attempt state.Attempt
}

func (s stubRouter) PreviewDeployment(project string, intent int64) (state.Attempt, state.Status, error) {
	if project != "my-project-7" || intent != 123 || (s.port == 0 && s.status == "") {
		return state.Attempt{}, "", state.ErrNotFound
	}
	attempt := s.attempt
	attempt.Port = s.port
	return attempt, s.status, nil
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

func TestInspectorInjection(t *testing.T) {
	const page = `<!doctype html><html><body><script>const marker = "</body>";</script><p>Preview</p></body></html>`
	const title = "</script><b>Ticket title</b>"
	for _, tc := range []struct {
		name, destination, title string
		enabled, want            bool
		commitDigit              string
	}{
		{"document with title", "document", title, true, true, "a"},
		{"older deployment", "document", "", true, true, "a"},
		{"replacement deployment", "document", title, true, true, "b"},
		{"embedded document", "iframe", title, true, false, "a"},
		{"HTML fetched by API client", "", title, true, false, "a"},
		{"opted out", "document", title, false, false, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Content-Security-Policy", "default-src 'self'")
				w.Header().Set("ETag", `"original"`)
				_, _ = w.Write([]byte(page))
			}))
			defer backend.Close()
			port, err := strconv.Atoi(backend.URL[strings.LastIndexByte(backend.URL, ':')+1:])
			if err != nil {
				t.Fatal(err)
			}
			attempt := state.Attempt{Port: port, Commit: strings.Repeat(tc.commitDigit, 40), IssueTitle: tc.title}
			cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{
				"my-project-7": {Repository: "https://github.com/example/project.git", PreviewInspector: tc.enabled},
			}}
			request := httptest.NewRequest(http.MethodGet, "http://my-project-7-123.preview.example.com/", nil)
			request.Header.Set("Sec-Fetch-Dest", tc.destination)
			request.Header.Set("Sec-Fetch-Mode", "navigate")
			response := httptest.NewRecorder()
			Handler(cfg, stubRouter{port: port, attempt: attempt}).ServeHTTP(response, request)
			body := response.Body.String()
			if got := response.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
				t.Fatalf("CSP = %q", got)
			}
			if !tc.want {
				if body != page || response.Header().Get("ETag") != `"original"` {
					t.Fatal("response was transformed")
				}
				return
			}
			for _, want := range []string{
				`<p>Preview</p><script>`,
				`"Repository":"https://github.com/example/project"`,
				`"IssueURL":"https://github.com/example/project/issues/123"`,
				`"CommitURL":"https://github.com/example/project/commit/` + attempt.Commit + `"`,
				`window.self !== window.top`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
			label := `"Issue":"#123"`
			if tc.title != "" {
				label = `"Issue":"#123 \u003c/script\u003e\u003cb\u003eTicket title\u003c/b\u003e"`
			}
			if !strings.Contains(body, label) || strings.Contains(body, title) {
				t.Errorf("expected safe issue label %q", label)
			}
			if response.Header().Get("ETag") != "" || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
				t.Error("transformed response has incorrect cache or length headers")
			}
		})
	}
}
