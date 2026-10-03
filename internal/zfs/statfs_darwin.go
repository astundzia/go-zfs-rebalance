package zfs

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// OnZFS reports whether path is on a ZFS filesystem. It asks the kernel, so it
// works whether or not the zfs tools are installed.
func OnZFS(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return isZFS(&st), nil
}

// isZFS reports whether st is for ZFS. OpenZFS on macOS names itself "zfs".
func isZFS(st *unix.Statfs_t) bool { return unix.ByteSliceToString(st.Fstypename[:]) == "zfs" }
