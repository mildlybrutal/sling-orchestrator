package deploy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mildlybrutal/sling-orchestrator/internal/proxy"
	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

// Version is one commit Sling knows about on this host: it has a container
// (running or stopped), an image, or both.
type Version struct {
	SHA              string
	Container        string // "" if no container exists
	ContainerStatus  string // docker's words: "Up 3 minutes", "Exited (0) 2 hours ago"
	ContainerCreated string
	HasImage         bool
	ImageCreated     string // CreatedAt, sortable as a string (see imagesToRemove)
	ImageAge         string // "2 hours ago"
	Live             bool
}

// State is what is deployed on this host right now.
type State struct {
	LiveUpstream string // what Caddy routes to; "" = nothing (503 page)
	LiveErr      error  // set if Caddy could not be asked; LiveUpstream is then unknown
	Versions     []Version
	// RollbackTarget is the SHA "sling rollback" would go back to: the most
	// recent non-live version whose container was kept (the previous
	// version) and whose image still exists. "" if there is none.
	RollbackTarget string
}

var ErrNoRollbackTarget = errors.New("no previous version to roll back to")

var shaPrefixPattern = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// CurrentState asks Caddy and Docker what is deployed. It changes nothing
// and does not start the proxy, so it is safe to run at any time.
func CurrentState(ctx context.Context) (State, error) {
	live, liveErr := proxy.CurrentUpstream(ctx)

	containers, err := run.Capture(dockerTimeout, "", "docker", "ps", "-a",
		"--filter", "label=sling.managed=true",
		"--format", "{{.Names}}\t{{.CreatedAt}}\t{{.Status}}")
	if err != nil {
		return State{}, fmt.Errorf("could not list containers: %w", err)
	}
	images, err := run.Capture(dockerTimeout, "", "docker", "images", "paas-app",
		"--format", "{{.Tag}}\t{{.CreatedAt}}\t{{.CreatedSince}}")
	if err != nil {
		return State{}, fmt.Errorf("could not list images: %w", err)
	}

	st := buildState(live, lines(containers), lines(images))
	st.LiveErr = liveErr
	if liveErr != nil {
		// Without knowing what's live, don't offer anything to roll back to.
		st.LiveUpstream, st.RollbackTarget = "", ""
		for i := range st.Versions {
			st.Versions[i].Live = false
		}
	}
	return st, nil
}

// buildState merges "name\tcreated\tstatus" container rows and
// "tag\tcreated\tage" image rows into one entry per commit. Rows that don't
// look like Sling's are ignored, whatever their labels say.
func buildState(liveUpstream string, containerRows, imageRows []string) State {
	byS := map[string]*Version{}
	get := func(sha string) *Version {
		if v, ok := byS[sha]; ok {
			return v
		}
		v := &Version{SHA: sha}
		byS[sha] = v
		return v
	}

	for _, r := range containerRows {
		f := strings.SplitN(r, "\t", 3)
		if len(f) != 3 || !containerNamePattern.MatchString(f[0]) {
			continue
		}
		v := get(strings.TrimPrefix(f[0], "app-"))
		v.Container, v.ContainerCreated, v.ContainerStatus = f[0], f[1], f[2]
	}
	for _, r := range imageRows {
		f := strings.SplitN(r, "\t", 3)
		if len(f) != 3 || !imageTagPattern.MatchString(f[0]) {
			continue
		}
		v := get(f[0])
		v.HasImage, v.ImageCreated, v.ImageAge = true, f[1], f[2]
	}

	st := State{LiveUpstream: liveUpstream}
	liveSHA := strings.TrimPrefix(upstreamContainer(liveUpstream), "app-")
	for _, v := range byS {
		v.Live = v.SHA == liveSHA && liveSHA != ""
		st.Versions = append(st.Versions, *v)
	}

	// Live first, then newest first (by container if there is one, else image).
	newest := func(v Version) string {
		if v.ContainerCreated != "" {
			return v.ContainerCreated
		}
		return v.ImageCreated
	}
	sort.SliceStable(st.Versions, func(i, j int) bool {
		a, b := st.Versions[i], st.Versions[j]
		if a.Live != b.Live {
			return a.Live
		}
		if newest(a) != newest(b) {
			return newest(a) > newest(b)
		}
		return a.SHA < b.SHA
	})

	for _, v := range st.Versions {
		if !v.Live && v.Container != "" && v.HasImage {
			st.RollbackTarget = v.SHA
			break // sorted newest first
		}
	}
	return st
}

// resolveTarget turns what the user typed after "sling rollback" into a
// full SHA whose image exists. "" means "the previous version"; otherwise a
// full SHA or a unique prefix of at least 4 hex characters.
func (st State) resolveTarget(arg string) (string, error) {
	if arg == "" {
		if st.RollbackTarget == "" {
			return "", fmt.Errorf("%w (run \"sling status\" to see which images exist, then \"sling rollback <sha>\")", ErrNoRollbackTarget)
		}
		return st.RollbackTarget, nil
	}
	if !shaPrefixPattern.MatchString(arg) {
		return "", fmt.Errorf("invalid commit %q: give 4 to 40 lowercase hex characters", arg)
	}
	var matches []string
	for _, v := range st.Versions {
		if v.HasImage && strings.HasPrefix(v.SHA, arg) {
			matches = append(matches, v.SHA)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no image for %s on this host (pruned or never built); deploy it again with: sling <repo> <full-sha>", arg)
	default:
		return "", fmt.Errorf("%q matches %d images; give more characters", arg, len(matches))
	}
}

// Rollback puts an earlier, already-built version live. d.CommitSHA may be
// "" (the previous version) or a full or short SHA; on return it holds the
// full SHA that was used. It is release() alone: no clone, no build, but
// every health check, the proxy verification and the grace window.
func Rollback(d *Deployment) error {
	d.Kind = KindRollback
	return locked(d, func(ctx context.Context) error {
		if err := prepare(ctx, d); err != nil {
			return err
		}
		// Read state under the lock, so it can't change before release acts.
		st, err := CurrentState(ctx)
		if err != nil {
			return err
		}
		if st.LiveErr != nil {
			return fmt.Errorf("could not read current upstream: %w", st.LiveErr)
		}
		sha, err := st.resolveTarget(d.CommitSHA)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.CommitSHA = sha
		d.mu.Unlock()
		d.ImageTag = imageFor(sha)
		fmt.Printf("Rolling back to %s (image %s, no rebuild)...\n", sha[:7], d.ImageTag)
		return release(ctx, d)
	})
}
