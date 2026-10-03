package fileutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
)

const nothingChanged = " — nothing was changed"

// sentinel is an error kind whose message is written for the people running the tool.
type sentinel struct{ base string }

func (s *sentinel) Error() string { return s.base + nothingChanged }

// Errors returned by ReplaceInPlace and ReplaceGroup. Match them with errors.Is; the returned error
// may add a short reason in parentheses and also matches the underlying system error (for example
// syscall.ENOSPC for ErrNoSpace).
var (
	// ErrModified means another program changed the file while it was being copied.
	ErrModified error = &sentinel{"the file changed while it was being copied"}
	// ErrNoSpace means the pool or a quota ran out of space (ENOSPC or EDQUOT).
	ErrNoSpace error = &sentinel{"not enough free space"}
	// ErrOwnership means the copy could not be given the original's owner or group.
	ErrOwnership error = &sentinel{"couldn't keep the file's owner (try running with sudo)"}
	// ErrMetadata means permissions, extended attributes, ACLs or timestamps could not be
	// reproduced exactly on the copy.
	ErrMetadata error = &sentinel{"couldn't keep the file's permissions, attributes or timestamps exactly"}
	// ErrChecksum means the copy read back differently from the original.
	ErrChecksum error = &sentinel{"the new copy didn't match the original when checked"}
	// ErrNotRegular means the name is not a regular file (for example a symlink or a pipe).
	ErrNotRegular error = &sentinel{"it isn't a regular file"}
	// ErrLinkMismatch means the file's hardlinked names are not the ones expected.
	ErrLinkMismatch error = &sentinel{"the file's hardlinks don't match what was expected"}
	// ErrUnsupportedPlatform is returned on operating systems other than Linux and macOS.
	ErrUnsupportedPlatform error = &sentinel{"rewriting files is only supported on Linux and macOS"}
)

// failure is the error type returned by ReplaceGroup. Its message is meant for end users; errors.Is
// sees both its kind (one of the sentinels above) and the underlying cause.
type failure struct {
	kind    *sentinel // nil for an unexpected problem described by what
	what    string
	detail  string
	cause   error
	swapped int // hardlinked names already switched to the new copy when the failure happened
	names   int
}

func (e *failure) Error() string {
	var b strings.Builder
	if e.kind != nil {
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
