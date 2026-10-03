package rebalance

import (
	"testing"

	"golang.org/x/sys/unix"
)

// noexec reports whether dir is on a filesystem mounted noexec.
func noexec(t *testing.T, dir string) bool {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return st.Flags&unix.MNT_NOEXEC != 0
}
