package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeFailureOutput(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fake := "#!/bin/sh\nprintf '%09000d' 0\nprintf 'last build error\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, confidential := range []bool{false, true} {
		output, err := runner.runDeploymentStep(context.Background(), confidential, "compose", "build")
		if err == nil || strings.Contains(err.Error(), "last build error") {
			t.Fatalf("public-facing command error = %v", err)
		}
		if confidential && output != "" {
			t.Fatal("confidential Compose output was captured")
		}
		if !confidential && (len(output) != diagnosticOutputLimit || !strings.HasSuffix(output, "last build error\n")) {
			t.Fatalf("unexpected diagnostic tail of %d bytes", len(output))
		}
	}
}
