//go:build linux || darwin

package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// AcquireLock takes the global run lock (an exclusive flock on dir/run.lock),
// creating dir and the lock file if needed. It returns ErrLocked straight away
// if another run holds the lock. The lock is released when release is called
// or the process exits; the lock file itself is left in place.
func AcquireLock(dir string) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("couldn't create the folder %q for the run lock: %w", dir, err)
	}
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("couldn't open the run lock %q: %w", path, err)
	}
	fd := int(f.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w; wait for it to finish, then try again", ErrLocked)
		}
		return nil, fmt.Errorf("couldn't take the run lock %q: %w", path, err)
	}

	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseErr = errors.Join(unix.Flock(fd, unix.LOCK_UN), f.Close())
		})
		return releaseErr
	}, nil
}
