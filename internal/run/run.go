// Package run executes external commands (git, docker) with a timeout and
// without a shell: every argument reaches the program as-is, so input like
// "; rm -rf /" is never interpreted.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func commandEnv() []string {
	// Fail fast instead of hanging on a credentials prompt.
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
}

// Streamed runs a command with its output shown live (clone, build).
func Streamed(timeout time.Duration, dir, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = commandEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s timed out after %v", name, timeout)
		}
		return err
	}
	return nil
}

// Capture returns stdout only. stderr is kept out of the result and
// is only used to build the error message when the command fails.
func Capture(timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = commandEnv()

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s timed out after %v", name, timeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
