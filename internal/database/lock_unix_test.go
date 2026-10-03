//go:build linux || darwin

package database

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// flock locks belong to an open file description, so a second AcquireLock in
// the same process (which opens the file again) conflicts just like another
// process would.
func TestAcquireLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	requireMode(t, dir, 0o700)
	lockPath := filepath.Join(dir, "run.lock")
	requireMode(t, lockPath, 0o600)

	_, err = AcquireLock(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second AcquireLock err = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "another rebalance is already running") {
		t.Errorf("error %q should say another rebalance is running", err)
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := release(); err != nil {
		t.Errorf("second release should be a no-op, got %v", err)
	}
	requireMode(t, lockPath, 0o600) // the lock file stays in place

	release2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("release: %v", err)
	}
}
