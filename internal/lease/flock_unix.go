//go:build unix

package lease

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// The lock lives on the open file description, not on the path and not on the
// process. Two consequences drover depends on: the kernel drops it however the
// holder dies, so a crashed drive leaves no state to clean up; and a second
// os.OpenFile of the same path in the same process is refused too, which is
// what lets a test hold a lease and contend with it honestly.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func lockShared(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// inheritable clears close-on-exec, which Go sets on every descriptor it opens.
func inheritable(fd uintptr) error {
	_, err := unix.FcntlInt(fd, unix.F_SETFD, 0)
	return err
}

func closeOnExec(fd uintptr) { syscall.CloseOnExec(int(fd)) } // #nosec G115 -- a descriptor number, far below MaxInt
