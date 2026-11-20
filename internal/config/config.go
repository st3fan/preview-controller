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
)

const DefaultPath = "/etc/preview-server.json"

var (
	projectName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	gitPart     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Config contains operator settings. Deployment state is stored separately.
type Config struct {
	ManagementListen   string             `json:"management_listen"`
	PreviewDomain      string             `json:"preview_domain"`
	StateRoot          string             `json:"state_root"`
	DeploymentTokenEnv string             `json:"deployment_token_env"`
	Projects           map[string]Project `json:"projects"`
	token              string
}

type Project struct {
	Repository    string `json:"repository"`
	ComposeFile   string `json:"compose_file"`
	PublicService string `json:"public_service"`
	PublicPort    int    `json:"public_port"`
	HealthPath    string `json:"health_path"`
}

func Load(filename string) (Config, error) {
	var cfg Config
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
		return Config{}, errors.New("settings file is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode settings: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("settings must contain one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.token = os.Getenv(cfg.DeploymentTokenEnv)
	if cfg.token == "" {
		return Config{}, fmt.Errorf("deployment token environment variable %s is empty", cfg.DeploymentTokenEnv)
	}
	return cfg, nil
}

func (c Config) Token() string { return c.token }

func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.ManagementListen)
	if err != nil {
		return fmt.Errorf("management_listen: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return errors.New("management_listen must use a loopback or private IP address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("management_listen has an invalid port")
	}
	if err := validateDomain(c.PreviewDomain); err != nil {
		return fmt.Errorf("preview_domain: %w", err)
	}
	if !pathIsAbsolute(c.StateRoot) {
		return errors.New("state_root must be an absolute path")
	}
	if !envName.MatchString(c.DeploymentTokenEnv) {
		return errors.New("deployment_token_env must name an environment variable")
	}
	if len(c.Projects) == 0 {
		return errors.New("at least one project is required")
	}
	for name, project := range c.Projects {
		if !projectName.MatchString(name) || len(name) > 40 {
			return fmt.Errorf("invalid project slug %q", name)
		}
		if err := project.Validate(); err != nil {
			return fmt.Errorf("project %q: %w", name, err)
		}
	}
	return nil
}

func (p Project) Validate() error {
	u, err := url.Parse(p.Repository)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("repository must be a public https://github.com/owner/repo URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !gitPart.MatchString(parts[0]) || !gitPart.MatchString(strings.TrimSuffix(parts[1], ".git")) || parts[0] == "." || parts[1] == "." {
		return errors.New("repository must identify one GitHub owner and repository")
	}
	if p.ComposeFile == "" || path.IsAbs(p.ComposeFile) || path.Clean(p.ComposeFile) != p.ComposeFile || strings.HasPrefix(p.ComposeFile, "../") || p.ComposeFile == ".." || strings.Contains(p.ComposeFile, `\`) {
		return errors.New("compose_file must be a clean relative path")
	}
	if !projectName.MatchString(p.PublicService) {
		return errors.New("public_service must be a service slug")
	}
	if p.PublicPort < 1 || p.PublicPort > 65535 {
		return errors.New("public_port must be between 1 and 65535")
	}
	if !strings.HasPrefix(p.HealthPath, "/") || strings.HasPrefix(p.HealthPath, "//") || strings.ContainsAny(p.HealthPath, "\r\n") {
		return errors.New("health_path must be an absolute HTTP path")
	}
	return nil
}

func (c Config) URL(project string, intent int64) (string, error) {
	if _, ok := c.Projects[project]; !ok || intent < 1 {
		return "", errors.New("unknown project or invalid intent")
	}
	label := fmt.Sprintf("%d-%s", intent, project)
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
