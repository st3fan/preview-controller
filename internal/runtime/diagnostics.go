package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const diagnosticOutputLimit = 8 << 10

func (r *Runtime) dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DOCKER_CONFIG=" + r.dockerConfig}
	return cmd
}

// runDeploymentStep captures the end of failed build or startup output. These
// commands use output for diagnostics only, so truncation cannot fail a build.
func (r *Runtime) runDeploymentStep(ctx context.Context, confidential bool, args ...string) (string, error) {
	cmd := r.dockerCommand(ctx, args...)
	var output diagnosticOutput
	if !confidential {
		cmd.Stdout, cmd.Stderr = &output, &output
	}
	if err := cmd.Run(); err != nil {
		return strings.ToValidUTF8(string(output.data), ""), fmt.Errorf("Docker command failed: %w", err)
	}
	return "", nil
}

type diagnosticOutput struct {
	data []byte
}

func (b *diagnosticOutput) Write(p []byte) (int, error) {
	switch {
	case len(p) >= diagnosticOutputLimit:
		b.data = append(b.data[:0], p[len(p)-diagnosticOutputLimit:]...)
	case len(b.data)+len(p) > diagnosticOutputLimit:
		drop := len(b.data) + len(p) - diagnosticOutputLimit
		kept := copy(b.data, b.data[drop:])
		b.data = append(b.data[:kept], p...)
	default:
		b.data = append(b.data, p...)
	}
	return len(p), nil
}
