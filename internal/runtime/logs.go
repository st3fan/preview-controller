package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/st3fan/preview-controller/internal/state"
)

var ErrLogsUnavailable = errors.New("deployment logs unavailable")

// OpenLogs checks for containers belonging to the attempt and prepares a log
// follow that can run after the caller has sent the response headers.
func (r composeBackend) logs(ctx context.Context, attempt state.Attempt, follow bool) (func(io.Writer) error, error) {
	if !attemptID.MatchString(attempt.ID) ||
		attempt.ComposeProject != "pv"+strings.TrimPrefix(attempt.ID, "deployment-") {
		return nil, ErrLogsUnavailable
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	containers, err := r.docker(check, 64<<10, "container", "ls", "-aq",
		"--filter", "label=com.docker.compose.project="+attempt.ComposeProject)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(containers) == "" {
		return nil, ErrLogsUnavailable
	}
	args := []string{"compose", "-p", attempt.ComposeProject, "logs", "--no-color", "--tail", "100"}
	if follow {
		args = append(args, "--follow")
	}
	return r.runLogs(ctx, false, args...), nil
}

func (r *Runtime) runLogs(ctx context.Context, stderr bool, args ...string) func(io.Writer) error {
	return func(output io.Writer) error {
		cmd := r.dockerCommand(ctx, args...)
		if stderr {
			cmd.Stderr = output
		}
		// The Docker CLI launches Compose as a child process. Cancel both so
		// an idle follow cannot outlive the HTTP request.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.Dir = r.dockerConfig
		cmd.Stdout = output
		return cmd.Run()
	}
}

// LogExcerpt uses the same attempt ownership checks as live log streaming.
func (r *Runtime) LogExcerpt(ctx context.Context, a state.Attempt) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := r.backend(a)
	if err != nil {
		return "", ErrLogsUnavailable
	}
	read, err := b.logs(ctx, a, false)
	if err != nil {
		return "", err
	}
	var output diagnosticOutput
	if err := read(&output); err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(output.data), ""), nil
}
