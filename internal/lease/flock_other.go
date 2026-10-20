//go:build !unix

package lease

import (
	"errors"
	"os"
)

// Advisory file locking is a unix concept, and nothing here enforces anything.
// Every acquire succeeds and every read reports the tree free, so on this
// platform two drover commands reach one checkout as if there were no lease:
// a drive and a finish in two terminals still move the tree
// under each other, no command refuses, and `drover status` names nobody. The
// payload is written and never believed, because the lock is what proves the
// holder is alive and there is no lock here.
//
// Said here rather than discovered in a tree: the platform that cannot enforce
// this is the one nobody tests on.
func lockExclusive(*os.File) error { return nil }

func lockShared(*os.File) error { return nil }

func unlock(*os.File) error { return nil }

func inheritable(uintptr) error { return errors.New("a lease survives an exec only on a unix host") }

func closeOnExec(uintptr) {}
