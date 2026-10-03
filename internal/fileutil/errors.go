package fileutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const nothingChanged = " — nothing was changed"

// sentinel is an error kind whose message is written for the people running the tool. A kind with
// a parent is a more specific case of the parent, and errors.Is matches both.
type sentinel struct {
	base   string
	parent *sentinel
}

func (s *sentinel) Error() string { return s.base + nothingChanged }

func (s *sentinel) Unwrap() error {
	if s.parent == nil {
		return nil
	}
	return s.parent
}

// Errors returned by ReplaceInPlace and ReplaceGroup. Match them with errors.Is; the returned error
// may add a short reason in parentheses and also matches the underlying system error (for example
// syscall.ENOSPC for ErrNoSpace).
var (
	// ErrModified means another program changed the file while it was being copied.
	ErrModified error = &sentinel{base: "the file changed while it was being copied"}
	// ErrNoSpace means the pool or a quota ran out of space. The error also matches the system
	// error, so errors.Is(err, syscall.EDQUOT) tells a quota (which may belong to the file's owner
	// or group rather than the whole dataset) apart from a full pool (syscall.ENOSPC).
	ErrNoSpace error = &sentinel{base: "not enough free space"}
	// ErrOwnership means the copy could not be given the original's owner or group. Without root
	// the message suggests sudo; for root it says the filesystem refused (for example an ID-mapped
	// mount, or an NFS share that treats root as nobody).
	ErrOwnership error = &sentinel{base: "couldn't keep the file's owner or group"}
	// ErrMetadata means permissions, extended attributes, ACLs, timestamps, Linux file attributes
	// (chattr flags and the project ID) or ZFS DOS attributes could not be reproduced exactly on
	// the copy.
	ErrMetadata error = &sentinel{base: "couldn't keep the file's permissions, attributes or timestamps exactly"}
	// ErrProjectID is the ErrMetadata case of a file whose project ID (used for project quotas)
	// differs from its folder's, where the folder passes its own on to new files (chattr +P). Linux
	// and ZFS refuse to rename or link a file with another project ID into such a folder (EXDEV),
	// so a copy can never take the original's place. It is noticed before anything is copied.
	ErrProjectID error = &sentinel{
		base:   "its project ID (used for quotas) is different from its folder's, and that folder only accepts files with its own, so it was left alone",
		parent: ErrMetadata.(*sentinel),
	}
	// ErrChecksum means the copy read back differently from the original.
	ErrChecksum error = &sentinel{base: "the new copy didn't match the original when checked"}
	// ErrNotRegular means the name is not a regular file (for example a symlink or a pipe).
	ErrNotRegular error = &sentinel{base: "it isn't a regular file"}
	// ErrLinkMismatch means the file's hardlinked names are not the ones expected, for example
	// because one of them was removed or renamed while the names were being switched over.
	ErrLinkMismatch error = &sentinel{base: "the file's hardlinks don't match what was expected"}
	// ErrImmutable means the file is marked immutable or append-only, so it can't be replaced. It
	// is noticed before anything is copied.
	ErrImmutable error = &sentinel{base: "it's marked immutable or append-only (" + immutableHint + "), so it was left alone"}
	// ErrUndeletable is the ErrImmutable case of a file that is protected from being deleted (the
	// ZFS nounlink attribute, or the system flag SF_NOUNLINK on macOS), which also stops a rename
	// over it.
	ErrUndeletable error = &sentinel{
		base:   "it's protected from being deleted (" + undeletableHint + "), so it was left alone",
		parent: ErrImmutable.(*sentinel),
	}
	// ErrBusy means another program or another run holds a lock on the file (flock), so it is
	// being worked on and was left alone. It is noticed before anything is copied.
	ErrBusy error = &sentinel{base: "another program or another rebalance run is working on it, so it was left alone"}
	// ErrUnsupportedPlatform is returned on operating systems other than Linux and macOS.
	ErrUnsupportedPlatform error = &sentinel{base: "rewriting files is only supported on Linux and macOS"}
)

// Short reasons shared by several steps.
const (
	tempGone       = "its temporary copy was removed or replaced by another program"
	tempChanged    = "its temporary copy was changed by another program"
	nameGone       = "one of its names was removed or renamed by another program"
	ownerOverQuota = "its owner or group is over their quota"
)

// failure is the error type returned by ReplaceGroup. Its message is meant for end users; errors.Is
// sees both its kind (one of the sentinels above) and the underlying cause.
type failure struct {
	kind    *sentinel // nil for an unexpected problem described by what
	what    string    // replaces the kind's own text when set
	detail  string
	cause   error
	swapped int // hardlinked names already switched to the new copy when the failure happened
	names   int
}

// LeftSplit reports whether err came from a hardlinked file whose names were only partly switched
// to the new copy, so they now point at two identical copies instead of one.
func LeftSplit(err error) bool {
	var f *failure
	return errors.As(err, &f) && f.swapped > 0
}

func (e *failure) Error() string {
	var b strings.Builder
	if e.what == "" && e.kind != nil {
		b.WriteString(e.kind.base)
	} else {
		b.WriteString(e.what)
	}
	if e.detail != "" {
		b.WriteString(" (" + e.detail + ")")
	}
	if e.swapped > 0 {
		verb := "were"
		if e.swapped == 1 {
			verb = "was"
		}
		fmt.Fprintf(&b, " — %d of its %d hardlinked names %s switched to the new, identical copy; the others still use the original",
			e.swapped, e.names, verb)
	} else {
		b.WriteString(nothingChanged)
	}
	return b.String()
}

func (e *failure) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.kind != nil {
		errs = append(errs, e.kind)
	}
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	return errs
}

// failKind returns an error of the given kind with an optional short detail. Running out of space
// is reported as ErrNoSpace whatever step it happened in.
func failKind(kind error, detail string, cause error) error {
	if errors.Is(cause, syscall.ENOSPC) || errors.Is(cause, syscall.EDQUOT) {
		return fail("", cause)
	}
	return &failure{kind: kind.(*sentinel), detail: detail, cause: cause}
}

// ownershipErr describes a failure to give the copy the original's owner or group; err is nil when
// the owner simply didn't stick. ZFS refuses with EDQUOT when that owner or group is over their
// quota; that counts as running out of space. Only root can give a file away, so without root the
// hint is sudo, and for root the filesystem itself must have refused.
func ownershipErr(err error) error {
	if errors.Is(err, syscall.EDQUOT) {
		return &failure{kind: ErrNoSpace.(*sentinel), detail: ownerOverQuota, cause: err}
	}
	detail := "try running with sudo"
	if os.Geteuid() == 0 {
		detail = "the filesystem refused, even for root"
		if err != nil {
			detail += ": " + reason(err)
		}
	}
	return failKind(ErrOwnership, detail, err)
}

// fail wraps an unexpected error from step what ("couldn't read the file"), turning running out of
// space into ErrNoSpace and keeping context cancellation recognisable.
func fail(what string, err error) error {
	var f *failure
	switch {
	case errors.As(err, &f):
		return err
	case errors.Is(err, syscall.EDQUOT):
		return &failure{kind: ErrNoSpace.(*sentinel), detail: "a quota was reached", cause: err}
	case errors.Is(err, syscall.ENOSPC):
		return &failure{kind: ErrNoSpace.(*sentinel), cause: err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &failure{what: "stopped before the copy was finished", cause: err}
	}
	return &failure{what: what, detail: reason(err), cause: err}
}

// reason is a short, path-free description of err, such as "permission denied".
func reason(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}
