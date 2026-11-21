package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

type fixtureRuntime struct{}

func (fixtureRuntime) Verify(context.Context, string, string) error { return nil }
func (fixtureRuntime) Run(_ context.Context, _ string, _ int64, _ config.Project, a state.Attempt, progress runtime.Progress) (runtime.Result, error) {
	if err := progress(state.Building, runtime.Result{}); err != nil {
		return runtime.Result{}, err
	}
	if strings.HasPrefix(a.Commit, "b") {
		return runtime.Result{Diagnostic: &state.Diagnostic{Stage: state.BuildStage, Summary: "Fixture build failed", Output: "fixture compiler error"}}, errors.New("fixture build failed")
	}
	result := runtime.Result{Port: 12345}
	return result, progress(state.Starting, result)
}
func (fixtureRuntime) Cleanup(context.Context, string, int64, state.Attempt) error { return nil }
func (fixtureRuntime) HealthyPort(_ context.Context, a state.Attempt, _ config.Project) (int, error) {
	return a.Port, nil
}

type tokenTransport struct{ http.RoundTripper }

func (t tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Set("Authorization", "Bearer fixture")
	return t.RoundTripper.RoundTrip(req)
}
func TestMCPWorkflow(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"demo": {Repository: "https://github.com/example/demo.git"}}}
	service, err := deploy.New(cfg, store, fixtureRuntime{}, fixtureRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	mux := http.NewServeMux()
	mux.Handle("/mcp", MCPHandler(service, cfg.Projects, "fixture", func(_ context.Context, a state.Attempt) (string, error) { return "output for " + a.ID, nil }))
	for _, tc := range []struct {
		auth, origin string
		status       int
	}{{"", "", 401}, {"Bearer wrong", "", 401}, {"Bearer fixture", "https://other.example", 403}} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
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
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{server.Client().Transport}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if !strings.Contains(session.InitializeResult().Instructions, "confirm active_commit matches") {
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
	for _, tc := range []struct {
		sha    string
		status state.Status
	}{{good, state.Healthy}, {strings.Repeat("b", 40), state.Failed}} {
		args := submitInput{preview, tc.sha, "request-" + tc.sha}
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
		if current.ActiveCommit != good || current.Latest == nil || current.Latest.Commit != tc.sha || current.URL != "https://demo-54.preview.example.com" {
			t.Fatal(current)
		}
		var logs logResult
		call("get_deployment_logs", id, &logs)
		if tc.status == state.Failed {
			if logs.Diagnostic == nil || logs.Diagnostic.Output != "fixture compiler error" || logs.ServiceStatus != "unavailable" {
				t.Fatal(logs)
			}
		} else if logs.ServiceOutput != "output for "+submitted.ID {
			t.Fatal(logs)
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
