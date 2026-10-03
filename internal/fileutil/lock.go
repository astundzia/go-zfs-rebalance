//go:build linux || darwin

package fileutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"syscall"

	"golang.org/x/sys/unix"
)

// flockNB takes an exclusive flock on fd without waiting. Locks taken through different opens of
// the same file conflict, even within one process, and the kernel drops them when the process
// dies, so a held lock means a run is still using the file.
func flockNB(fd int) error {
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// lockTemp returns a duplicate of the new temporary file f's descriptor, locked where the
// filesystem has locks, or nil if f's descriptor couldn't be duplicated. The duplicate lets f be
// closed (and its close error checked) before the swap while the lock lasts until the temporary
// names are gone, and lets the new file's times be put right after the swap (see keepTimes).
// Hardlinks made to the file later share its lock. Locking is a courtesy to other runs (see
// RemoveStaleTemp), so a filesystem without locks doesn't stop the rewrite.
func lockTemp(f *os.File) *os.File {
	var held *os.File
	_ = withFd(f, func(fd int) error {
		dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return err
		}
		_ = flockNB(dup)
		held = os.NewFile(uintptr(dup), f.Name())
		return nil
	})
	return held
}

// lockSource locks the original, open as src, for as long as it stays open, so two runs (or a run
// and another program that locks the files it works on) never work on the same file at once. A
// file someone else has locked gives ErrBusy. A filesystem without locks (or one, like NFS, that
// only locks files open for writing) doesn't stop the rewrite: the final checks before the swap
// still catch any change.
func lockSource(src *os.File) error {
	if err := withFd(src, flockNB); errors.Is(err, unix.EWOULDBLOCK) {
		return failKind(ErrBusy, "", nil)
	}
	return nil
}

// RemoveStaleTemp removes rel, one of the temporary files ReplaceInPlace and ReplaceGroup create,
// if it was left behind by a run that has stopped. rel must have a temporary file's name (see
// IsTempName).
//
// removed is false with a nil error when a run that is still going (this one or another) holds the
// file's lock: the file is in use and was left alone. When rel no longer exists the error matches
// fs.ErrNotExist, and when it is not a regular file the error matches ErrNotRegular; either way
// there was nothing of ours to remove.
func RemoveStaleTemp(root *os.Root, rel string) (removed bool, err error) {
	rel = path.Clean(rel)
	if !IsTempName(path.Base(rel)) {
		return false, fmt.Errorf("%q isn't the name of a temporary file this tool makes", path.Base(rel))
	}
	info, err := Lstat(root, rel)
	if err != nil {
		return false, err
	}
	if !info.Mode.IsRegular() {
		return false, failKind(ErrNotRegular, "", nil)
	}
	// O_NONBLOCK keeps a pipe swapped in at the last moment from blocking the open.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	opened, err := statFile(f)
	if err != nil {
		return false, err
	}
	if opened.ID != info.ID || !opened.Mode.IsRegular() {
		return false, errors.New("it was replaced by another file while it was being checked")
	}
	if err := withFd(f, flockNB); errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("couldn't check whether another run is using it: %w", err)
	}

	// Holding the lock, make sure the name still leads to the file that was checked.
	now, err := Lstat(root, rel)
	if err != nil {
		return false, err
	}
	if now.ID != info.ID {
		return false, errors.New("it was replaced by another file while it was being checked")
	}
	if err := root.Remove(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		clearACL(root, rel, info.ID)
		if err := root.Remove(rel); err != nil {
			return false, err
		}
	}
	return true, nil
}
