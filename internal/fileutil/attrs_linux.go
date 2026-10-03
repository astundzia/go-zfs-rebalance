//go:build linux

package fileutil

import (
	"errors"
	"os"
	"path"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openNoATime stops reading the original from updating its access time, so a file that ends up
// skipped or failed keeps its atime. Linux allows it for the file's owner and for root.
const openNoATime = unix.O_NOATIME

const immutableHint = "chattr +i / +a"

// Inode flags as FS_IOC_GETFLAGS reports them (lsattr, chattr).
const (
	fsSyncFl        = 0x00000008 // S
	fsImmutableFl   = 0x00000010 // i
	fsAppendFl      = 0x00000020 // a
	fsNodumpFl      = 0x00000040 // d
	fsNoatimeFl     = 0x00000080 // A
	fsProjinheritFl = 0x20000000 // P

	// copiedFlags are the chattr flags that are copied and checked. ZFS reports only nodump and
	// project inheritance besides immutable and append-only, which make the file be left alone;
	// the others are kept on filesystems that have them. Flags a filesystem manages itself (such
	// as ext4's extents flag) are left as it sets them on the new file.
	copiedFlags = fsSyncFl | fsNodumpFl | fsNoatimeFl | fsProjinheritFl
)

// fsxattr is the kernel's struct fsxattr, read and written with FS_IOC_FSGETXATTR and
// FS_IOC_FSSETXATTR. Only the project ID and the project-inheritance flag are copied.
type fsxattr struct {
	xflags, extsize, nextents, projid, cowextsize uint32
	_                                             [8]byte
}

const (
	fsxattrSize = 28

	// projInheritX is project inheritance in fsx_xflags: FS_XFLAG_PROJINHERIT, or FS_PROJINHERIT_FL
	// on OpenZFS before 2.2, which put chattr flags there. Neither value means anything else in
	// the other encoding, and the flags are always written back in the encoding they were read in.
	projInheritX = 0x00000200 | fsProjinheritFl
)

// OpenZFS DOS attributes (include/sys/fs/zfs.h), read and written with ZFS_IOC_GETDOSFLAGS and
// ZFS_IOC_SETDOSFLAGS from OpenZFS 2.2 on. TrueNAS's SMB server keeps the Windows read-only,
// hidden, system and archive attributes here.
const (
	zfsReadonly   = 1 << 32
	zfsHidden     = 1 << 33
	zfsSystem     = 1 << 34
	zfsArchive    = 1 << 35
	zfsImmutable  = 1 << 36
	zfsNounlink   = 1 << 37
	zfsAppendonly = 1 << 38
	zfsNodump     = 1 << 39
	zfsReparse    = 1 << 43
	zfsOffline    = 1 << 44
	zfsSparse     = 1 << 45

	// zfsDOSVisible is ZFS_DOS_FL_USER_VISIBLE: every attribute the two ioctls read and write.
	zfsDOSVisible = zfsImmutable | zfsAppendonly | zfsNounlink | zfsArchive | zfsNodump |
		zfsSystem | zfsHidden | zfsReadonly | zfsReparse | zfsOffline | zfsSparse

	zfsSuperMagic = 0x2fc12fc1
)

// ioctl request numbers, built like the kernel's _IOR and _IOW macros. The direction bits differ
// between architectures, so they are taken from x/sys's FS_IOC_GETFLAGS (an _IOR) and
// FS_IOC_SETFLAGS (an _IOW); the sizes used here fit the smallest (13-bit) size field.
const (
	iocRead  = unix.FS_IOC_GETFLAGS &^ (1<<29 - 1)
	iocWrite = unix.FS_IOC_SETFLAGS &^ (1<<29 - 1)

	fsIocFSGetXattr   = iocRead | fsxattrSize<<16 | 'X'<<8 | 31
	fsIocFSSetXattr   = iocWrite | fsxattrSize<<16 | 'X'<<8 | 32
	zfsIocGetDOSFlags = iocRead | 8<<16 | 0x83<<8 | 1
	zfsIocSetDOSFlags = iocWrite | 8<<16 | 0x83<<8 | 2
)

// fileAttrs are the Linux inode attributes kept besides the mode and extended attributes. Each has*
// field is false when the filesystem can't report that attribute, so there is nothing to copy.
type fileAttrs struct {
	flags    uint32
	hasFlags bool
	fsx      fsxattr
	hasFsx   bool
	dos      uint64
	hasDOS   bool
}

func ioctlPtr(fd int, req uint, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// supported sorts the result of reading an attribute: "this filesystem doesn't have it" is not an
// error, just nothing to copy.
func supported(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOTTY), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return false, nil
	}
	return false, err
}

