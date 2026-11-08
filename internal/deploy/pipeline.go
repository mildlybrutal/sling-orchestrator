package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mildlybrutal/sling-orchestrator/internal/health"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

// Run wraps pipeline so that any failure, from any stage, is
// recorded in one place. A deploy that was rolled back keeps that status
// instead of being overwritten with plain "failed", and one whose commit was
// already live is "skipped".
func Run(d *Deployment) error {
	d.SetStatus(StatusQueued)

	deployMu.Lock()
	defer deployMu.Unlock()

	if err := pipeline(d); err != nil {
		switch s, _ := d.Snapshot(); {
		case errors.Is(err, ErrAlreadyLive):
			d.SetStatus(StatusSkipped)
		case s != StatusRolledBack:
			d.SetStatus(StatusFailed)
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
	if err := ValidateRepoURL(d.RepoURL, d.AllowLocalRepo); err != nil {
		return err
	}

	ctx := context.Background()

	// Fail fast if Docker or the proxy is broken, before spending time on a
	// build. This also creates sling-net, which the app container joins.
	d.SetStatus(StatusPreparing)
	if err := proxy.Ensure(ctx); err != nil {
		return fmt.Errorf("proxy setup failed: %w", err)
	}

	containerName := fmt.Sprintf("app-%s", d.CommitSHA)
	upstream := fmt.Sprintf("%s:%s", containerName, proxy.AppPort)

	// Read what is live now, before touching any container. deployMu is held
	// for the whole pipeline, so this cannot change underneath us. Redeploying
	// the live commit is refused: the "docker rm -f" below would kill the
	// container that is serving traffic.
	prev, err := proxy.CurrentUpstream(ctx)
	if err != nil {
		return fmt.Errorf("could not read current upstream: %w", err)
	}
	if prev == upstream {
		return fmt.Errorf("%s: %w", d.CommitSHA, ErrAlreadyLive)
	}
	d.PreviousUpstream = prev

	buildDir := filepath.Join(os.TempDir(), "paas-builds", d.CommitSHA)
	d.ImageTag = fmt.Sprintf("paas-app:%s", d.CommitSHA)

	_ = os.RemoveAll(buildDir)
	// The image holds everything the app needs; the checkout is only an
	// input to docker build. Remove it however the deploy ends.
	defer removeBuildDir(buildDir)

	d.SetStatus(StatusCloning)
	fmt.Printf("Cloning %s into %s...\n", d.RepoURL, buildDir)
	if err := run.Streamed(cloneTimeout, "", "git", "clone", "--", d.RepoURL, buildDir); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	// Pin the build to the exact commit, then verify we really are on it.
	if err := run.Streamed(gitTimeout, buildDir, "git", "checkout", "--detach", d.CommitSHA); err != nil {
		return fmt.Errorf("git checkout failed: %w", err)
	}
	head, err := run.Capture(gitTimeout, buildDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("git rev-parse failed: %w", err)
	}
	if head != d.CommitSHA {
		return fmt.Errorf("checked-out commit %s does not match requested %s", head, d.CommitSHA)
	}

	d.SetStatus(StatusBuilding)
	fmt.Printf("Building Docker image %s...\n", d.ImageTag)
	if err := run.Streamed(buildTimeout, buildDir, "docker", "build", "-t", d.ImageTag, "."); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}

	d.SetStatus(StatusStarting)
	fmt.Printf("Starting container on dynamic host port...\n")
	_, _ = run.Capture(cleanupTimeout, "", "docker", "rm", "-f", containerName)

	// If anything below fails, don't leave a running container behind.
	succeeded := false
	defer func() {
		if !succeeded {
			_, _ = run.Capture(cleanupTimeout, "", "docker", "rm", "-f", containerName)
		}
	}()

	out, err := run.Capture(dockerTimeout, "", "docker", "run", "-d",
		"--name", containerName,
		"--network", proxy.NetworkName, // Caddy reaches the app by this name
		"--label", "sling.managed=true",
		"--label", "sling.sha="+d.CommitSHA,
		"-p", "127.0.0.1::"+proxy.AppPort, // loopback only: Sling's own health checks
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

	portOut, err := run.Capture(dockerTimeout, "", "docker", "port", d.ContainerID, proxy.AppPort+"/tcp")
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
	d.SetStatus(StatusHealthChecking)
	healthURL := fmt.Sprintf("http://127.0.0.1:%s/health", d.HostPort)
	fmt.Printf("Waiting for %s to pass health checks...\n", healthURL)

	isRunning := func() (bool, error) { return containerRunning(d.ContainerID) }
	if err := health.WaitHealthy(ctx, health.Default, healthURL, isRunning); err != nil {
		return fmt.Errorf("health check failed: %w\n--- last container logs ---\n%s",
			err, containerLogs(d.ContainerID, 30))
	}

	d.SetStatus(StatusHealthy)

	// Cutover. The proxy keeps serving the previous version until Caddy
	// has applied the new config, then starts sending requests to the new one.
	d.SetStatus(StatusSwitching)
	fmt.Printf("Switching traffic: %q -> %q\n", prev, upstream)
	if err := proxy.SetUpstream(ctx, upstream); err != nil {
		return fmt.Errorf("cutover failed (traffic unchanged): %w", err)
	}

	// Verify through the proxy, the path real users take, and require that
	// the NEW container answered (Caddy reports which upstream did). This
	// catches what the direct check can't: Caddy unable to reach the
	// container on sling-net, or still routing to the old one.
	fmt.Printf("Verifying through the proxy (%s)...\n", proxy.PublicURL)
	if err := health.WaitUntil(ctx, postCutoverHealth, proxy.PublicURL+"/health", health.ServedBy(proxy.UpstreamHeader, upstream), isRunning); err != nil {
		return rollback(ctx, d, prev, fmt.Errorf("post-cutover check failed: %w", err))
	}

	// Grace window: the old container is still running, so if the new one
	// falls over now, rolling back is a single proxy switch.
	d.SetStatus(StatusObserving)
	fmt.Printf("Observing for %v before retiring the previous version...\n", graceWindow)
	if err := health.Watch(ctx, postCutoverHealth, proxy.PublicURL+"/health", health.ServedBy(proxy.UpstreamHeader, upstream),
		isRunning, graceWindow, graceMaxFailures); err != nil {
		return rollback(ctx, d, prev, fmt.Errorf("failed during grace window: %w", err))
	}

	d.SetStatus(StatusLive)
	succeeded = true

	// Housekeeping only: the new version is already live, so a failure here
	// is a warning, never a failed deploy.
	d.PreviousStopped = retirePrevious(prev, upstream)
	prune(upstream, prev)
	return nil
}
