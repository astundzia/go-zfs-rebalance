//go:build darwin

package fileutil

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// setTimesFd sets the access and modified times of the file open as fd with fsetattrlist, which
// keeps nanoseconds (futimes only takes microseconds).
func setTimesFd(fd int, atime, mtime time.Time) error {
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	// The values follow each other in the order of their attribute bits: modified, then accessed.
	var buf struct{ mtime, atime unix.Timespec }
	var err error
	if buf.mtime, err = unix.TimeToTimespec(mtime); err != nil {
		return err
	}
	if buf.atime, err = unix.TimeToTimespec(atime); err != nil {
		return err
	}
	//lint:ignore SA1019 x/sys/unix has no fsetattrlist wrapper (see readACL).
	_, _, e := unix.Syscall6(unix.SYS_FSETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf)), unsafe.Sizeof(buf), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
