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

// A deploy is two halves joined only by an image tag:
//
//	build   : source code -> image paas-app:<sha>   (no effect on users)
//	release : image -> live, safely                 (all the risk, all the checks)
//
// A deploy is build + release. A rollback will be release alone, on an image
// that already exists, so it gets every health check and the grace window
// without a second copy of that logic.

// Run deploys d.CommitSHA from d.RepoURL: build, then release.
func Run(d *Deployment) error {
	return locked(d, func(ctx context.Context) error {
		if !shaPattern.MatchString(d.CommitSHA) {
			return fmt.Errorf("invalid commit SHA %q: must be 40 lowercase hex characters", d.CommitSHA)
		}
		if err := ValidateRepoURL(d.RepoURL, d.AllowLocalRepo); err != nil {
			return err
		}
		if err := prepare(ctx, d); err != nil {
			return err
		}
		// Cheap early exit: don't spend minutes building a commit that is
		// already live. release checks again; that check is the one that counts.
		if err := refuseIfLive(ctx, d.CommitSHA); err != nil {
			return err
		}
		if err := build(d); err != nil {
			return err
		}
		return release(ctx, d)
	})
}

// locked runs fn holding the deploy lock, and records the final status in
// one place for every failure. A deploy that was rolled back keeps that
// status instead of plain "failed", and one whose commit was already live
// is "skipped".
func locked(d *Deployment, fn func(ctx context.Context) error) error {
	if s, _ := d.Snapshot(); s == "" {
		d.SetStatus(StatusQueued) // the webhook queue may already have done this
	}

	// In-process first (cheap, and keeps goroutines in "sling serve" in
	// order), then the file lock, which also covers other Sling processes.
	deployMu.Lock()
	defer deployMu.Unlock()
	unlock, err := acquireLock(lockPath())
	if err != nil {
		d.setError(err)
		d.SetStatus(StatusFailed)
		return err
	}
	defer unlock()

	// We hold the lock, so no other deploy is running. Any deploy the history
	// still shows as in progress belongs to a process that died mid-deploy.
	if r := currentRecorder(); r != nil {
		if n, err := r.MarkInterrupted(d.ID()); err != nil {
			fmt.Printf("⚠️  deploy history: %v\n", err)
		} else if n > 0 {
			fmt.Printf("Marked %d earlier deploy(s) as interrupted (their process stopped mid-deploy)\n", n)
		}
	}

	if err := fn(context.Background()); err != nil {
		d.setError(err)
		switch s, _ := d.Snapshot(); {
		case errors.Is(err, ErrAlreadyLive):
			d.SetStatus(StatusSkipped)
		case s != StatusRolledBack:
			d.SetStatus(StatusFailed)
		default:
			// rollback() set rolled_back before the error existed: save
			// again so the history keeps why it rolled back.
			d.save()
		}
		return err
	}
	return nil
}

// prepare fails fast if Docker or the proxy is broken, before any real work.
// It also creates sling-net, which the app container joins.
func prepare(ctx context.Context, d *Deployment) error {
	d.SetStatus(StatusPreparing)
	if err := proxy.Ensure(ctx); err != nil {
		return fmt.Errorf("proxy setup failed: %w", err)
	}
	return nil
}

// liveUpstream is what the proxy serves now ("" = nothing, the 503 page).
func liveUpstream(ctx context.Context) (string, error) {
	prev, err := proxy.CurrentUpstream(ctx)
	if err != nil {
		return "", fmt.Errorf("could not read current upstream: %w", err)
	}
	return prev, nil
}

// refuseIfLive returns ErrAlreadyLive when sha is what the proxy serves now.
func refuseIfLive(ctx context.Context, sha string) error {
	prev, err := liveUpstream(ctx)
	if err != nil {
		return err
	}
	if prev == upstreamFor(sha) {
		return fmt.Errorf("%s: %w", sha, ErrAlreadyLive)
	}
	return nil
}

func containerFor(sha string) string { return "app-" + sha }
func upstreamFor(sha string) string  { return containerFor(sha) + ":" + proxy.AppPort }
func imageFor(sha string) string     { return "paas-app:" + sha }

// build turns d.CommitSHA into the image paas-app:<sha> and sets d.ImageTag.
// Nothing here touches the proxy or a running container.
func build(d *Deployment) error {
	buildDir := filepath.Join(os.TempDir(), "paas-builds", d.CommitSHA)
	_ = os.RemoveAll(buildDir)
	// The image holds everything the app needs; the checkout is only an
	// input to docker build. Remove it however the build ends.
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

	tag := imageFor(d.CommitSHA)
	d.SetStatus(StatusBuilding)
	fmt.Printf("Building Docker image %s...\n", tag)
	if err := run.Streamed(buildTimeout, buildDir, "docker", "build", "-t", tag, "."); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}
	d.ImageTag = tag
	return nil
}

// release makes the existing image d.ImageTag live without downtime: start a
// fresh container, check it directly, switch the proxy, check through the
// proxy, watch a grace window, and roll back if any post-cutover check fails.
// It must run under the deploy lock.
func release(ctx context.Context, d *Deployment) error {
	if d.ImageTag != imageFor(d.CommitSHA) {
		return fmt.Errorf("release: image %q does not match commit %s", d.ImageTag, d.CommitSHA)
	}
	containerName := containerFor(d.CommitSHA)
	upstream := upstreamFor(d.CommitSHA)

	// Read what is live right before acting on it. Redeploying the live
	// commit is refused: the "docker rm -f" below would kill the container
	// that is serving traffic.
	prev, err := liveUpstream(ctx)
	if err != nil {
		return err
	}
	if prev == upstream {
		return fmt.Errorf("%s: %w", d.CommitSHA, ErrAlreadyLive)
	}
	d.PreviousUpstream = prev

	d.SetStatus(StatusStarting)
	fmt.Printf("Starting container on dynamic host port...\n")
	// Always a fresh container from the image: removes leftovers from a
	// failed attempt, and (for a rollback) the stopped old container.
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
	viaProxy := postCutoverHealth
	viaProxy.Transport = proxy.ProbeTransport()
	probeURL := proxy.ProbeURL() + "/health"
	fmt.Printf("Verifying through the proxy (%s)...\n", proxy.PublicURL)
	if err := health.WaitUntil(ctx, viaProxy, probeURL, health.ServedBy(proxy.UpstreamHeader, upstream), isRunning); err != nil {
		return rollback(ctx, d, prev, fmt.Errorf("post-cutover check failed: %w", err))
	}

	// Grace window: the old container is still running, so if the new one
	// falls over now, rolling back is a single proxy switch.
	d.SetStatus(StatusObserving)
	fmt.Printf("Observing for %v before retiring the previous version...\n", graceWindow)
	if err := health.Watch(ctx, viaProxy, probeURL, health.ServedBy(proxy.UpstreamHeader, upstream),
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
