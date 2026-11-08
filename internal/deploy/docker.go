package deploy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

func containerRunning(id string) (bool, error) {
	out, err := run.Capture(dockerTimeout, "", "docker", "inspect", "-f", "{{.State.Running}}", id)
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// containerLogs returns the last n log lines for diagnosing a failed deploy.
// docker logs replays the app's stdout on stdout and its stderr on stderr, so
// both are captured here (unlike run.Capture, which is stdout only).
func containerLogs(id string, n int) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprint(n), id).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(could not read logs: %v)", err)
	}
	return strings.TrimSpace(string(out))
}
