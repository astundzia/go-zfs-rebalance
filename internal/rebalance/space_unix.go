//go:build linux || darwin

package rebalance

import (
	"os"

	"golang.org/x/sys/unix"
)

// folderFreeBytes returns how many bytes can still be written in the filesystem holding dir
// (relative to root), as an unprivileged user sees it. On ZFS this already takes the dataset's
// quotas into account, but not per-user or per-group ones.
func folderFreeBytes(root *os.Root, dir string) (uint64, error) {
	f, err := root.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	conn, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var st unix.Statfs_t
	var statErr error
	if err := conn.Control(func(fd uintptr) { statErr = unix.Fstatfs(int(fd), &st) }); err != nil {
		return 0, err
	}
	if statErr != nil {
		return 0, &os.PathError{Op: "statfs", Path: dir, Err: statErr}
	}
	return st.Bavail * blockSize(&st), nil
}
