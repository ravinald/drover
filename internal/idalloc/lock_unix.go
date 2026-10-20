//go:build unix

package idalloc

import (
	"os"
	"syscall"
)

// Lock takes an exclusive advisory lock on dir, blocking until it is available.
//
// The directory itself is locked rather than a file inside it. A lock file in
// the backlog would be one more file every sync and overlay snapshot carries,
// and a review worktree reaches the backlog through a link, which resolves to
// the same directory and so to the same lock.
func Lock(dir string) (func(), error) {
	f, err := os.Open(dir) // #nosec G304 -- the project's own backlog directory
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
