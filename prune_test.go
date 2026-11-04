package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sha(c string) string { return strings.Repeat(c, 40) }

func TestContainersToRemove(t *testing.T) {
	live, prev := "app-"+sha("a"), "app-"+sha("b")
	names := []string{live, prev, "app-" + sha("c"), "app-" + sha("d"), "sling-caddy", "app-oops"}
	got := containersToRemove(names, live, prev)
	want := []string{"app-" + sha("c"), "app-" + sha("d")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestContainersToRemoveFirstDeploy(t *testing.T) {
	// No previous version: only the live one is kept.
	live := "app-" + sha("a")
	got := containersToRemove([]string{live, "app-" + sha("c")}, live, "")
	if !reflect.DeepEqual(got, []string{"app-" + sha("c")}) {
		t.Fatalf("got %v", got)
	}
}

func TestImagesToRemove(t *testing.T) {
	rows := []string{
		"2026-11-02 10:00:00 +0530 IST\t" + sha("6"),
		"2026-11-02 09:00:00 +0530 IST\t" + sha("5"),
		"2026-11-02 08:00:00 +0530 IST\t" + sha("4"),
		"2026-11-02 07:00:00 +0530 IST\t" + sha("3"),
		"2026-11-02 06:00:00 +0530 IST\t" + sha("2"),
		"2026-11-01 05:00:00 +0530 IST\t" + sha("1"), // oldest, but live
		"2026-11-02 11:00:00 +0530 IST\t<none>",
	}
	inUse := map[string]bool{sha("1"): true}
	got := imagesToRemove(rows, inUse, 3)
	// keep: 1 (in use) + 6,5,4 (newest 3 others); remove 3 and 2
	want := []string{sha("3"), sha("2")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestImagesToRemoveUnsortedInput(t *testing.T) {
	rows := []string{
		"2026-11-02 06:00:00 +0530 IST\t" + sha("2"),
		"2026-11-02 08:00:00 +0530 IST\t" + sha("4"),
		"2026-11-02 07:00:00 +0530 IST\t" + sha("3"),
	}
	got := imagesToRemove(rows, nil, 2)
	if !reflect.DeepEqual(got, []string{sha("2")}) {
		t.Fatalf("got %v, want only the oldest", got)
	}
}

func TestUpstreamContainer(t *testing.T) {
	if got := upstreamContainer("app-" + sha("a") + ":3000"); got != "app-"+sha("a") {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", "app-x:3000", "app-" + sha("a") + ":80"} {
		if got := upstreamContainer(bad); got != "" {
			t.Fatalf("upstreamContainer(%q) = %q, want empty", bad, got)
		}
	}
}

func TestRemoveBuildDirStaysInsideRoot(t *testing.T) {
	root := filepath.Join(os.TempDir(), "paas-builds")
	inside := filepath.Join(root, "test-"+sha("e"))
	outside := t.TempDir()
	os.MkdirAll(inside, 0o755)
	keep := filepath.Join(outside, "keep")
	os.WriteFile(keep, []byte("x"), 0o644)

	removeBuildDir(inside)
	removeBuildDir(outside)
	removeBuildDir(root)

	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("build dir inside paas-builds should be removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("a path outside paas-builds must never be removed")
	}
}
