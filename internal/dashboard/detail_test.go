package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/config"
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
	env := state.Environment{
		ID: state.ID("alpha", 18), Project: "alpha", Intent: 18, URL: "https://alpha-18.preview.example.com", ActiveDeploymentID: "active",
		Attempts: []state.Attempt{{ID: "active", Commit: fmt.Sprintf("%040x", 100), Status: state.Healthy, ComposeProject: "running"}},
	}
	for i := 1; i <= 11; i++ {
		env.Attempts = append(env.Attempts, state.Attempt{
			ID: fmt.Sprintf("attempt-%d", i), Commit: fmt.Sprintf("%040x", i), Status: state.Failed,
			CreatedAt: time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC),
		})
	}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	path := server.URL + "/dashboard/previews/alpha/"
	page := readPage(t, server.Client(), path+"18", http.StatusOK)
	requireText(t, page, env.URL, "https://github.com/example/alpha/issues/18", "alpha #18",
		env.Attempts[0].Commit, "/dashboard/logs/alpha/18/active", "2026-11-20T12:30:00Z")
	if strings.Contains(page, env.Attempts[1].Commit) || strings.Count(page, "<article>") != 10 ||
		strings.Index(page, env.Attempts[11].Commit) >= strings.Index(page, env.Attempts[2].Commit) {
		t.Fatalf("unexpected recent attempt history: %s", page)
	}
	for _, missing := range []string{"0", "unknown", "19"} {
		readPage(t, server.Client(), path+missing, http.StatusNotFound)
	}
}
