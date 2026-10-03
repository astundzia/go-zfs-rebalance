//go:build linux || darwin

package fileutil

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
)

const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// wantedMeta is the metadata of the original that is not part of Info.
type wantedMeta struct {
	xattrs map[string][]byte // includes Linux POSIX and NFSv4 ACLs
	acl    []byte            // macOS ACL, which is not an extended attribute
}

// copyMetadata gives tmp (reachable as tmpRel in root) the owner, extended attributes, ACLs,
// permissions and timestamps of src, whose status is orig, and then checks the result.
//
// The order matters. The owner goes first because changing it clears setuid/setgid bits and file
// capabilities, and because setuid/setgid must never be set while the copy belongs to someone else.
// Extended attributes (and with them Linux ACLs) come next, then the mode, because setting an ACL
// can change it. The timestamps are set after that, since writing data or attributes can change
// them. A macOS ACL is set at the very end so a "deny" entry can't block the steps before it;
// setting it leaves the timestamps alone.
func copyMetadata(root *os.Root, src, tmp *os.File, tmpRel string, orig Info) error {
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
			return failKind(ErrOwnership, "", err)
		}
	}

	var want wantedMeta
	if want.xattrs, err = readXattrs(src); err != nil {
		return failKind(ErrMetadata, err.Error(), err)
	}
	if want.acl, err = readACL(src); err != nil {
		return failKind(ErrMetadata, "access control list: "+reason(err), err)
	}
	if err := applyXattrs(tmp, want.xattrs); err != nil {
		return failKind(ErrMetadata, err.Error(), err)
	}

	if cur, err = statFile(tmp); err != nil {
		return fail("couldn't check the new copy", err)
	}
	if cur.UID != orig.UID || cur.GID != orig.GID {
		return failKind(ErrOwnership, "", nil)
	}
	if cur.Mode != orig.Mode {
		if err := tmp.Chmod(orig.Mode & permBits); err != nil {
			return failKind(ErrMetadata, "permissions: "+reason(err), err)
		}
	}

	if err := root.Chtimes(tmpRel, orig.Atime, orig.Mtime); err != nil {
		return failKind(ErrMetadata, "timestamps: "+reason(err), err)
	}

	if acl, err := readACL(tmp); err != nil || !bytes.Equal(acl, want.acl) {
		if err == nil {
			err = writeACL(tmp, want.acl)
		}
		if err != nil {
			return failKind(ErrMetadata, "access control list: "+reason(err), err)
		}
	}

	return verifyMetadata(tmp, orig, want)
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
