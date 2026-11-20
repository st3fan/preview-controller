//go:build linux && integration

package dashboard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

func TestDeploymentLogIsolation(t *testing.T) {
	runner, err := runtime.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start := func(marker string) state.Attempt {
		t.Helper()
		id := fmt.Sprintf("%032x", time.Now().UnixNano())
		attempt := state.Attempt{ID: "deployment-" + id, ComposeProject: "pv" + id, Status: state.Healthy}
		compose := fmt.Sprintf(`services:
  web:
    image: redis:7-alpine
    command:
      - sh
      - -c
      - |
        i=0
        while [ $$i -lt 105 ]; do echo "%s-$$i"; i=$$((i+1)); done
        while true; do sleep 1; echo "%s-live"; done
`, marker, marker)
		run := func(args ...string) ([]byte, error) {
			cmd := exec.Command("docker", append([]string{"compose", "-p", attempt.ComposeProject, "-f", "-"}, args...)...)
			cmd.Stdin = strings.NewReader(compose)
			return cmd.CombinedOutput()
		}
		t.Cleanup(func() {
			if out, err := run("down", "--timeout", "0"); err != nil {
				t.Errorf("clean up Compose fixture: %v: %s", err, out)
			}
		})
		if out, err := run("up", "-d", "--pull", "never"); err != nil {
			t.Fatalf("start Compose fixture: %v: %s", err, out)
		}
		return attempt
	}

	first := start("first")
	other := start("other")
	replacement := start("replacement")
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := state.Environment{ID: state.ID("demo", 1), Project: "demo", Intent: 1, ActiveDeploymentID: first.ID, Attempts: []state.Attempt{first}}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state.Environment{ID: state.ID("demo", 2), Project: "demo", Intent: 2, Attempts: []state.Attempt{other}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	finished := make(chan string, 4)
	RegisterLogs(mux, context.Background(), store.Load, func(ctx context.Context, attempt state.Attempt) (func(io.Writer) error, error) {
		follow, err := runner.OpenLogs(ctx, attempt)
		if err != nil {
			return nil, err
		}
		return func(w io.Writer) error {
			defer func() { finished <- attempt.ID }()
			return follow(w)
		}, nil
	})
	server := httptest.NewUnstartedServer(mux)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	defer server.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	connect := func(intent int, attempt state.Attempt) (*http.Response, *bufio.Scanner) {
		t.Helper()
		res, err := client.Get(fmt.Sprintf("%s/dashboard/logs/demo/%d/%s", server.URL, intent, attempt.ID))
		if err != nil {
			t.Fatal(err)
		}
		return res, bufio.NewScanner(res.Body)
	}
	next := func(scan *bufio.Scanner, marker string) {
		t.Helper()
		if !scan.Scan() || !strings.Contains(scan.Text(), "web") || !strings.Contains(scan.Text(), marker) {
			t.Fatalf("expected web log containing %q, got %q (%v)", marker, scan.Text(), scan.Err())
		}
	}

	a, aLogs := connect(1, first)
	defer a.Body.Close()
	b, bLogs := connect(2, other)
	defer b.Body.Close()
	next(aLogs, "first-")
	next(bLogs, "other-")
	for lines := 1; !strings.Contains(aLogs.Text(), "first-live"); lines++ {
		if lines > 100 {
			t.Fatal("log tail exceeded 100 lines")
		}
		next(aLogs, "first-")
	}

	env.ActiveDeploymentID = replacement.ID
	env.Attempts = append(env.Attempts, replacement)
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	c, cLogs := connect(1, replacement)
	defer c.Body.Close()
	next(cLogs, "replacement-")
	next(aLogs, "first-live")
	if err := a.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-finished:
		if id != first.ID {
			t.Fatalf("unexpected log follow stopped: %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not stop log follow")
	}
	env.Attempts[0].Status = state.Retired
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	retired, _ := connect(1, first)
	defer retired.Body.Close()
	if retired.StatusCode != http.StatusGone {
		t.Fatalf("retired deployment logs returned %d", retired.StatusCode)
	}
}
