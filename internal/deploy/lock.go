package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The deploy lock is a file lock (flock), not just a mutex, because the
// processes that can switch traffic are separate programs: "sling serve" and
// any "sling ..." command run from a terminal. A mutex only protects
// goroutines inside one process.
//
// The kernel releases a flock when the holder exits, even on a crash or
// kill -9, so a dead Sling can never leave the lock stuck.

// lockPath is where the lock file lives. $XDG_RUNTIME_DIR is a per-user,
// memory-backed directory on most Linux systems; otherwise fall back to the
// temp dir, with the uid in the name so two users don't share one lock.
func lockPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "sling.lock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sling-%d.lock", os.Getuid()))
}

// acquireLock takes the deploy lock at path, waiting while another process
// (or goroutine) holds it, and returns the function that releases it.
func acquireLock(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deploy lock %s: %w", path, err)
	}
	fd := int(f.Fd())

	// Try without waiting first, only so we can tell the user why we pause.
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		fmt.Println("Another Sling deploy is in progress; waiting for it to finish...")
		err = flockRetryEINTR(fd, syscall.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		f.Close()
	}, nil
}

// flockRetryEINTR retries a blocking flock interrupted by a signal.
func flockRetryEINTR(fd, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