// readFileAttrs returns f's chattr flags, project ID and, on ZFS, DOS attributes.
func readFileAttrs(f *os.File) (fileAttrs, error) {
	var a fileAttrs
	err := withFd(f, func(fd int) error {
		var err error
		a.flags, err = unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		if a.hasFlags, err = supported(err); err != nil {
			return failKind(ErrMetadata, "chattr attributes: "+reason(err), err)
		}
		if a.hasFsx, err = supported(ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&a.fsx))); err != nil {
			return failKind(ErrMetadata, "project ID: "+reason(err), err)
		}
		// The DOS ioctls' numbers are only meaningful to ZFS, so they are never sent elsewhere.
		var st unix.Statfs_t
		if unix.Fstatfs(fd, &st) != nil || st.Type != zfsSuperMagic {
			return nil
		}
		if a.hasDOS, err = supported(ioctlPtr(fd, zfsIocGetDOSFlags, unsafe.Pointer(&a.dos))); err != nil {
			return failKind(ErrMetadata, "Windows attributes: "+reason(err), err)
		}
		return nil
	})
	return a, err
}

// protected returns an ErrImmutable error if the original can't be replaced because it is marked
// immutable, append-only or (ZFS) undeletable. Renaming over such a file fails, so it is refused
// before anything is copied.
func (a fileAttrs) protected() error {
	switch {
	case a.hasFlags && a.flags&(fsImmutableFl|fsAppendFl) != 0,
		a.hasDOS && a.dos&(zfsImmutable|zfsAppendonly) != 0:
		return failKind(ErrImmutable, "", nil)
	case a.hasDOS && a.dos&zfsNounlink != 0:
		return &failure{kind: ErrImmutable.(*sentinel),
			what: "it's protected from being deleted (the ZFS nounlink attribute), so it was left alone"}
	}
	return nil
}

// prepareFileAttrs gives the empty copy tmp the original's project ID and project-inheritance flag,
// so the data written into it counts against the same project quota as the original.
func prepareFileAttrs(root *os.Root, names []string, tmp *os.File, want fileAttrs) error {
	if !want.hasFsx {
		return nil
	}
	return withFd(tmp, func(fd int) error {
		var cur fsxattr
		if err := ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&cur)); err != nil {
			return failKind(ErrMetadata, "project ID: "+reason(err), err)
		}
		if len(names) > 1 || cur.projid != want.fsx.projid {
			if err := checkProjectDirs(root, names, want.fsx.projid); err != nil {
				return err
			}
		}
		if cur.projid == want.fsx.projid && cur.xflags&projInheritX == want.fsx.xflags&projInheritX {
			return nil
		}
		next := cur
		next.projid = want.fsx.projid
		next.xflags = cur.xflags&^projInheritX | want.fsx.xflags&projInheritX
		if err := ioctlPtr(fd, fsIocFSSetXattr, unsafe.Pointer(&next)); err != nil {
			return failKind(ErrMetadata, "project ID: "+reason(err), err)
		}
		return nil
	})
}

