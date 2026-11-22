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
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/state"
)

func TestPreviewDetail(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerDetailPage(mux, newHostSampler(""), config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{
		"alpha": {Repository: "https://github.com/example/alpha.git"},
	}}, store.Load, health.New(store, nil, nil))
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
	requireText(t, page, "2026-11-20T12:29:00Z", "2026-11-20T12:29:42Z", "<dt>Time to preview</dt><dd>42s</dd>",
		"2026-11-20T12:30:23Z", "<dt>Time to preview</dt><dd>23s</dd>", "Timing unavailable")
	if strings.Count(page, "<dt>Activated</dt>") != 2 {
		t.Fatalf("unexpected attempt timings: %s", page)
	}
	if strings.Contains(page, "<script>alert") {
		t.Fatal("diagnostics rendered as markup")
	}
	_, recent, _ := strings.Cut(page, "<h2>Recent attempts</h2>")
	recent, _, _ = strings.Cut(recent, "</section>")
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
		Projects: map[string]config.Project{"alpha": {Repository: "https://github.com/example/alpha.git/"}},
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
	partial := true
	registerDetail(mux, newHostSampler(""), cfg, load, health.New(nil, nil, nil), func(_ context.Context, repository string, intent int64) githubmeta.Result {
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
		var issue githubmeta.Issue
		if err := json.Unmarshal([]byte(`{
			"number":51,"title":"<script>live title</script>","state":"CLOSED","stateReason":"COMPLETED",
			"createdAt":"2026-11-20T12:00:00Z","closedAt":"2026-11-20T12:08:00Z",
			"timelineItems":{"pageInfo":{"hasPreviousPage":true},"nodes":[
				{"__typename":"ClosedEvent","createdAt":"2026-11-20T12:02:00Z","url":"https://github.com/example/alpha/issues/51#event-123"},
				{"__typename":"ReopenedEvent","createdAt":"2026-11-20T12:03:30Z"},
				{"__typename":"ClosedEvent","createdAt":"2026-11-20T12:08:01Z","url":"https://example.com/example/alpha/issues/51#event-124"},
				{"__typename":"ReopenedEvent"}
			]}
		}`), &issue); err != nil {
			t.Fatal(err)
		}
		return githubmeta.Result{
			Intent:  &issue,
			Related: []githubmeta.Issue{{Number: 52, Title: "Related work", State: "CLOSED"}},
			Pulls: []githubmeta.Pull{
				{Number: 57, Title: "<b>Pull request</b>", State: "MERGED",
					CreatedAt: opened.Add(3 * time.Minute), MergedAt: new(opened.Add(7 * time.Minute))},
				earlier,
			},
			Partial: partial,
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
		"Stack order unavailable.", "GitHub returned incomplete results.", "#activity",
		"<dt>Elapsed time to first linked PR</dt><dd>Timing unavailable</dd>", "<dt>Elapsed time to merge</dt><dd>4m0s</dd>")

	_, timeline, _ := strings.Cut(page, "<h2 id=\"activity\">Activity timeline</h2>")
	timeline, _, _ = strings.Cut(timeline, "</section>")
	requireText(t, timeline, "GitHub activity is incomplete.", "Controller deployment activity is incomplete.", "GitHub", "Controller",
		"/pull/56#pullrequestreview-42", "/pull/56/commits/"+commit, "/issues/51#event-123")
	if strings.Contains(timeline, "#event-124") || strings.Count(timeline, "Issue #51 reopened") != 1 {
		t.Fatal("timeline rendered an untrusted link or a reopen without a timestamp")
	}
	for _, event := range []string{"Issue #51 opened", "PR #56 commit recorded", "PR #56 opened", "Issue #51 closed", "PR #56 review submitted", "PR #57 opened", "Issue #51 reopened",
		"Deployment requested</a> · first", "Deployment requested</a> · retry",
		"Preview activated", "Deployment failed", "PR #57 merged", "Issue #51 closed"} {
		_, remaining, found := strings.Cut(timeline, event)
		if !found {
			t.Fatalf("missing or out-of-order event %q", event)
		}
		timeline = remaining
	}

	partial = false
	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, request)
	if resp.Code != http.StatusOK {
		t.Fatalf("complete history returned HTTP %d", resp.Code)
	}
	requireText(t, resp.Body.String(), "<dt>Intent opened</dt>", "<dt>First linked PR opened</dt>",
		"<dt>Elapsed time to first linked PR</dt><dd>1m0s</dd>", "<dt>Intent closed as completed</dt>",
		"<dt>Elapsed time from Intent opening to completed closure</dt><dd>8m0s</dd>")

	mux = http.NewServeMux()
	registerDetail(mux, newHostSampler(""), cfg, load, health.New(nil, nil, nil), func(context.Context, string, int64) githubmeta.Result { return githubmeta.Result{} })
	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, request)
	if resp.Code != http.StatusOK {
		t.Fatalf("fallback returned HTTP %d", resp.Code)
	}
	requireText(t, resp.Body.String(), "Deployment title", "GitHub metadata unavailable.", "GitHub activity is unavailable.",
		"Elapsed time to first linked PR is unavailable.",
		"Deployment requested", "Preview activated", "Deployment failed", "<dt>Time to preview</dt><dd>1m0s</dd>")
}

func TestArchivedDetailKeepsServingRevision(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cfg := config.Config{PreviewDomain: "preview.example.com", Projects: map[string]config.Project{"demo": {Repository: "https://github.com/example/demo"}}}
	archived := time.Now()
	env := state.Environment{Project: "demo", Intent: 12, ActiveCommit: sha, ActiveDeploymentID: "serving", DeleteAt: &archived, Attempts: []state.Attempt{
		{ID: "serving", Commit: sha, Status: state.Healthy},
		{ID: "replacement", Commit: strings.Repeat("f", 40), Status: state.Failed},
	}}
	mux := http.NewServeMux()
	registerDetailPage(mux, newHostSampler(""), cfg, func(string, int64) (state.Environment, error) { return env, nil }, health.New(nil, cfg.Projects, nil))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dashboard/previews/demo/12", nil))
	body := response.Body.String()
	requireText(t, body, "Archived deployment", "Archived commit", ">Offline<", `href="https://github.com/example/demo/commit/`+sha+`" target="_blank"`, ">Failed<")
	if strings.Contains(body, "Open preview") || strings.Contains(body, "/dashboard/logs/") {
		t.Fatal("archived detail offers live navigation")
	}
}
