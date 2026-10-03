//go:build darwin

package fileutil

import (
	"os"

	"golang.org/x/sys/unix"
)

// openNoATime is zero: macOS can't open a file without updating its access time.
const openNoATime = 0

const (
	immutableHint   = "chflags uchg / uappnd"
	undeletableHint = "a macOS system flag"
)

// fileAttrs holds a macOS file's chflags flags. They are only used to refuse files that can't be
// replaced; the other flags (such as hidden) are not copied.
type fileAttrs struct{ flags uint32 }

func readFileAttrs(f *os.File) (fileAttrs, error) {
	var st unix.Stat_t
	err := withFd(f, func(fd int) error { return unix.Fstat(fd, &st) })
	if err != nil {
		return fileAttrs{}, fail("couldn't check the file", err)
	}
	return fileAttrs{flags: st.Flags}, nil
}

// protected returns an ErrImmutable error if the original is marked immutable or append-only, or
// an ErrUndeletable one if it is marked undeletable; each stops a rename over it, even for root.
func (a fileAttrs) protected() error {
	switch {
	case a.flags&(unix.UF_IMMUTABLE|unix.SF_IMMUTABLE|unix.UF_APPEND|unix.SF_APPEND) != 0:
		return failKind(ErrImmutable, "", nil)
	case a.flags&unix.SF_NOUNLINK != 0:
		return failKind(ErrUndeletable, "", nil)
	}
	return nil
}

func prepareFileAttrs(*os.Root, []string, *os.File, fileAttrs) error { return nil }

func applyFileAttrs(*os.File, fileAttrs) error { return nil }

func diffFileAttrs(*os.File, fileAttrs) (string, error) { return "", nil }

func afterSwap(*os.File, fileAttrs) {}
