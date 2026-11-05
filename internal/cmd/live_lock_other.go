//go:build !unix

package cmd

import "os"

// flock is a unix call, and nothing here replaces it: every acquire succeeds,
// so on this platform two live checks naming one lock run at once against the
// same hosts. Said here because the platform that cannot enforce it is the one
// nobody tests on.
func tryLiveLock(*os.File) (bool, error) { return true, nil }

func releaseLiveLock(*os.File) {}
