package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateComposeSubset(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	safe := `services:
  web:
    build: .
    environment:
      DATABASE_URL: postgres://db/app
    depends_on:
      db:
        condition: service_healthy
    volumes:
      - ./data:/app/data:ro
  db:
    image: postgres:17
    volumes:
      - dbdata:/var/lib/postgresql/data
    healthcheck:
      test: [CMD, pg_isready]
volumes:
  dbdata:
`
	name := filepath.Join(checkout, "compose.yaml")
	if err := os.WriteFile(name, []byte(safe), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompose(name, checkout, "web"); err != nil {
		t.Fatalf("safe Compose rejected: %v", err)
	}

	tests := []struct {
		name, body string
	}{
		{"privileged", "services:\n  web:\n    image: alpine\n    privileged: true\n"},
		{"provider", "services:\n  web:\n    image: alpine\n    provider:\n      type: dangerous\n"},
		{"include", "include: /etc/secret.yaml\nservices:\n  web:\n    image: alpine\n"},
		{"published port", "services:\n  web:\n    image: alpine\n    ports: ['8080:80']\n"},
		{"interpolation", "services:\n  web:\n    image: alpine\n    environment:\n      SECRET: ${HOST_SECRET}\n"},
		{"external mount", "services:\n  web:\n    image: alpine\n    volumes: ['/etc:/host:ro']\n"},
		{"duplicate key", "services:\n  web:\n    image: alpine\n    image: postgres\n"},
		{"anchor", "services:\n  web: &web\n    image: alpine\n"},
		{"undeclared volume", "services:\n  web:\n    image: alpine\n    volumes: ['data:/app/data']\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(name, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCompose(name, checkout, "web"); err == nil {
				t.Fatal("unsafe Compose accepted")
			}
		})
	}
}

func TestValidateComposeRejectsSymlinkEscape(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(checkout, "linked")); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(checkout, "compose.yaml")
	body := "services:\n  web:\n    image: alpine\n    volumes:\n      - ./linked:/outside\n"
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompose(name, checkout, "web"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape result = %v", err)
	}
}

func TestValidateComposeRejectsComposeFileOutsideCheckout(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("services:\n  web:\n    image: alpine\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompose(filepath.Join(checkout, "linked", "compose.yaml"), checkout, "web"); err == nil {
		t.Fatal("Compose file outside checkout accepted")
	}
}
