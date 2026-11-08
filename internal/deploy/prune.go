package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mildlybrutal/sling-orchestrator/internal/run"
)

const keepImages = 3

var (
	containerNamePattern = regexp.MustCompile(`^app-[0-9a-f]{40}$`)
	imageTagPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func prune(liveUpstream, prevUpstream string) {
	live := upstreamContainer(liveUpstream)
	prev := upstreamContainer(prevUpstream)

	names, err := run.Capture(dockerTimeout, "", "docker", "ps", "-a",
		"--filter", "label=sling.managed=true", "--format", "{{.Names}}")
	if err != nil {
		fmt.Printf("prune : could not list containers: %v\n", err)
	} else {
		for _, name := range containersToRemove(lines(names), live, prev) {
			if _, err := run.Capture(cleanupTimeout, "", "docker", "rm", "-f", name); err != nil {
				fmt.Printf("prune: could not remove %s: %v\n", name, err)
			} else {
				fmt.Printf("Pruned container %s\n", name)
			}
		}
	}

	imgs, err := run.Capture(dockerTimeout, "", "docker", "images", "paas-app",
		"--format", "{{.CreatedAt}}\t{{.Tag}}")
	if err != nil {
		fmt.Printf("prune: could not list images: %v\n", err)
		return
	}
	inUse := map[string]bool{}
	for _, c := range []string{live, prev} {
		if c != "" {
			inUse[strings.TrimPrefix(c, "app-")] = true
		}
	}
	for _, tag := range imagesToRemove(lines(imgs), inUse, keepImages) {
		if _, err := run.Capture(cleanupTimeout, "", "docker", "rmi", "paas-app:"+tag); err != nil {
			fmt.Printf("⚠️  prune: could not remove image paas-app:%s: %v\n", tag[:7], err)
		} else {
			fmt.Printf("Pruned image paas-app:%s\n", tag[:7])
		}
	}
}

// upstreamContainer turns "app-<sha>:3000" into "app-<sha>", or "" if the
// value is empty or not an upstream Sling created.
func upstreamContainer(upstream string) string {
	if m := upstreamPattern.FindStringSubmatch(upstream); m != nil {
		return m[1]
	}
	return ""
}

// containersToRemove returns every Sling app container except live and prev.
// Names that don't look like ours are never returned, whatever their label.
func containersToRemove(names []string, live, prev string) []string {
	var out []string
	for _, n := range names {
		if n == live || n == prev || !containerNamePattern.MatchString(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// imagesToRemove takes "<created>\t<tag>" rows and returns the tags to
// delete: everything except the images in use and the keep newest others.
// Docker's CreatedAt ("2026-11-02 00:44:14 +0530 IST") sorts correctly as a
// string when all rows come from the same machine and time zone.
func imagesToRemove(rows []string, inUse map[string]bool, keep int) []string {
	type img struct{ created, tag string }
	var imgs []img
	for _, r := range rows {
		created, tag, ok := strings.Cut(r, "\t")
		if !ok || !imageTagPattern.MatchString(tag) {
			continue // "<none>" tags and anything not ours
		}
		imgs = append(imgs, img{created, tag})
	}
	sort.SliceStable(imgs, func(i, j int) bool { return imgs[i].created > imgs[j].created })

	var out []string
	kept := 0
	for _, im := range imgs {
		if inUse[im.tag] {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		out = append(out, im.tag)
	}
	return out
}

// removeBuildDir deletes a build directory once its image exists. It only
// touches paths under paas-builds, so a bad argument can't delete anything else.
func removeBuildDir(dir string) {
	root := filepath.Join(os.TempDir(), "paas-builds")
	if filepath.Dir(dir) != root {
		return
	}
	_ = os.RemoveAll(dir)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
