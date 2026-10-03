//go:build darwin

package fileutil

import (
	"encoding/binary"
	"errors"
	"os"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS keeps ACLs (chmod +a) outside the extended attributes, as a kauth_filesec that
// getattrlist/setattrlist expose through ATTR_CMN_EXTENDED_SECURITY.
const (
	filesecMagic  = 0x012cc16d // KAUTH_FILESEC_MAGIC
	filesecNoACL  = 0xffffffff // KAUTH_FILESEC_NOACL: entry count meaning "no ACL"
	filesecHeader = 4 + 16 + 16 + 4 + 4
	filesecBufLen = 16 << 10 // the header plus 128 entries (the most macOS allows) of 24 bytes fits easily
)

var securityAttrs = unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}

// readACL returns f's raw extended security data (its ACL), or nil if it has none.
func readACL(f *os.File) ([]byte, error) {
	al := securityAttrs
	buf := make([]byte, filesecBufLen)
	err := withFd(f, func(fd int) error {
		//lint:ignore SA1019 x/sys/unix has no fgetattrlist wrapper; if the raw call ever stops working, every file fails safely with ErrMetadata.
		_, _, e := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&al)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e != 0 {
			return e
		}
		return nil
	})
	if err != nil {
		if isNoXattr(err) {
			return nil, nil // the filesystem has no ACLs
		}
		return nil, err
	}
	// Layout: uint32 total length, then an attrreference_t {int32 offset from itself, uint32 length}.
	total := binary.NativeEndian.Uint32(buf[0:4])
	offset := int32(binary.NativeEndian.Uint32(buf[4:8]))
	length := binary.NativeEndian.Uint32(buf[8:12])
	if length == 0 {
		return nil, nil
	}
	start := 4 + int64(offset)
	end := start + int64(length)
	if offset < 0 || end > int64(total) || end > int64(len(buf)) {
		return nil, errors.New("unexpected extended security data")
	}
	return slices.Clone(buf[start:end]), nil
}

// writeACL sets f's extended security data to acl as returned by readACL; nil removes the ACL.
func writeACL(f *os.File, acl []byte) error {
	if acl == nil {
		acl = make([]byte, filesecHeader)
		binary.NativeEndian.PutUint32(acl[0:4], filesecMagic)
		binary.NativeEndian.PutUint32(acl[36:40], filesecNoACL)
	}
	al := securityAttrs
	buf := make([]byte, 8+len(acl))
	binary.NativeEndian.PutUint32(buf[0:4], 8) // the data follows the attrreference_t
	binary.NativeEndian.PutUint32(buf[4:8], uint32(len(acl)))
	copy(buf[8:], acl)
	return withFd(f, func(fd int) error {
		//lint:ignore SA1019 x/sys/unix has no fsetattrlist wrapper (see readACL).
		_, _, e := unix.Syscall6(unix.SYS_FSETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&al)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e != 0 {
			return e
		}
		return nil
	})
}

// clearACL removes the ACL from the temporary file rel, if it is still the inode id, so that a
// "deny delete" entry copied from the original can't stop the temporary file from being removed.
func clearACL(root *os.Root, rel string, id FileID) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if info, err := statFile(f); err == nil && info.Mode.IsRegular() && info.ID == id {
		_ = writeACL(f, nil)
	}
}