// checkProjectDirs refuses a file whose project ID differs from that of a folder holding one of its
// names when that folder passes its project ID on to new files (chattr +P): Linux and ZFS won't
// rename or link a file with another project ID into such a folder, so the copy could never take
// the original's place.
func checkProjectDirs(root *os.Root, names []string, projid uint32) error {
	done := make(map[string]bool, 1)
	for _, name := range names {
		dir := path.Dir(name)
		if done[dir] {
			continue
		}
		done[dir] = true
		d, err := root.Open(dir)
		if err != nil {
			return fail("couldn't check the file's folder", err)
		}
		var fsx fsxattr
		err = withFd(d, func(fd int) error { return ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&fsx)) })
		_ = d.Close()
		if ok, err := supported(err); err != nil {
			return failKind(ErrMetadata, "project ID: "+reason(err), err)
		} else if ok && fsx.xflags&projInheritX != 0 && fsx.projid != projid {
			return failKind(ErrMetadata, "its project ID is different from its folder's, so a copy can't take its place", nil)
		}
	}
	return nil
}

// applyFileAttrs gives tmp the original's chattr flags and DOS attributes. It runs after the
// extended attributes, ACLs and mode are set, because ZFS turns the DOS archive attribute back on
// whenever those change; setting the timestamps afterwards leaves it alone.
func applyFileAttrs(tmp *os.File, want fileAttrs) error {
	return withFd(tmp, func(fd int) error {
		if want.hasFlags {
			cur, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
			if err != nil {
				return failKind(ErrMetadata, "chattr attributes: "+reason(err), err)
			}
			if next := cur&^copiedFlags | want.flags&copiedFlags; next != cur {
				if err := unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(next)); err != nil {
					return failKind(ErrMetadata, "chattr attributes: "+reason(err), err)
				}
			}
		}
		if want.hasDOS {
			var cur uint64
			if err := ioctlPtr(fd, zfsIocGetDOSFlags, unsafe.Pointer(&cur)); err != nil {
				return failKind(ErrMetadata, "Windows attributes: "+reason(err), err)
			}
			if next := want.dos & zfsDOSVisible; cur&zfsDOSVisible != next {
				if err := ioctlPtr(fd, zfsIocSetDOSFlags, unsafe.Pointer(&next)); err != nil {
					return failKind(ErrMetadata, "Windows attributes: "+reason(err), err)
				}
			}
		}
		return nil
	})
}

// afterSwap puts back the DOS attributes of the new file, open as fd, once it has taken the
// original's place: ZFS turns the archive attribute on whenever a file is renamed or linked, so the
// swap itself undoes a cleared one. It is best effort, as the swap has already happened.
func afterSwap(fd int, want fileAttrs) {
	if !want.hasDOS {
		return
	}
	var cur uint64
	if ioctlPtr(fd, zfsIocGetDOSFlags, unsafe.Pointer(&cur)) != nil {
		return
	}
	if next := want.dos & zfsDOSVisible; cur&zfsDOSVisible != next {
		_ = ioctlPtr(fd, zfsIocSetDOSFlags, unsafe.Pointer(&next))
	}
}

// diffFileAttrs names the first of tmp's chattr flags, project ID and DOS attributes that differs
// from want, or returns "" if they all match.
func diffFileAttrs(tmp *os.File, want fileAttrs) (string, error) {
	got, err := readFileAttrs(tmp)
	switch {
	case err != nil:
		return "", err
	case want.hasFlags && (!got.hasFlags || got.flags&copiedFlags != want.flags&copiedFlags):
		return "the chattr attributes", nil
	case want.hasFsx && (!got.hasFsx || got.fsx.projid != want.fsx.projid ||
		got.fsx.xflags&projInheritX != want.fsx.xflags&projInheritX):
		return "the project ID", nil
	case want.hasDOS && (!got.hasDOS || got.dos&zfsDOSVisible != want.dos&zfsDOSVisible):
		return "the Windows attributes", nil
	}
	return "", nil
}
