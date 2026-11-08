package deploy

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The second holder must wait until the first releases. Each acquireLock
// opens its own file description, which is exactly how two separate Sling
// processes contend, so this exercises the real cross-process behavior.
func TestLockExcludesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sling.lock")

	unlock1, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan func(), 1)
	go func() {
		unlock2, err := acquireLock(path)
		if err != nil {
			t.Error(err)
			got <- func() {}
			return
		}
		got <- unlock2
	}()

	select {
	case <-got:
		t.Fatal("second holder got the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
	}

	unlock1()
	select {
	case unlock2 := <-got:
		unlock2()
	case <-time.After(2 * time.Second):
		t.Fatal("second holder never got the lock after release")
	}
}

func TestLockReusableAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sling.lock")
	for i := 0; i < 3; i++ {
		unlock, err := acquireLock(path)
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		unlock()
	}
}

func TestNamesForSHA(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	if got := containerFor(sha); got != "app-"+sha {
		t.Errorf("containerFor = %q", got)
	}
	if got := upstreamFor(sha); upstreamContainer(got) != containerFor(sha) {
		t.Errorf("upstreamFor = %q is not an upstream Sling recognizes", got)
	}
	if got := imageFor(sha); got != "paas-app:"+sha {
		t.Errorf("imageFor = %q", got)
	}
}

// release only ever runs an image built for its own commit.
func TestReleaseRejectsMismatchedImage(t *testing.T) {
	d := &Deployment{CommitSHA: "0123456789abcdef0123456789abcdef01234567", ImageTag: "paas-app:latest"}
	if err := release(context.Background(), d); err == nil {
		t.Fatal("release accepted an image for a different commit")
	}
}
