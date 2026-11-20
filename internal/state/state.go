package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("environment not found")

type Status string

const (
	Queued   Status = "queued"
	Fetching Status = "fetching"
	Verified Status = "verified"
	Building Status = "building"
	Starting Status = "starting"
	Healthy  Status = "healthy"
	Retired  Status = "retired"
	Failed   Status = "failed"
)

type Attempt struct {
	IssueTitle     string    `json:"issue_title,omitempty"`
	ID             string    `json:"id"`
	Commit         string    `json:"commit"`
	Status         Status    `json:"status"`
	Port           int       `json:"port,omitempty"`
	Workspace      string    `json:"workspace,omitempty"`
	ComposeProject string    `json:"compose_project,omitempty"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Environment struct {
	ID                 string    `json:"id"`
	Project            string    `json:"project"`
	Intent             int64     `json:"intent"`
	URL                string    `json:"url"`
	ActiveDeploymentID string    `json:"active_deployment_id,omitempty"`
	ActiveCommit       string    `json:"active_commit,omitempty"`
	Deleting           bool      `json:"deleting,omitempty"`
	Attempts           []Attempt `json:"attempts"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func ID(project string, intent int64) string {
	return fmt.Sprintf("%s:%d", project, intent)
}

type Store struct {
	root    string
	mu      sync.Mutex
	changed chan struct{}
}

func New(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("state root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("state root must be a real directory")
	}
	return &Store{root: root, changed: make(chan struct{})}, nil
}

func (s *Store) Load(project string, intent int64) (Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(project, intent)
}

func (s *Store) Save(env Environment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateKey(env.Project, env.Intent); err != nil {
		return err
	}
	if env.ID != ID(env.Project, env.Intent) {
		return errors.New("environment ID does not match project and intent")
	}
	if env.Attempts == nil {
		env.Attempts = []Attempt{}
	}
	dir := filepath.Join(s.root, env.Project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project state path must be a real directory")
	}
	name := filepath.Join(dir, strconv.FormatInt(env.Intent, 10)+".json")
	if info, err := os.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return errors.New("environment state path must be a regular file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, name); err != nil {
		return err
	}
	s.notifyChange()
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) Delete(project string, intent int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateKey(project, intent); err != nil {
		return err
	}
	dir := filepath.Join(s.root, project)
	name := filepath.Join(dir, strconv.FormatInt(intent, 10)+".json")
	dirInfo, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("project state path must be a real directory")
	}
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("environment state path must be a regular file")
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	s.notifyChange()
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Changes closes when the store next changes. Call it before reading a snapshot
// so a write concurrent with the read cannot be missed.
func (s *Store) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Store) notifyChange() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Store) List() ([]Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	projects, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	envs := []Environment{}
	for _, project := range projects {
		if !project.IsDir() {
			return nil, fmt.Errorf("unexpected state entry %q", project.Name())
		}
		files, err := os.ReadDir(filepath.Join(s.root, project.Name()))
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if strings.HasPrefix(file.Name(), ".state-") && strings.HasSuffix(file.Name(), ".tmp") && file.Type().IsRegular() {
				// A process may stop before its final atomic rename.
				continue
			}
			if !strings.HasSuffix(file.Name(), ".json") || !file.Type().IsRegular() {
				return nil, fmt.Errorf("unexpected project state entry %q", file.Name())
			}
			intent, err := strconv.ParseInt(strings.TrimSuffix(file.Name(), ".json"), 10, 64)
			if err != nil {
				return nil, err
			}
			env, err := s.load(project.Name(), intent)
			if err != nil {
				return nil, err
			}
			envs = append(envs, env)
		}
	}
	return envs, nil
}

func (s *Store) load(project string, intent int64) (Environment, error) {
	if err := validateKey(project, intent); err != nil {
		return Environment{}, err
	}
	dir := filepath.Join(s.root, project)
	dirInfo, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, err
	}
	if !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return Environment{}, errors.New("project state path must be a real directory")
	}
	name := filepath.Join(dir, strconv.FormatInt(intent, 10)+".json")
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, err
	}
	if !info.Mode().IsRegular() {
		return Environment{}, errors.New("environment state path must be a regular file")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return Environment{}, err
	}
	var env Environment
	if err := json.Unmarshal(data, &env); err != nil {
		return Environment{}, fmt.Errorf("decode state %s: %w", name, err)
	}
	if env.Project != project || env.Intent != intent || env.ID != ID(project, intent) {
		return Environment{}, fmt.Errorf("state identity mismatch in %s", name)
	}
	return env, nil
}

func validateKey(project string, intent int64) error {
	if project == "" || len(project) > 40 || intent < 1 {
		return errors.New("invalid environment key")
	}
	for i, c := range project {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && (c != '-' || i == 0 || i == len(project)-1) {
			return errors.New("invalid project slug")
		}
	}
	return nil
}
