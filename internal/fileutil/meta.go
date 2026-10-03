//go:build linux || darwin

package fileutil

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"syscall"
)

const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// wantedMeta is the metadata of the original that is not part of Info. It is read before anything
// is copied; a later change to it also changes the original's ctime, which the final check before
// the swap notices.
type wantedMeta struct {
	xattrs map[string][]byte // includes Linux POSIX and NFSv4 ACLs
	acl    []byte            // macOS ACL, which is not an extended attribute
	attrs  fileAttrs         // Linux chattr flags, project ID and ZFS DOS attributes
}

func readWanted(src *os.File) (wantedMeta, error) {
	var want wantedMeta
	var err error
	if want.attrs, err = readFileAttrs(src); err != nil {
		return want, err
	}
	if err := want.attrs.protected(); err != nil {
		return want, err
	}
	if want.xattrs, err = readXattrs(src); err != nil {
		return want, failKind(ErrMetadata, err.Error(), err)
	}
	if want.acl, err = readACL(src); err != nil {
		return want, failKind(ErrMetadata, "access control list: "+reason(err), err)
	}
	return want, nil
}

// prepareTemp is the first half of copying the metadata, done while the copy tmp is still empty.
// It gives tmp the original's owner and project, so the data is charged to the right quotas and a
// file that can't be given its owner is refused before anything is copied, and the original's ACL
// and permission bits, so the data is never readable by anyone the original keeps out (an
// inherited folder ACL is replaced). setuid, setgid and sticky wait for finishTemp: writing the data
// would clear setuid and setgid, and they are never set on a file that is still being written.
//
// Without root, the owner keeps write permission on tmp until finishTemp, because writing extended
// attributes needs it.
func prepareTemp(root *os.Root, names []string, tmp *os.File, orig Info, want wantedMeta) error {
	cur, err := statFile(tmp)
	if err != nil {
		return fail("couldn't check the new copy", err)
	}
	if cur.UID != orig.UID || cur.GID != orig.GID {
		uid, gid := -1, -1
		if cur.UID != orig.UID {
			uid = int(orig.UID)
		}
		if cur.GID != orig.GID {
			gid = int(orig.GID)
		}
		if err := tmp.Chown(uid, gid); err != nil {
			return ownershipErr(err)
		}
	}
	if err := prepareFileAttrs(root, names, tmp, want.attrs); err != nil {
		return err
	}

	if err := applyXattrs(tmp, want.xattrs, isACLXattr); err != nil {
		return failKind(ErrMetadata, err.Error(), err)
	}
	// A macOS ACL with "deny" entries could block the steps still to come for anyone but root, so
	// without root the copy has no ACL until finishTemp.
	acl := want.acl
	notRoot := os.Geteuid() != 0
	if notRoot {
		acl = nil
	}
	if err := setACL(tmp, acl); err != nil {
		return err
	}

	perm := orig.Mode.Perm()
	if notRoot {
		perm |= 0o200
	}
	if cur, err = statFile(tmp); err != nil {
		return fail("couldn't check the new copy", err)
	}
	// An NFSv4 ACL in "restricted" mode refuses chmod (EPERM); then the ACL alone decides who may
	// read the copy, and finishTemp tries again.
	if cur.Mode.Perm() != perm {
		if err := tmp.Chmod(perm); err != nil && !errors.Is(err, syscall.EPERM) {
			return failKind(ErrMetadata, "permissions: "+reason(err), err)
		}
	}
	return nil
}

// finishTemp is the second half of copying the metadata, done once the data is written and
// verified: the remaining extended attributes (including file capabilities, which a write would
// clear), the full mode with setuid and setgid, chattr flags, ZFS DOS attributes and, last of all,
// the timestamps, which every other step can change. A macOS ACL is set after the timestamps so
// a "deny" entry can't block the steps before it; setting it leaves them alone. The result is then
// checked against the original.
func finishTemp(root *os.Root, tmp *os.File, tmpRel string, orig Info, want wantedMeta) error {
	// The ACL goes after the other attributes: it can take away the write permission they need.
	for _, pick := range []func(string) bool{isOtherXattr, isACLXattr} {
		if err := applyXattrs(tmp, want.xattrs, pick); err != nil {
			return failKind(ErrMetadata, err.Error(), err)
		}
	}

	cur, err := statFile(tmp)
	if err != nil {
		return fail("couldn't check the new copy", err)
	}
	// setuid and setgid are only ever set on a copy that belongs to the original's owner.
	if cur.UID != orig.UID || cur.GID != orig.GID {
		return failKind(ErrOwnership, "", nil)
	}
	if cur.Mode != orig.Mode {
		if err := tmp.Chmod(orig.Mode & permBits); err != nil {
			return failKind(ErrMetadata, "permissions: "+reason(err), err)
		}
	}

	if err := applyFileAttrs(tmp, want.attrs); err != nil {
		return err
	}

	if err := root.Chtimes(tmpRel, orig.Atime, orig.Mtime); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return failKind(ErrModified, tempGone, err)
		}
		return failKind(ErrMetadata, "timestamps: "+reason(err), err)
	}

	if err := setACL(tmp, want.acl); err != nil {
		return err
	}
	return verifyMetadata(tmp, orig, want)
}

// setACL makes f's macOS ACL exactly acl (nil for none). It does nothing on Linux, where ACLs are
// extended attributes.
func setACL(f *os.File, acl []byte) error {
	cur, err := readACL(f)
	if err == nil && !bytes.Equal(cur, acl) {
		err = writeACL(f, acl)
	}
	if err != nil {
		return failKind(ErrMetadata, "access control list: "+reason(err), err)
	}
	return nil
}

// verifyMetadata re-reads the copy's status, attributes and ACL and requires them to match the
// original. A file is never swapped for one with weaker metadata.
func verifyMetadata(tmp *os.File, orig Info, want wantedMeta) error {
	got, err := statFile(tmp)
	if err != nil {
		return fail("couldn't check the new copy", err)
	}
	var what string
	switch {
	case got.UID != orig.UID || got.GID != orig.GID:
		what = "the owner"
	case got.Mode != orig.Mode:
		what = "the permissions"
	case got.Size != orig.Size:
		what = "the size"
	case !got.Mtime.Equal(orig.Mtime):
		what = "the modification time"
	}
	if what == "" {
		xattrs, err := readXattrs(tmp)
		if err != nil {
			return failKind(ErrMetadata, err.Error(), err)
		}
		acl, err := readACL(tmp)
		if err != nil {
			return failKind(ErrMetadata, "access control list: "+reason(err), err)
		}
		if !maps.EqualFunc(xattrs, want.xattrs, bytes.Equal) || !bytes.Equal(acl, want.acl) {
			what = "the extended attributes or ACL"
		}
	}
	if what == "" {
		if what, err = diffFileAttrs(tmp, want.attrs); err != nil {
			return err
		}
	}
	if what != "" {
		return failKind(ErrMetadata, what+" came out different", nil)
	}
	return nil
}

func statFile(f *os.File) (Info, error) {
	fi, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	return InfoOf(fi)
}
