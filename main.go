package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Deployment struct {
	RepoURL     string
	CommitSHA   string
	ImageTag    string
	ContainerID string
	HostPort    string

	// PreviousUpstream is what the proxy served before this deploy
	// ("app-<sha>:3000"), or "" if nothing was live. Phase 5 retires it.
	PreviousUpstream string

	// Status tracking (see status.go). Guarded by mu because the dashboard
	// API will read these while the pipeline goroutine writes them.
	mu      sync.Mutex
	status  Status
	history []StatusChange
}

const (
	cloneTimeout   = 1 * time.Minute
	gitTimeout     = 30 * time.Second
	buildTimeout   = 10 * time.Minute
	dockerTimeout  = 30 * time.Second
	cleanupTimeout = 30 * time.Second

	// Dev only: lets RepoURL be a local path like ../sling. Turn off before
	// the webhook server accepts input from the network.
	allowLocalRepos = true
)

var (
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	allowedRepoHosts   = map[string]bool{"github.com": true}

	// One deploy at a time. There is a single app for now; the webhook
	// phase turns this into a per-app lock.
	deployMu sync.Mutex
)

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s <repo-url-or-path> <40-char-commit-sha>", os.Args[0])
	}

	deploy := &Deployment{
		RepoURL:   os.Args[1],
		CommitSHA: os.Args[2],
	}

	if err := runPipeline(deploy); err != nil {
		deploy.printTimeline()
		log.Fatalf("Deployment failed: %v", err)
	}

	fmt.Printf("\n✅ Live\n")
	fmt.Printf("   Container ID : %s\n", deploy.ContainerID[:12])
	fmt.Printf("   Public URL   : %s\n", proxyPublicURL)
	fmt.Printf("   Direct URL   : http://127.0.0.1:%s (bypasses the proxy)\n", deploy.HostPort)
	if deploy.PreviousUpstream != "" {
		fmt.Printf("   Previous     : %s (still running; phase 5 retires it)\n", deploy.PreviousUpstream)
	}
	deploy.printTimeline()
}

func validateRepoURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") {
		return fmt.Errorf("invalid repo URL %q", raw)
	}
	if allowLocalRepos && !strings.Contains(raw, "://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid repo URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("repo URL must use https, got %q", u.Scheme)
	}
	if !allowedRepoHosts[u.Host] {
		return fmt.Errorf("repo host %q is not allowed", u.Host)
	}
	return nil
}

// runPipeline wraps pipeline so that any failure, from any stage, is
// recorded in one place. A deploy that was rolled back keeps that status
// instead of being overwritten with plain "failed".
func runPipeline(d *Deployment) error {
	d.setStatus(StatusQueued)

	deployMu.Lock()
	defer deployMu.Unlock()

	if err := pipeline(d); err != nil {
		if s, _ := d.Snapshot(); s != StatusRolledBack {
			d.setStatus(StatusFailed)
		}
		return err
	}
	return nil
}

