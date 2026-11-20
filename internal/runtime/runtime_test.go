package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
