//go:build !unix

package idalloc

// Lock is a no-op where flock is unavailable. The body is still created with
// O_EXCL, so two allocations that collide on one filename fail rather than
// overwrite; two that collide on an id with different slugs are not caught.
func Lock(_ string) (func(), error) {
	return func() {}, nil
}
