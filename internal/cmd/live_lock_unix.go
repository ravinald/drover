//go:build unix

package cmd

import (
	"errors"
	"os"
	"syscall"
)

// tryLiveLock takes the flock without waiting. false with a nil error means
// another process holds it.
func tryLiveLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a file descriptor fits an int
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func releaseLiveLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits an int
}
