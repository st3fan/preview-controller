package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewRejectsSymlinkRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root); err == nil {
		t.Fatal("symlink workspace accepted")
	}
}

func TestResolvedComposePolicy(t *testing.T) {
	checkout := t.TempDir()
	inside := filepath.Join(checkout, "data")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	model := map[string]any{"services": map[string]any{
		"web": map[string]any{"ports": []any{map[string]any{"host_ip": "127.0.0.1", "target": 8080, "protocol": "tcp"}}, "volumes": []any{map[string]any{"type": "bind", "source": inside}}},
		"db":  map[string]any{"volumes": []any{map[string]any{"type": "volume", "source": "dbdata"}}},
	}}
	check := func() error {
		data, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		return validateResolved(data, "web", 8080, checkout)
	}
	if err := check(); err != nil {
		t.Fatalf("safe resolved model rejected: %v", err)
	}
	web := model["services"].(map[string]any)["web"].(map[string]any)
	web["ports"] = []any{map[string]any{"host_ip": "0.0.0.0", "target": 8080, "protocol": "tcp"}}
	if err := check(); err == nil {
		t.Fatal("public bind accepted")
	}
	web["ports"] = []any{map[string]any{"host_ip": "127.0.0.1", "target": 8080, "protocol": "tcp"}}
	web["volumes"] = []any{map[string]any{"type": "bind", "source": t.TempDir()}}
	if err := check(); err == nil {
		t.Fatal("external bind mount accepted")
	}
	delete(web, "volumes")
	model["services"].(map[string]any)["db"].(map[string]any)["privileged"] = true
	if err := check(); err == nil {
		t.Fatal("privileged dependency accepted")
	}
}

func TestProbeHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthy", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/healthy", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, failure string }{
		{"/healthy", ""},
		{"/redirect", "HTTP 302"},
		{"/missing", "HTTP 404"},
		{"/slow", "health check timed out"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ctx := t.Context()
			if tc.path == "/slow" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			err := runner.ProbeHealth(ctx, port, tc.path)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.failure {
				t.Fatalf("probe error = %q, want %q", got, tc.failure)
			}
		})
	}
}
