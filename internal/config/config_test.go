package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validConfig(root string) Config {
	return Config{
		ManagementListen: "127.0.0.1:8080",
		PreviewListen:    "127.0.0.1:8081",
		PreviewDomain:    "preview.example.com",
		StateRoot:        root,
		WorkspaceRoot:    filepath.Join(filepath.Dir(root), "workspaces"),
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

func isolatedEnvironment(t *testing.T) {
	t.Helper()
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "PREVIEW_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("PREVIEW_SERVER_TOKEN", "deployment-secret")
	t.Setenv("PREVIEW_DOMAIN", "preview.example.com")
}

func writeProjects(t *testing.T, contents []byte) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(filename, contents, 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadMigratedSettings(t *testing.T) {
	isolatedEnvironment(t)
	want := validConfig(filepath.Join(t.TempDir(), "state"))
	want.ManagementListen = "127.0.0.2:9090"
	want.PreviewListen = "127.0.0.3:9091"
	want.ArchiveAfterIdle = "72h"
	want.DeleteAfterArchive = "96h"
	want.DashboardOrigin = "https://dashboard.example.com"
	want.DashboardAuth = &DashboardAuth{Issuer: "https://id.example.com", ClientID: "browser", ViewGroup: "view", OperateGroup: "operate"}
	want.MCPOAuth = &MCPOAuth{Issuer: want.DashboardAuth.Issuer, Resource: want.DashboardOrigin + "/mcp", JWKSURI: want.DashboardAuth.Issuer + "/keys"}
	want.token = "deployment-secret"
	want.githubToken = "github-secret"
	project := want.Projects["myproject"]
	project.ArchiveAfterIdle = "12h"
	project.ComposeEnv = map[string]string{"APP_MODE": "preview"}
	project.ComposeEnvFrom = map[string]string{"APP_TOKEN": "APP_TOKEN_SOURCE"}
	want.Projects["myproject"] = project
	want.Projects["web"] = Project{Backend: BackendDocker, Repository: "https://github.com/example/web", HealthPath: "/"}
	for key, value := range map[string]string{
		"PREVIEW_MANAGEMENT_LISTEN":       want.ManagementListen,
		"PREVIEW_LISTEN":                  want.PreviewListen,
		"PREVIEW_STATE_ROOT":              want.StateRoot,
		"PREVIEW_SERVER_WORKSPACE":        want.WorkspaceRoot,
		"PREVIEW_GITHUB_TOKEN":            want.githubToken,
		"PREVIEW_ARCHIVE_AFTER_IDLE":      want.ArchiveAfterIdle,
		"PREVIEW_DELETE_AFTER_ARCHIVE":    want.DeleteAfterArchive,
		"PREVIEW_DASHBOARD_ORIGIN":        want.DashboardOrigin,
		"PREVIEW_DASHBOARD_ISSUER":        want.DashboardAuth.Issuer,
		"PREVIEW_DASHBOARD_CLIENT_ID":     want.DashboardAuth.ClientID,
		"PREVIEW_DASHBOARD_VIEW_GROUP":    want.DashboardAuth.ViewGroup,
		"PREVIEW_DASHBOARD_OPERATE_GROUP": want.DashboardAuth.OperateGroup,
		"PREVIEW_MCP_ISSUER":              want.MCPOAuth.Issuer,
		"PREVIEW_MCP_RESOURCE":            want.MCPOAuth.Resource,
		"PREVIEW_MCP_JWKS_URI":            want.MCPOAuth.JWKSURI,
	} {
		t.Setenv(key, value)
	}
	projects, err := json.Marshal(want.Projects)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(writeProjects(t, projects))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("migrated configuration differs")
	}
	url, err := got.URL("myproject", 123)
	if err != nil || url != "https://myproject-123.preview.example.com" {
		t.Fatalf("URL = %q, %v", url, err)
	}
}

func TestLoadRejectsMalformedProjects(t *testing.T) {
	isolatedEnvironment(t)
	for _, contents := range []string{"null", "{}", "[]", "{", `{"web":{"unknown":true}}`, `{} {}`, `{"projects":{"web":{}}}`, strings.Repeat(" ", 1<<20+1)} {
		if _, err := Load(writeProjects(t, []byte(contents))); err == nil {
			t.Fatal("invalid projects file was accepted")
		}
	}
}

func TestLoadAuthConfiguration(t *testing.T) {
	isolatedEnvironment(t)
	projects := writeProjects(t, []byte(`{"web":{"backend":"docker","repository":"https://github.com/example/web","health_path":"/"}}`))
	cfg, err := Load(projects)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPOAuth != nil || cfg.DashboardAuth != nil || cfg.GitHubToken() != "" ||
		cfg.ManagementListen != "127.0.0.1:8080" || cfg.PreviewListen != "127.0.0.1:8081" ||
		cfg.StateRoot != "/var/lib/preview-server" || cfg.WorkspaceRoot != "/srv/preview-server" {
		t.Fatal("unexpected defaults")
	}
	for _, test := range []struct{ key, value, message string }{
		{"PREVIEW_SERVER_TOKEN", "", "PREVIEW_SERVER_TOKEN"},
		{"PREVIEW_DOMAIN", "", "PREVIEW_DOMAIN"},
		{"PREVIEW_MCP_ISSUER", "https://id.example.com", "PREVIEW_MCP_"},
		{"PREVIEW_DASHBOARD_CLIENT_ID", "browser", "PREVIEW_DASHBOARD_ISSUER"},
	} {
		t.Run(test.key, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			_, err := Load(projects)
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "deployment-secret") {
				t.Fatalf("unexpected validation result: %v", err)
			}
		})
	}
}

func TestRejectControllerCredentialSources(t *testing.T) {
	for _, source := range []string{"PREVIEW_SERVER_TOKEN", "PREVIEW_GITHUB_TOKEN"} {
		cfg := validConfig(filepath.Join(t.TempDir(), "state"))
		p := cfg.Projects["myproject"]
		p.ComposeEnvFrom = map[string]string{"APP_TOKEN": source}
		cfg.Projects["myproject"] = p
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), source) {
			t.Fatalf("unexpected validation for %s: %v", source, err)
		}
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
