package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig(root string) Config {
	return Config{
		ManagementListen:   "127.0.0.1:8080",
		PreviewListen:      "127.0.0.1:8081",
		PreviewDomain:      "preview.example.com",
		StateRoot:          root,
		WorkspaceRoot:      filepath.Join(filepath.Dir(root), "workspaces"),
		DeploymentTokenEnv: "PREVIEW_TEST_TOKEN",
		Projects: map[string]Project{
			"myproject": {
				Repository:    "https://github.com/example/myproject.git",
				ComposeFile:   "compose.preview.yaml",
				PublicService: "web",
				PublicPort:    8080,
				HealthPath:    "/healthz",
			},
		},
	}
}

func TestLoadAndURL(t *testing.T) {
	t.Setenv("PREVIEW_TEST_TOKEN", "secret-value")
	cfg := validConfig(filepath.Join(t.TempDir(), "state"))
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Token() != "secret-value" {
		t.Fatal("token environment variable was not loaded")
	}
	url, err := loaded.URL("myproject", 123)
	if err != nil || url != "https://myproject-123.preview.example.com" {
		t.Fatalf("URL = %q, %v", url, err)
	}
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("token was serialized into global settings")
	}
}

func TestValidateRejectsUnsafeSettings(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"public management listener", func(c *Config) { c.ManagementListen = "0.0.0.0:8080" }},
		{"public preview listener", func(c *Config) { c.PreviewListen = "0.0.0.0:8081" }},
		{"shared listeners", func(c *Config) { c.PreviewListen = c.ManagementListen }},
		{"non-GitHub repository", func(c *Config) {
			p := c.Projects["myproject"]
			p.Repository = "https://elsewhere.test/x/y"
			c.Projects["myproject"] = p
		}},
		{"repository userinfo", func(c *Config) {
			p := c.Projects["myproject"]
			p.Repository = "https://token@github.com/x/y"
			c.Projects["myproject"] = p
		}},
		{"compose traversal", func(c *Config) {
			p := c.Projects["myproject"]
			p.ComposeFile = "../compose.yaml"
			c.Projects["myproject"] = p
		}},
		{"long slug", func(c *Config) { c.Projects[strings.Repeat("a", 41)] = c.Projects["myproject"] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig(filepath.Join(t.TempDir(), "state"))
			test.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid settings were accepted")
			}
		})
	}
}
