//go:build !linux && !darwin

package database

// AcquireLock is a no-op on unsupported platforms, where the tool refuses to
// run before it would need the lock.
func AcquireLock(dir string) (release func() error, err error) {
	return func() error { return nil }, nil
}
