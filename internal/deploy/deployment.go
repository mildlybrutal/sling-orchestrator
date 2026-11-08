// Package deploy runs Sling's blue/green pipeline: clone an exact commit,
// build it, start it beside the live version, health-check it, switch the
// proxy, watch a grace window, then retire the old version and prune.
package deploy

import (
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/mildlybrutal/sling-orchestrator/internal/health"
	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
)

// Deployment is one attempt to put one commit live.
type Deployment struct {
	// Kind is KindDeploy or KindRollback ("" means deploy).
	Kind      string
	RepoURL   string
	CommitSHA string

	// AllowLocalRepo lets RepoURL be a local path like ../sling. Only the CLI
	// sets it; webhook deploys always clone from GitHub.
	AllowLocalRepo bool

	ImageTag    string
	ContainerID string
	HostPort    string

	// PreviousUpstream is what the proxy served before this deploy
	// ("app-<sha>:3000"), or "" if nothing was live. After the grace window
	// it is stopped (not removed), so a manual rollback is start + switch.
	PreviousUpstream string
	PreviousStopped  bool

	// Status tracking (see status.go). Guarded by mu because the dashboard
	// API will read these while the pipeline goroutine writes them.
	mu      sync.Mutex
	status  Status
	history []StatusChange
	err     string // why it ended badly, if it did
}

const (
	KindDeploy   = "deploy"
	KindRollback = "rollback"
)

const (
	cloneTimeout   = 1 * time.Minute
	gitTimeout     = 30 * time.Second
	buildTimeout   = 10 * time.Minute
	dockerTimeout  = 30 * time.Second
	cleanupTimeout = 30 * time.Second

	// After cutover, keep the previous version running this long and roll
	// back if graceMaxFailures proxy probes in a row fail.
	graceWindow      = 30 * time.Second
	graceMaxFailures = 3
)

// ErrAlreadyLive means the requested commit is already serving traffic, so
// there is nothing to do. Callers treat it as "skipped", not as a failure.
var ErrAlreadyLive = errors.New("commit is already live")

var (
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	allowedRepoHosts   = map[string]bool{"github.com": true}

	// An upstream Sling itself created: "app-<40 hex>:3000". Anything read
	// back from Caddy is checked against this before it reaches docker.
	upstreamPattern = regexp.MustCompile(`^(app-[0-9a-f]{40}):` + proxy.AppPort + `$`)

	// One deploy at a time inside this process. The file lock in lock.go
	// extends that to every Sling process on the host. There is a single
	// app for now; multiple apps will turn both into per-app locks.
	deployMu sync.Mutex

	// After cutover the check goes through the proxy, so it is shorter: the
	// app already passed its direct check.
	postCutoverHealth = health.Config{
		Interval:       500 * time.Millisecond,
		Deadline:       10 * time.Second,
		RequestTimeout: 2 * time.Second,
		RequiredStreak: 3,
	}
)
