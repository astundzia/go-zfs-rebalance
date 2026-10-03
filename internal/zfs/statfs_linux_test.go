package zfs

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIsZFS(t *testing.T) {
	require.True(t, isZFS(&unix.Statfs_t{Type: zfsSuperMagic}))
	require.False(t, isZFS(&unix.Statfs_t{Type: unix.EXT4_SUPER_MAGIC}))
	require.False(t, isZFS(&unix.Statfs_t{Type: unix.TMPFS_MAGIC}))
}
