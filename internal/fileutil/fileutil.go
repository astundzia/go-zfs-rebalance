// Package fileutil rewrites a file in place so that its data lands on fresh blocks, while keeping
// everything else about it (contents, owner, permissions, extended attributes, ACLs and timestamps)
// exactly the same.
//
// The original is never removed or truncated: a verified copy is built under a temporary name in
// the same directory and then atomically renamed over the original. If anything goes wrong, or the
// context is cancelled, the temporary copy is removed and the original is left untouched.
package fileutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// Temporary copies are named TempPrefix + 12 lowercase hex digits + TempSuffix (31 bytes), short
// enough that even a file whose own name is at the length limit can be rewritten next to it.
const (
	TempPrefix = ".zfs-rebalance."
	TempSuffix = ".tmp"
)

const tempHexLen = 12

// IsTempName reports whether base (a file name without directories) is exactly the name of one of
// our temporary copies.
func IsTempName(base string) bool {
	if len(base) != len(TempPrefix)+tempHexLen+len(TempSuffix) ||
		!strings.HasPrefix(base, TempPrefix) || !strings.HasSuffix(base, TempSuffix) {
		return false
	}
	for _, c := range base[len(TempPrefix) : len(TempPrefix)+tempHexLen] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func randomTempName() string {
	var b [tempHexLen / 2]byte
	_, _ = rand.Read(b[:]) // never fails
	return TempPrefix + hex.EncodeToString(b[:]) + TempSuffix
}

// ChecksumType selects the hash used to check that the new copy matches the original.
type ChecksumType string

// Supported checksum types.
const (
	ChecksumSHA256 ChecksumType = "sha256"
	ChecksumMD5    ChecksumType = "md5"
)

// ParseChecksumType turns user input such as "SHA256" or "md5" into a ChecksumType.
func ParseChecksumType(s string) (ChecksumType, error) {
	switch ChecksumType(strings.ToLower(strings.TrimSpace(s))) {
	case ChecksumSHA256:
		return ChecksumSHA256, nil
	case ChecksumMD5:
		return ChecksumMD5, nil
	}
	return "", fmt.Errorf("%q isn't a checksum this tool knows; use sha256 (the default) or md5", s)
}

// FileID identifies an inode: the device it lives on and its inode number.
type FileID struct{ Dev, Ino uint64 }

// Info is the subset of a file's status that the rewrite has to preserve or watch for changes.
type Info struct {
	ID    FileID
	Size  int64
	Mode  os.FileMode // full Go mode, including setuid, setgid and sticky bits
	Nlink uint64
	UID   uint32
	GID   uint32
	Atime time.Time
	Mtime time.Time
	Ctime time.Time
}

// Lstat returns Info for rel inside root without following a final symlink.
func Lstat(root *os.Root, rel string) (Info, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return Info{}, err
	}
	return InfoOf(fi)
}

// Options tune ReplaceInPlace and ReplaceGroup. The zero value is ready to use.
type Options struct {
	Checksum   ChecksumType // defaults to sha256
	BufferSize int          // bytes per read; defaults to 1 MiB
}

const defaultBufferSize = 1 << 20

// Result describes a successful rewrite.
type Result struct {
	Size     int64         // bytes copied
	Duration time.Duration // wall time for the whole rewrite
	Before   Info          // the original inode, as it was before the rewrite
	NewID    FileID        // the inode now behind the name(s)
}

// ReplaceInPlace atomically replaces root/rel with a verified copy on new blocks that has the same
// contents and metadata. rel is slash-separated and relative to root.
//
// The original is never removed before the new copy is complete and verified; the swap is a single
// rename over it. On any error or context cancellation the temporary copy is removed and the
// original is left untouched. A file with more than one hardlink is refused with ErrLinkMismatch;
// use ReplaceGroup for those.
func ReplaceInPlace(ctx context.Context, root *os.Root, rel string, opts Options) (Result, error) {
	return ReplaceGroup(ctx, root, []string{rel}, opts)
}
