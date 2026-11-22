package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultProjectsPath     = "/etc/preview-server/projects.json"
	deploymentTokenVariable = "PREVIEW_SERVER_TOKEN"
	githubTokenVariable     = "PREVIEW_GITHUB_TOKEN"
)

var (
	projectName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	gitPart     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Config contains operator settings. Deployment state is stored separately.
type Config struct {
	DashboardAuth      *DashboardAuth
	MCPOAuth           *MCPOAuth
	ArchiveAfterIdle   string
	DeleteAfterArchive string
	DashboardOrigin    string
	ManagementListen   string
	PreviewListen      string
	PreviewDomain      string
	StateRoot          string
	WorkspaceRoot      string
	Projects           map[string]Project
	token              string
	githubToken        string
}

// DashboardAuth maps Pocket ID group names to browser dashboard roles.
type DashboardAuth struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ViewGroup    string `json:"view_group"`
	OperateGroup string `json:"operate_group"`
}

// MCPOAuth configures the trusted Pocket ID issuer and protected MCP resource.
type MCPOAuth struct {
	Issuer   string `json:"issuer"`
	Resource string `json:"resource"`
	JWKSURI  string `json:"jwks_uri"`
}

func (o MCPOAuth) validate() error {
	for name, value := range map[string]string{"issuer": o.Issuer, "resource": o.Resource, "jwks_uri": o.JWKSURI} {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(value, "?#") {
			return fmt.Errorf("PREVIEW_MCP_%s must be an absolute HTTPS URL without credentials, query or fragment", strings.ToUpper(name))
		}
		if name == "resource" && u.EscapedPath() != "/mcp" {
			return errors.New("PREVIEW_MCP_RESOURCE must use the exact /mcp path")
		}
	}
	return nil
}

const (
	BackendCompose = "compose"
	BackendDocker  = "docker"
)

func (p Project) DeploymentBackend() string {
	if p.Backend == "" {
		return BackendCompose
	}
	return p.Backend
}

type Project struct {
	Backend            string            `json:"backend,omitempty"`
	ArchiveAfterIdle   string            `json:"archive_after_idle,omitempty"`
	DeleteAfterArchive string            `json:"delete_after_archive,omitempty"`
	PreviewInspector   bool              `json:"preview_inspector,omitempty"`
	Repository         string            `json:"repository"`
	ComposeFile        string            `json:"compose_file"`
	PublicService      string            `json:"public_service"`
	PublicPort         int               `json:"public_port"`
	HealthPath         string            `json:"health_path"`
	ComposeEnv         map[string]string `json:"compose_env,omitempty"`
	ComposeEnvFrom     map[string]string `json:"compose_env_from,omitempty"`
}

