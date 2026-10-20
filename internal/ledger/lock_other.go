//go:build !unix

package ledger

import "os"

// lock is a no-op where flock is unavailable. Appends of a single short line to
// an O_APPEND handle are the only writes this package makes, so the exposure is
// interleaved rows under concurrent writers, not a corrupted file.
func lock(_ *os.File) (func(), error) {
	return func() {}, nil
}
