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

func TestLifecycleSettings(t *testing.T) {
	cfg := Config{Projects: map[string]Project{"demo": {DeleteAfterArchive: "12h"}}}
	if archive, retention := cfg.Lifecycle("demo"); archive.Hours() != 168 || retention.Hours() != 12 {
		t.Fatalf("archive = %v, retention = %v", archive, retention)
	}
	cfg.ArchiveAfterIdle = "24h"
	cfg.DeleteAfterArchive = "48h"
	if archive, retention := cfg.Lifecycle("demo"); archive.Hours() != 24 || retention.Hours() != 12 {
		t.Fatalf("archive = %v, retention = %v", archive, retention)
	}
	if archive, retention := cfg.Lifecycle("other"); archive.Hours() != 24 || retention.Hours() != 48 {
		t.Fatalf("archive = %v, retention = %v", archive, retention)
	}
	for _, value := range []string{"0s", "-1h", "one week"} {
		if err := (Config{ArchiveAfterIdle: value}).Validate(); err == nil || !strings.Contains(err.Error(), "archive_after_idle") {
			t.Fatalf("global validation for %q = %v", value, err)
		}
		if err := (Project{DeleteAfterArchive: value}).Validate(); err == nil || !strings.Contains(err.Error(), "delete_after_archive") {
			t.Fatalf("project validation for %q = %v", value, err)
		}
	}
}

func TestProjectBackend(t *testing.T) {
	p := Project{Repository: "https://github.com/example/web", HealthPath: "/"}
	if p.DeploymentBackend() != BackendCompose || p.Validate() == nil {
		t.Fatal("default backend must validate Compose settings")
	}
	p.Backend = BackendDocker
	for _, port := range []int{0, 3000} {
		p.PublicPort = port
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	p.ComposeFile = "compose.yaml"
	if p.Validate() == nil {
		t.Fatal("accepted Compose settings on Docker project")
	}
	p.Backend = "unknown"
	if p.Validate() == nil {
		t.Fatal("accepted unknown backend")
	}
}

func TestMCPOAuthSettings(t *testing.T) {
	valid := MCPOAuth{Issuer: "https://id.example.net", JWKSURI: "https://id.example.net/keys", Resource: "https://mcp.example.net/mcp"}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*MCPOAuth){
		func(o *MCPOAuth) { o.Issuer = "http://issuer.test" },
		func(o *MCPOAuth) { o.JWKSURI = "" },
		func(o *MCPOAuth) { o.JWKSURI = "https://user@keys.test/" },
		func(o *MCPOAuth) { o.Resource = "https://preview.test/mcp?" },
		func(o *MCPOAuth) { o.Resource = "https://preview.test/%6dcp" },
	} {
		cfg := valid
		change(&cfg)
		if err := cfg.validate(); err == nil {
			t.Fatalf("accepted invalid OAuth settings: %+v", cfg)
		}
	}
}

func TestDashboardAuthValidation(t *testing.T) {
	cfg := validConfig(filepath.Join(t.TempDir(), "state"))
	cfg.DashboardOrigin = "https://preview.test"
	cfg.DashboardAuth = &DashboardAuth{Issuer: "https://id.test", ClientID: "browser", ViewGroup: "view", OperateGroup: "operate"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.DashboardOrigin = "http://preview.test" },
		func(c *Config) { c.DashboardAuth.Issuer = "http://id.test" },
		func(c *Config) { c.DashboardAuth.ClientID = "" },
		func(c *Config) { c.DashboardAuth.ViewGroup = "" },
		func(c *Config) { c.DashboardAuth.OperateGroup = c.DashboardAuth.ViewGroup },
		func(c *Config) {
			c.MCPOAuth = &MCPOAuth{Issuer: "https://other.test", Resource: "https://preview.test/mcp", JWKSURI: "https://other.test/keys"}
		},
	} {
		candidate := cfg
		auth := *cfg.DashboardAuth
		candidate.DashboardAuth = &auth
		edit(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatal("accepted invalid dashboard configuration")
		}
	}
}
