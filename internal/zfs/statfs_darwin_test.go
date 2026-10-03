package zfs

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIsZFS(t *testing.T) {
	named := func(name string) *unix.Statfs_t {
		var st unix.Statfs_t
		copy(st.Fstypename[:], name)
		return &st
	}
	require.True(t, isZFS(named("zfs")))
	require.False(t, isZFS(named("apfs")))
	require.False(t, isZFS(named("zfsx")))

	// macOS boots from APFS.
	on, err := OnZFS("/")
	require.NoError(t, err)
	require.False(t, on, "the system volume isn't ZFS")
}
