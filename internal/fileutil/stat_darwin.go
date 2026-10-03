//go:build darwin

package fileutil

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// InfoOf extracts Info from the result of Lstat, Stat or File.Stat.
func InfoOf(fi os.FileInfo) (Info, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, fmt.Errorf("no file status available for %q", fi.Name())
	}
	return Info{
		ID:     FileID{Dev: uint64(uint32(st.Dev)), Ino: st.Ino},
		Size:   st.Size,
		Blocks: int64(st.Blocks) * 512,
		Mode:   fi.Mode(),
		Nlink:  uint64(st.Nlink),
		UID:    st.Uid,
		GID:    st.Gid,
		Atime:  time.Unix(st.Atimespec.Unix()),
		Mtime:  time.Unix(st.Mtimespec.Unix()),
		Ctime:  time.Unix(st.Ctimespec.Unix()),
	}, nil
}
