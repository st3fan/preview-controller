package dashboard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/state"
)

func TestDashboard(t *testing.T) {
	const active = "0123456789abcdef0123456789abcdef01234567"
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shutdown, stop := context.WithCancel(context.Background())
	mux := http.NewServeMux()
	Register(mux, shutdown, store.List, store.Changes)
	server := httptest.NewUnstartedServer(mux)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	defer server.Close()
	defer stop()
	client := server.Client()
	client.Timeout = 5 * time.Second

	page := readPage(t, client, server.URL+"/", http.StatusOK)
	requireText(t, page, "No preview environments yet.", `sse-connect="/dashboard/events"`, `sse-swap="environments"`)
	for _, name := range []string{"htmx-2.0.11.min.js", "sse-2.2.4.js", "dashboard.css"} {
		requireText(t, page, "/dashboard/assets/"+name)
		readPage(t, client, server.URL+"/dashboard/assets/"+name, http.StatusOK)
	}
	readPage(t, client, server.URL+"/missing", http.StatusNotFound)

	stream, err := client.Get(server.URL + "/dashboard/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if got := stream.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("unexpected stream content type %q", got)
	}
	events := bufio.NewScanner(stream.Body)
	requireText(t, readEvent(t, events), "No preview environments yet.")
	// Changes after the server's normal write timeout must still be delivered.
	time.Sleep(150 * time.Millisecond)
	env := state.Environment{
		ID:           state.ID("alpha", 18),
		Project:      "alpha",
		Intent:       18,
		URL:          "https://18-alpha.preview.example.com",
		ActiveCommit: active,
		Attempts: []state.Attempt{
			{Status: state.Healthy},
			{Status: state.Failed, Error: "private command output", Workspace: "/private/workspace"},
		},
	}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	event := readEvent(t, events)
	requireText(t, event, "<h2>alpha</h2>", "#18", env.URL, active, ">failed<")

	env.Intent = 19
	env.ID = state.ID(env.Project, env.Intent)
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	event = readEvent(t, events)
	requireText(t, event, "#18", "#19")
	if strings.Count(event, "<h2>alpha</h2>") != 1 {
		t.Fatalf("expected one project group in %s", event)
	}

	for _, body := range []string{event, readPage(t, client, server.URL+"/", http.StatusOK)} {
		for _, sensitive := range []string{"private command output", "/private/workspace"} {
			if strings.Contains(body, sensitive) {
				t.Fatalf("dashboard exposed %q", sensitive)
			}
		}
	}

	reconnected, err := client.Get(server.URL + "/dashboard/events")
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Body.Close()
	reconnectEvents := bufio.NewScanner(reconnected.Body)
	requireText(t, readEvent(t, reconnectEvents), "#18", "#19", active)
	stop()
	if reconnectEvents.Scan() || reconnectEvents.Err() != nil {
		t.Fatalf("stream did not close cleanly on shutdown: %v", reconnectEvents.Err())
	}
}

func TestUnavailableDashboard(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, t.Context(), func() ([]state.Environment, error) {
		return nil, errors.New("private state path")
	}, nil)
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	requireText(t, page.Body.String(), "Could not load preview environments.")
	if page.Code != http.StatusServiceUnavailable || strings.Contains(page.Body.String(), "private state path") {
		t.Fatalf("unexpected unavailable response: %d %s", page.Code, page.Body)
	}
}

func readPage(t *testing.T, client *http.Client, url string, status int) string {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("GET %s: got %d, want %d", url, response.StatusCode, status)
	}
	return string(body)
}

func readEvent(t *testing.T, events *bufio.Scanner) string {
	t.Helper()
	var event strings.Builder
	for events.Scan() {
		line := events.Text()
		if line == "" {
			message := event.String()
			requireText(t, message, "event: environments\n")
			return message
		}
		event.WriteString(line)
		event.WriteByte('\n')
	}
	t.Fatalf("stream ended before receiving an event: %v", events.Err())
	return ""
}

func requireText(t *testing.T, body string, snippets ...string) {
	t.Helper()
	for _, snippet := range snippets {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected %q in %s", snippet, body)
		}
	}
}
