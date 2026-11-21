package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

type fixtureRuntime struct{ failStartup atomic.Bool }

func (*fixtureRuntime) Verify(context.Context, string, string) error { return nil }
func (f *fixtureRuntime) Run(_ context.Context, _ string, _ int64, _ config.Project, a state.Attempt, progress runtime.Progress) (runtime.Result, error) {
	if err := progress(state.Building, runtime.Result{}); err != nil {
		return runtime.Result{}, err
	}
	if strings.HasPrefix(a.Commit, "b") {
		return runtime.Result{Diagnostic: &state.Diagnostic{Stage: state.BuildStage, Summary: "Fixture build failed", Output: "fixture compiler error"}}, errors.New("fixture build failed")
	}
	result := runtime.Result{Port: 12345}
	if err := progress(state.Starting, result); err != nil {
		return result, err
	}
	if f.failStartup.Load() {
		result.Diagnostic = &state.Diagnostic{Stage: state.StartStage, Summary: "startup failed", ServiceOutput: "startup marker " + a.ID}
		return result, errors.New("startup failed")
	}
	return result, nil
}
func (*fixtureRuntime) CleanupFailed(context.Context, string, int64, config.Project, state.Attempt, *state.Diagnostic) error {
	return nil
}
func (*fixtureRuntime) Cleanup(context.Context, string, int64, state.Attempt) error { return nil }
func (*fixtureRuntime) HealthyPort(_ context.Context, a state.Attempt, _ config.Project) (int, error) {
	return a.Port, nil
}

type tokenTransport struct {
	http.RoundTripper
	token string
	host  string
}

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	if t.host != "" {
		req.Host = t.host
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.RoundTripper.RoundTrip(req)
}
func TestMCPWorkflow(t *testing.T) {
	t.Run("shared bearer", func(t *testing.T) { testMCPWorkflow(t, nil, "fixture") })
	t.Run("Pocket ID operator", func(t *testing.T) {
		issuer := newFixtureIssuer(t)
		testMCPWorkflow(t, &issuer.cfg, issuer.accessToken(t))
	})
}

func testMCPWorkflow(t *testing.T, oauth *config.MCPOAuth, token string) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"demo": {Repository: "https://github.com/example/demo.git"}}}
	runner := new(fixtureRuntime)
	service, err := deploy.New(cfg, store, runner, runner)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	mux := http.NewServeMux()
	var host string
	if oauth != nil {
		host = "preview.test"
	}
	RegisterMCP(mux, service, cfg.Projects, "fixture", oauth, func(_ context.Context, a state.Attempt) (string, error) { return "output for " + a.ID, nil })
	for _, tc := range []struct {
		auth, origin string
		status       int
	}{{"", "", 401}, {"Bearer wrong", "", 401}, {"Bearer " + token, "https://other.example", 403}} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Authorization", tc.auth)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("access check: got %d, want %d", w.Code, tc.status)
		}
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{server.Client().Transport, token, host}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	instructions := session.InitializeResult().Instructions
	if !strings.Contains(instructions, "returned ID to report healthy") || !strings.Contains(instructions, "confirm active_commit matches") {
		t.Fatal("missing readiness instructions")
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 7 {
		t.Fatalf("tools %v, %v", tools, err)
	}
	call := func(name string, args any, target any) {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s: %v", name, result.Content)
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var projects projectList
	call("list_projects", struct{}{}, &projects)
	if len(projects.Projects) != 1 || projects.Projects[0] != (projectInfo{"demo", cfg.Projects["demo"].Repository}) {
		t.Fatal(projects)
	}
	preview := previewInput{"demo", 54}
	var previews previewList
	call("list_previews", struct{}{}, &previews)
	if len(previews.Previews) != 0 {
		t.Fatal(previews)
	}
	good := strings.Repeat("a", 40)
	var healthyID string
	for _, tc := range []struct {
		request string
		sha     string
		status  state.Status
	}{{"first", good, state.Healthy}, {"build-failure", strings.Repeat("b", 40), state.Failed}, {"same-sha-startup-failure", good, state.Failed}} {
		runner.failStartup.Store(tc.request == "same-sha-startup-failure")
		args := submitInput{preview, tc.sha, tc.request}
		var submitted, retried deploymentView
		call("deploy_preview", args, &submitted)
		call("deploy_preview", args, &retried)
		if submitted.ID == "" || retried.ID != submitted.ID {
			t.Fatalf("retry created another attempt: %+v %+v", submitted, retried)
		}
		id := deploymentInput{submitted.ID}
		for {
			var current deploymentView
			call("get_deployment", id, &current)
			if current.Status == tc.status {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(current)
			case <-time.After(time.Millisecond * 10):
			}
		}
		var current previewView
		call("get_preview", preview, &current)
		if current.ActiveCommit != good || current.Latest == nil || current.Latest.Status != tc.status || current.Latest.ID != submitted.ID || current.Latest.Commit != tc.sha || current.URL != "https://demo-54.preview.example.com" {
			t.Fatal(current)
		}
		var logs logResult
		call("get_deployment_logs", id, &logs)
		if tc.request == "same-sha-startup-failure" {
			if submitted.ID == healthyID || logs.ServiceStatus != "available" || logs.ServiceOutput != "startup marker "+submitted.ID {
				t.Fatal(submitted.ID, logs)
			}
			var serving deploymentView
			call("get_deployment", deploymentInput{healthyID}, &serving)
			if serving.Status != state.Healthy || serving.Commit != tc.sha {
				t.Fatal(serving)
			}
		} else if tc.status == state.Failed {
			if logs.Diagnostic == nil || logs.Diagnostic.Output != "fixture compiler error" || logs.ServiceStatus != "unavailable" {
				t.Fatal(logs)
			}
		} else {
			healthyID = submitted.ID
			if logs.ServiceOutput != "output for "+submitted.ID {
				t.Fatal(logs)
			}
		}
	}
	var removed removal
	call("remove_preview", preview, &removed)
	if !removed.Removed {
		t.Fatal(removed)
	}
	call("list_previews", struct{}{}, &previews)
	if len(previews.Previews) != 0 {
		t.Fatal(previews)
	}
}
