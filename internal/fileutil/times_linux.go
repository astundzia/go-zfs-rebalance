//go:build linux

package fileutil

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// setTimesFd sets the access and modified times of the file open as fd. utimensat with no path
// works on fd itself, as futimens(3) does; x/sys/unix has no wrapper for that form.
func setTimesFd(fd int, atime, mtime time.Time) error {
	var ts [2]unix.Timespec
	var err error
	if ts[0], err = unix.TimeToTimespec(atime); err != nil {
		return err
	}
	if ts[1], err = unix.TimeToTimespec(mtime); err != nil {
		return err
	}
	_, _, e := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
