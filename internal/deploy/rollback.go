package deploy

import (
	"context"
	"fmt"

	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

// rollback points the proxy back at prev ("" = the 503 page) after cause.
// Both rollback paths (post-cutover check, grace window) go through here so
// they can't drift apart.
func rollback(ctx context.Context, d *Deployment, prev string, cause error) error {
	target := prev
	if target == "" {
		target = "no app (503 page)"
	}
	if err := proxy.SetUpstream(ctx, prev); err != nil {
		return fmt.Errorf("%w AND rollback to %s failed (%w): the proxy may be pointing at a container that is about to be removed",
			cause, target, err)
	}
	d.SetStatus(StatusRolledBack)
	return fmt.Errorf("%w; traffic restored to %s", cause, target)
}

// previousContainer returns the container behind prev, and false when there
// is nothing to retire: first deploy, same version redeployed, or a value
// Sling didn't create.
func previousContainer(prev, current string) (string, bool) {
	if prev == "" || prev == current {
		return "", false
	}
	name := upstreamContainer(prev)
	return name, name != ""
}

// retirePrevious stops (does not remove) the previous version. docker stop
// sends SIGTERM and waits up to 10s, so in-flight requests finish; docker
// rm -f would SIGKILL them. It reports whether a container was stopped.
func retirePrevious(prev, current string) bool {
	name, ok := previousContainer(prev, current)
	if !ok {
		if prev != "" && prev != current {
			fmt.Printf("⚠️  not retiring %q: not an upstream Sling created\n", prev)
		}
		return false
	}
	fmt.Printf("Retiring previous version %s (SIGTERM, up to 10s to drain)...\n", name)
	if _, err := run.Capture(dockerTimeout, "", "docker", "stop", "-t", "10", name); err != nil {
		fmt.Printf("⚠️  could not stop %s: %v (the new version is live; stop it by hand)\n", name, err)
		return false
	}
	return true
}
