//go:build linux || darwin

package fileutil

import (
	"os"
	"time"
)

// SetTimes sets the access and modified times of the open file or folder f, to the nanosecond.
// Unlike os.Chtimes it changes the file that is open, never whatever its name leads to by now, so a
// symlink or another file put in its place can't be given the times instead.
func SetTimes(f *os.File, atime, mtime time.Time) error {
	return withFd(f, func(fd int) error { return setTimesFd(fd, atime, mtime) })
}
