//go:build unix

package ledger

import (
	"os"
	"syscall"
)

// lock takes an exclusive advisory lock, blocking until it is available.
func lock(f *os.File) (func(), error) {
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }, nil
}
