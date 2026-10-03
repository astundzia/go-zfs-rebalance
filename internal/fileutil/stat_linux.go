//go:build linux

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
		ID:     FileID{Dev: uint64(st.Dev), Ino: uint64(st.Ino)},
		Size:   st.Size,
		Blocks: int64(st.Blocks) * 512,
		Mode:   fi.Mode(),
		Nlink:  uint64(st.Nlink),
		UID:    st.Uid,
		GID:    st.Gid,
		Atime:  time.Unix(st.Atim.Unix()),
		Mtime:  time.Unix(st.Mtim.Unix()),
		Ctime:  time.Unix(st.Ctim.Unix()),
	}, nil
}