func pipeline(d *Deployment) error {
	// Validate everything that ends up in a path or a command argument.
	if !shaPattern.MatchString(d.CommitSHA) {
		return fmt.Errorf("invalid commit SHA %q: must be 40 lowercase hex characters", d.CommitSHA)
	}
	if err := validateRepoURL(d.RepoURL); err != nil {
		return err
	}

	ctx := context.Background()

	// Fail fast if Docker or the proxy is broken, before spending time on a
	// build. This also creates sling-net, which the app container joins.
	d.setStatus(StatusPreparing)
	if err := ensureProxy(ctx); err != nil {
		return fmt.Errorf("proxy setup failed: %w", err)
	}

	buildDir := filepath.Join(os.TempDir(), "paas-builds", d.CommitSHA)
	d.ImageTag = fmt.Sprintf("paas-app:%s", d.CommitSHA)

	_ = os.RemoveAll(buildDir)

	d.setStatus(StatusCloning)
	fmt.Printf("Cloning %s into %s...\n", d.RepoURL, buildDir)
	if err := runStreamedCmd(cloneTimeout, "", "git", "clone", "--", d.RepoURL, buildDir); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	// Pin the build to the exact commit, then verify we really are on it.
	if err := runStreamedCmd(gitTimeout, buildDir, "git", "checkout", "--detach", d.CommitSHA); err != nil {
		return fmt.Errorf("git checkout failed: %w", err)
	}
	head, err := runCaptureCmd(gitTimeout, buildDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse failed: %w", err)
	}
	if head != d.CommitSHA {
		return fmt.Errorf("checked-out commit %s does not match requested %s", head, d.CommitSHA)
	}

	d.setStatus(StatusBuilding)
	fmt.Printf("Building Docker image %s...\n", d.ImageTag)
	if err := runStreamedCmd(buildTimeout, buildDir, "docker", "build", "-t", d.ImageTag, "."); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}

	d.setStatus(StatusStarting)
	fmt.Printf("Starting container on dynamic host port...\n")
	containerName := fmt.Sprintf("app-%s", d.CommitSHA)
	_, _ = runCaptureCmd(cleanupTimeout, "", "docker", "rm", "-f", containerName)

	// If anything below fails, don't leave a running container behind.
	succeeded := false
	defer func() {
		if !succeeded {
			_, _ = runCaptureCmd(cleanupTimeout, "", "docker", "rm", "-f", containerName)
		}
	}()

	out, err := runCaptureCmd(dockerTimeout, "", "docker", "run", "-d",
		"--name", containerName,
		"--network", networkName, // Caddy reaches the app by this name
		"--label", "sling.managed=true",
		"--label", "sling.sha="+d.CommitSHA,
		"-p", "127.0.0.1::"+appPort, // loopback only: Sling's own health checks
		"--memory", "256m",
		"--cpus", "0.5",
		"-e", fmt.Sprintf("APP_VERSION=%s", d.CommitSHA),
		d.ImageTag,
	)
	if err != nil {
		return fmt.Errorf("docker run failed: %w", err)
	}
	if !containerIDPattern.MatchString(out) {
		return fmt.Errorf("unexpected output from docker run: %q", out)
	}
	d.ContainerID = out

	portOut, err := runCaptureCmd(dockerTimeout, "", "docker", "port", d.ContainerID, appPort+"/tcp")
	if err != nil {
		// A container that dies instantly has no published port, so this is
		// often the first symptom of a crash on boot. Show why.
		return fmt.Errorf("failed to inspect port (container may have exited): %w\n--- last container logs ---\n%s",
			err, containerLogs(d.ContainerID, 30))
	}

	firstLine := strings.Split(portOut, "\n")[0]
	parts := strings.Split(firstLine, ":")
	d.HostPort = strings.TrimSpace(parts[len(parts)-1])
	if d.HostPort == "" {
		return fmt.Errorf("could not parse host port from %q", portOut)
	}

	// Nothing has been routed to this container yet, so a failure here costs
	// no user traffic. The deferred cleanup above removes it on error.
	d.setStatus(StatusHealthChecking)
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/health", d.HostPort)
	fmt.Printf("Waiting for %s to pass health checks...\n", healthURL)

	isRunning := func() (bool, error) { return containerRunning(d.ContainerID) }
	if err := waitHealthy(ctx, defaultHealthConfig, healthURL, isRunning); err != nil {
		return fmt.Errorf("health check failed: %w\n--- last container logs ---\n%s",
			err, containerLogs(d.ContainerID, 30))
	}

	d.setStatus(StatusHealthy)

	// Cutover. The proxy keeps serving the previous version until Caddy
	// has applied the new config, then starts sending requests to the new one.
	upstream := fmt.Sprintf("%s:%s", containerName, appPort)

	prev, err := currentUpstream(ctx)
	if err != nil {
		return fmt.Errorf("could not read current upstream: %w", err)
	}
	d.PreviousUpstream = prev

	d.setStatus(StatusSwitching)
	fmt.Printf("Switching traffic: %q -> %q\n", prev, upstream)
	if err := setUpstream(ctx, upstream); err != nil {
		return fmt.Errorf("cutover failed (traffic unchanged): %w", err)
	}

	// Verify through the proxy, the path real users take, and require that
	// the NEW container answered (Caddy reports which upstream did). This
	// catches what the direct check can't: Caddy unable to reach the
	// container on sling-net, or still routing to the old one.
	fmt.Printf("Verifying through the proxy (%s)...\n", proxyPublicURL)
	if err := waitUntil(ctx, postCutoverHealth, proxyPublicURL+"/health", servedBy(upstream), isRunning); err != nil {
		rbErr := setUpstream(ctx, prev)
		if rbErr != nil {
			return fmt.Errorf("post-cutover check failed (%v) AND rollback failed (%v): the proxy may be pointing at a container that is about to be removed",
				err, rbErr)
		}
		d.setStatus(StatusRolledBack)
		return fmt.Errorf("post-cutover check failed, traffic restored to %q: %w", prev, err)
	}

	d.setStatus(StatusLive)
	succeeded = true
	return nil
}

func containerRunning(id string) (bool, error) {
	out, err := runCaptureCmd(dockerTimeout, "", "docker", "inspect", "-f", "{{.State.Running}}", id)
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// containerLogs returns the last n log lines for diagnosing a failed deploy.
// docker logs replays the app's stdout on stdout and its stderr on stderr, so
// both are captured here (unlike runCaptureCmd, which is stdout only).
func containerLogs(id string, n int) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprint(n), id).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(could not read logs: %v)", err)
	}
	return strings.TrimSpace(string(out))
}

func commandEnv() []string {
	// Fail fast instead of hanging on a credentials prompt.
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
}

func runStreamedCmd(timeout time.Duration, dir, name string, args ...string) error {
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

// runCaptureCmd returns stdout only. stderr is kept out of the result and
// is only used to build the error message when the command fails.
func runCaptureCmd(timeout time.Duration, dir, name string, args ...string) (string, error) {
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
