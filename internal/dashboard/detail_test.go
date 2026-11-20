package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/githubmeta"
	"github.com/st3fan/preview-controller/internal/state"
)

func TestPreviewDetail(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterDetail(mux, config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{
		"alpha": {Repository: "https://github.com/example/alpha.git"},
	}}, store.Load)
	acceptedAt := time.Date(2026, 11, 20, 12, 29, 0, 0, time.UTC)
	activatedAt := acceptedAt.Add(42 * time.Second)
	env := state.Environment{
		ID: state.ID("alpha", 18), Project: "alpha", Intent: 18, URL: "https://alpha-18.preview.example.com", ActiveDeploymentID: "active",
		Attempts: []state.Attempt{{ID: "active", Commit: fmt.Sprintf("%040x", 100), Status: state.Healthy, ComposeProject: "running", CreatedAt: acceptedAt, ActivatedAt: &activatedAt}},
	}
	for i := 1; i <= 11; i++ {
		env.Attempts = append(env.Attempts, state.Attempt{
			ID: fmt.Sprintf("attempt-%d", i), Commit: fmt.Sprintf("%040x", i), Status: state.Failed,
			CreatedAt: time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC),
		})
	}
	env.Attempts[10].Status = state.Retired
	env.Attempts[10].ActivatedAt = new(env.Attempts[10].CreatedAt.Add(23 * time.Second))
	env.Diagnostics = map[string]state.Diagnostic{
		"attempt-11": {Stage: state.BuildStage, Summary: "Build failed.", Output: "<script>alert(1)</script>"},
	}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	path := server.URL + "/dashboard/previews/alpha/"
	page := readPage(t, server.Client(), path+"18", http.StatusOK)
	requireText(t, page, env.URL, "https://github.com/example/alpha/issues/18", "alpha #18",
		env.Attempts[0].Commit, "/dashboard/logs/alpha/18/active", "2026-11-20T12:30:00Z", "Compose build", "Build failed.", "&lt;script&gt;alert(1)&lt;/script&gt;")
	requireText(t, page, "2026-11-20T12:29:00Z", "2026-11-20T12:29:42Z", "Time to preview 42s",
		"2026-11-20T12:30:23Z", "Time to preview 23s", "Timing unavailable")
	if strings.Count(page, "Time to preview") != 2 {
		t.Fatalf("unexpected attempt timings: %s", page)
	}
	if strings.Contains(page, "<script>alert") {
		t.Fatal("diagnostics rendered as markup")
	}
	_, recent, _ := strings.Cut(page, "<h2>Recent attempts</h2>")
	if strings.Contains(recent, env.Attempts[1].Commit) || strings.Count(recent, "<article ") != 10 ||
		strings.Index(recent, env.Attempts[11].Commit) >= strings.Index(recent, env.Attempts[2].Commit) {
		t.Fatalf("unexpected recent attempt history: %s", page)
	}
	for _, missing := range []string{"0", "unknown", "19"} {
		readPage(t, server.Client(), path+missing, http.StatusNotFound)
	}
}

func TestDetailGitHubMetadata(t *testing.T) {
	cfg := config.Config{
		GitHubTokenEnv: "PREVIEW_GITHUB_TOKEN",
		Projects:       map[string]config.Project{"alpha": {Repository: "https://github.com/example/alpha.git/"}},
	}
	opened := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	commit := strings.Repeat("a", 40)
	env := state.Environment{Project: "alpha", Intent: 51, Attempts: []state.Attempt{
		{ID: "first", IssueTitle: "Deployment title", Commit: commit, CreatedAt: opened.Add(4 * time.Minute),
			ActivatedAt: new(opened.Add(5 * time.Minute)), Status: state.Retired},
		{ID: "retry", Commit: commit, CreatedAt: opened.Add(4 * time.Minute),
			UpdatedAt: opened.Add(6 * time.Minute), Status: state.Failed},
		{ID: "legacy", Status: state.Failed},
	}}
	load := func(string, int64) (state.Environment, error) { return env, nil }
	mux := http.NewServeMux()
	registerDetail(mux, cfg, load, func(_ context.Context, repository string, intent int64) githubmeta.Result {
		if repository != "example/alpha" || intent != 51 {
			t.Fatalf("unexpected lookup %s #%d", repository, intent)
		}
		var earlier githubmeta.Pull
		if err := json.Unmarshal([]byte(`{
			"number":56, "title":"Earlier PR", "createdAt":"2026-11-20T12:01:00Z",
			"reviews":{"nodes":[{"submittedAt":"2026-11-20T12:02:00Z","url":"https://github.com/example/alpha/pull/56#pullrequestreview-42"}, {}]},
			"commits":{"pageInfo":{"hasPreviousPage":true},"nodes":[{"commit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","committedDate":"2026-11-20T12:00:30Z"}}]}
		}`), &earlier); err != nil {
			t.Fatal(err)
		}
		return githubmeta.Result{
			Intent:  &githubmeta.Issue{Number: 51, Title: "<script>live title</script>", State: "OPEN", CreatedAt: opened},
			Related: []githubmeta.Issue{{Number: 52, Title: "Related work", State: "CLOSED"}},
			Pulls: []githubmeta.Pull{
				{Number: 57, Title: "<b>Pull request</b>", State: "MERGED",
					CreatedAt: opened.Add(3 * time.Minute), MergedAt: new(opened.Add(7 * time.Minute))},
				earlier,
			},
			Partial: true,
		}
	})
	request := httptest.NewRequest(http.MethodGet, "/dashboard/previews/alpha/51", nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, request)
	page := resp.Body.String()
	if resp.Code != http.StatusOK || strings.Contains(page, "Deployment title") || strings.Contains(page, "<script>live title</script>") {
		t.Fatalf("unexpected GitHub detail page: %s", page)
	}
	requireText(t, page, "&lt;script&gt;live title&lt;/script&gt;", "https://github.com/example/alpha/issues/52",
		"https://github.com/example/alpha/pull/57", "&lt;b&gt;Pull request&lt;/b&gt;", "MERGED", "CLOSED",
		"Stack order unavailable.", "GitHub returned incomplete results.", "#activity")

	_, timeline, _ := strings.Cut(page, "<h2 id=\"activity\">Activity timeline</h2>")
	timeline, _, _ = strings.Cut(timeline, "</section>")
	requireText(t, timeline, "GitHub activity is incomplete.", "Controller deployment activity is incomplete.", "GitHub", "Controller",
		"/pull/56#pullrequestreview-42", "/pull/56/commits/"+commit)
	for _, event := range []string{"Issue #51 opened", "PR #56 commit recorded", "PR #56 opened", "PR #56 review submitted", "PR #57 opened",
		"Deployment requested</a> · first", "Deployment requested</a> · retry",
		"Preview activated", "Deployment failed", "PR #57 merged"} {
		_, remaining, found := strings.Cut(timeline, event)
		if !found {
			t.Fatalf("missing or out-of-order event %q", event)
		}
		timeline = remaining
	}

	mux = http.NewServeMux()
	registerDetail(mux, cfg, load, func(context.Context, string, int64) githubmeta.Result { return githubmeta.Result{} })
	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, request)
	if resp.Code != http.StatusOK {
		t.Fatalf("fallback returned HTTP %d", resp.Code)
	}
	requireText(t, resp.Body.String(), "Deployment title", "GitHub metadata unavailable.", "GitHub activity is unavailable.",
		"Deployment requested", "Preview activated", "Deployment failed", "Time to preview 1m0s")
}