func Load(filename string) (Config, error) {
	cfg := fromEnvironment()
	f, err := os.Open(filename)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("projects file is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg.Projects); err != nil {
		return Config{}, fmt.Errorf("decode projects: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("projects file must contain one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	if cfg.token == "" {
		return Config{}, errors.New("PREVIEW_SERVER_TOKEN is required")
	}
	return cfg, nil
}

func fromEnvironment() Config {
	cfg := Config{
		ManagementListen:   envDefault("PREVIEW_MANAGEMENT_LISTEN", "127.0.0.1:8080"),
		PreviewListen:      envDefault("PREVIEW_LISTEN", "127.0.0.1:8081"),
		PreviewDomain:      os.Getenv("PREVIEW_DOMAIN"),
		StateRoot:          envDefault("PREVIEW_STATE_ROOT", "/var/lib/preview-server"),
		WorkspaceRoot:      envDefault("PREVIEW_SERVER_WORKSPACE", "/srv/preview-server"),
		DashboardOrigin:    os.Getenv("PREVIEW_DASHBOARD_ORIGIN"),
		ArchiveAfterIdle:   os.Getenv("PREVIEW_ARCHIVE_AFTER_IDLE"),
		DeleteAfterArchive: os.Getenv("PREVIEW_DELETE_AFTER_ARCHIVE"),
		token:              os.Getenv(deploymentTokenVariable),
		githubToken:        strings.TrimSpace(os.Getenv(githubTokenVariable)),
	}
	dashboard := DashboardAuth{
		Issuer:       os.Getenv("PREVIEW_DASHBOARD_ISSUER"),
		ClientID:     os.Getenv("PREVIEW_DASHBOARD_CLIENT_ID"),
		ViewGroup:    os.Getenv("PREVIEW_DASHBOARD_VIEW_GROUP"),
		OperateGroup: os.Getenv("PREVIEW_DASHBOARD_OPERATE_GROUP"),
	}
	if dashboard != (DashboardAuth{}) {
		cfg.DashboardAuth = &dashboard
	}
	oauth := MCPOAuth{
		Issuer:   os.Getenv("PREVIEW_MCP_ISSUER"),
		Resource: os.Getenv("PREVIEW_MCP_RESOURCE"),
		JWKSURI:  os.Getenv("PREVIEW_MCP_JWKS_URI"),
	}
	if oauth != (MCPOAuth{}) {
		cfg.MCPOAuth = &oauth
	}
	return cfg
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func (c Config) GitHubToken() string { return c.githubToken }

func (c Config) Token() string { return c.token }

func (c Config) Validate() error {
	if c.MCPOAuth != nil {
		if err := c.MCPOAuth.validate(); err != nil {
			return err
		}
	}
	if err := validateLifecycle(c.ArchiveAfterIdle, c.DeleteAfterArchive); err != nil {
		return err
	}
	if c.DashboardOrigin != "" {
		origin, err := url.Parse(c.DashboardOrigin)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
			origin.Hostname() == "" || origin.User != nil || origin.Path != "" ||
			origin.ForceQuery || origin.RawQuery != "" || origin.Fragment != "" ||
			origin.String() != origin.Scheme+"://"+origin.Host {
			return errors.New("PREVIEW_DASHBOARD_ORIGIN must be an HTTP or HTTPS origin")
		}
	}
	if a := c.DashboardAuth; a != nil {
		issuer, err := url.Parse(a.Issuer)
		if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || strings.ContainsAny(a.Issuer, "?#") {
			return errors.New("PREVIEW_DASHBOARD_ISSUER must be an absolute HTTPS issuer URL")
		}
		if !strings.HasPrefix(c.DashboardOrigin, "https://") {
			return errors.New("dashboard sign-in requires an HTTPS PREVIEW_DASHBOARD_ORIGIN")
		}
		if c.MCPOAuth != nil && a.Issuer != c.MCPOAuth.Issuer {
			return errors.New("PREVIEW_DASHBOARD_ISSUER and PREVIEW_MCP_ISSUER must match")
		}
		if strings.TrimSpace(a.ClientID) == "" || strings.TrimSpace(a.ViewGroup) == "" || strings.TrimSpace(a.OperateGroup) == "" || a.ViewGroup == a.OperateGroup {
			return errors.New("dashboard sign-in requires PREVIEW_DASHBOARD_CLIENT_ID and distinct, nonempty PREVIEW_DASHBOARD_VIEW_GROUP and PREVIEW_DASHBOARD_OPERATE_GROUP")
		}
	}
	if err := validateListener(c.ManagementListen, "PREVIEW_MANAGEMENT_LISTEN"); err != nil {
		return err
	}
	if err := validateListener(c.PreviewListen, "PREVIEW_LISTEN"); err != nil {
		return err
	}
	if c.ManagementListen == c.PreviewListen {
		return errors.New("PREVIEW_MANAGEMENT_LISTEN and PREVIEW_LISTEN must differ")
	}
	if err := validateDomain(c.PreviewDomain); err != nil {
		return fmt.Errorf("PREVIEW_DOMAIN: %w", err)
	}
	if !pathIsAbsolute(c.StateRoot) {
		return errors.New("PREVIEW_STATE_ROOT must be an absolute path")
	}
	if !pathIsAbsolute(c.WorkspaceRoot) {
		return errors.New("PREVIEW_SERVER_WORKSPACE must be an absolute path")
	}
	if c.StateRoot == c.WorkspaceRoot || strings.HasPrefix(c.StateRoot, c.WorkspaceRoot+"/") || strings.HasPrefix(c.WorkspaceRoot, c.StateRoot+"/") {
		return errors.New("PREVIEW_STATE_ROOT and PREVIEW_SERVER_WORKSPACE must be separate directories")
	}
	if len(c.Projects) == 0 {
		return errors.New("at least one project is required")
	}
	for name, project := range c.Projects {
		if !projectName.MatchString(name) || len(name) > 40 {
			return fmt.Errorf("invalid project slug %q", name)
		}
		for _, source := range project.ComposeEnvFrom {
			if source == deploymentTokenVariable || source == githubTokenVariable {
				return fmt.Errorf("project %q: %s cannot be passed to Compose", name, source)
			}
		}
		if err := project.Validate(); err != nil {
			return fmt.Errorf("project %q: %w", name, err)
		}
	}
	return nil
}

func validateListener(address, name string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return fmt.Errorf("%s must use a loopback or private IP address", name)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s has an invalid port", name)
	}
	return nil
}

func (p Project) Validate() error {
	if err := validateLifecycle(p.ArchiveAfterIdle, p.DeleteAfterArchive); err != nil {
		return err
	}
	u, err := url.Parse(p.Repository)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("repository must be a public https://github.com/owner/repo URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !gitPart.MatchString(parts[0]) || !gitPart.MatchString(strings.TrimSuffix(parts[1], ".git")) || parts[0] == "." || parts[1] == "." {
		return errors.New("repository must identify one GitHub owner and repository")
	}
	switch p.DeploymentBackend() {
	case BackendCompose:
		if p.ComposeFile == "" || path.IsAbs(p.ComposeFile) || path.Clean(p.ComposeFile) != p.ComposeFile || strings.HasPrefix(p.ComposeFile, "../") || p.ComposeFile == ".." || strings.Contains(p.ComposeFile, `\`) {
			return errors.New("compose_file must be a clean relative path")
		}
		if !projectName.MatchString(p.PublicService) {
			return errors.New("public_service must be a service slug")
		}
	case BackendDocker:
		if p.ComposeFile != "" || p.PublicService != "" || len(p.ComposeEnv)+len(p.ComposeEnvFrom) != 0 {
			return errors.New("Compose settings require the compose backend")
		}
	default:
		return fmt.Errorf("unknown deployment backend %q", p.Backend)
	}
	if p.PublicPort < 0 || p.PublicPort > 65535 {
		return errors.New("public_port must be zero (default) or between 1 and 65535")
	}
	if !strings.HasPrefix(p.HealthPath, "/") || strings.HasPrefix(p.HealthPath, "//") || strings.ContainsAny(p.HealthPath, "\r\n") {
		return errors.New("health_path must be an absolute HTTP path")
	}
	for name, value := range p.ComposeEnv {
		if !validComposeVariable(name) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid compose_env entry %q", name)
		}
	}
	for name, source := range p.ComposeEnvFrom {
		if !validComposeVariable(name) || !envName.MatchString(source) {
			return fmt.Errorf("invalid compose_env_from entry %q", name)
		}
		if _, exists := p.ComposeEnv[name]; exists {
			return fmt.Errorf("Compose variable %q is configured twice", name)
		}
	}

	return nil
}

func (p Project) ContainerPort() int {
	if p.PublicPort == 0 {
		return 8080
	}
	return p.PublicPort
}

func validComposeVariable(name string) bool {
	return envName.MatchString(name) && name != "PREVIEW_PORT" && name != "PATH" &&
		!strings.HasPrefix(name, "COMPOSE_") && !strings.HasPrefix(name, "DOCKER_") &&
		!strings.HasPrefix(name, "BUILDX_") && !strings.HasPrefix(name, "BUILDKIT_")
}

func (c Config) URL(project string, intent int64) (string, error) {
	if _, ok := c.Projects[project]; !ok || intent < 1 {
		return "", errors.New("unknown project or invalid intent")
	}
	label := fmt.Sprintf("%s-%d", project, intent)
	if len(label) > 63 {
		return "", errors.New("preview hostname label is too long")
	}
	return "https://" + label + "." + c.PreviewDomain, nil
}

func validateDomain(domain string) error {
	if len(domain) < 3 || len(domain) > 253 || strings.HasSuffix(domain, ".") {
		return errors.New("invalid domain name")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || !projectName.MatchString(label) {
			return errors.New("invalid domain label")
		}
	}
	return nil
}

func pathIsAbsolute(name string) bool {
	return strings.HasPrefix(name, "/") && path.Clean(name) == name
}

func validateLifecycle(archive, remove string) error {
	for _, setting := range []struct{ name, value string }{
		{"archive_after_idle", archive}, {"delete_after_archive", remove},
	} {
		if setting.value == "" {
			continue
		}
		duration, err := time.ParseDuration(setting.value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("%s must be a positive duration such as 168h", setting.name)
		}
	}
	return nil
}

// Lifecycle returns the effective archive and retention intervals.
func (c Config) Lifecycle(project string) (archive, retention time.Duration) {
	p := c.Projects[project]
	return lifecycleDuration(p.ArchiveAfterIdle, c.ArchiveAfterIdle),
		lifecycleDuration(p.DeleteAfterArchive, c.DeleteAfterArchive)
}

func lifecycleDuration(override, global string) time.Duration {
	for _, value := range []string{override, global} {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			return duration
		}
	}
	return 7 * 24 * time.Hour
}
