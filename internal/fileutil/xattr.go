//go:build linux || darwin

package fileutil

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// xattrRetries bounds the size-then-read loops when an attribute keeps growing between calls.
const xattrRetries = 5

// withFd runs fn with f's descriptor, keeping f alive for the duration.
func withFd(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// readXattrs returns the extended attributes of f, including ACLs stored as attributes.
func readXattrs(f *os.File) (map[string][]byte, error) {
	attrs := map[string][]byte{}
	err := withFd(f, func(fd int) error {
		names, err := listXattrs(fd)
		if err != nil {
			return err
		}
		for _, name := range append(names, probedXattrs...) {
			if _, done := attrs[name]; done || kernelManagedXattr(name) {
				continue
			}
			val, ok, err := getXattr(fd, name)
			if err != nil {
				return &xattrError{name, err}
			}
			if ok {
				attrs[name] = val
			}
		}
		return nil
	})
	return attrs, err
}

// applyXattrs makes f's extended attributes exactly want: extras (for example an ACL inherited from
// the directory) are removed and missing or different ones are written.
func applyXattrs(f *os.File, want map[string][]byte) error {
	have, err := readXattrs(f)
	if err != nil {
		return err
	}
	return withFd(f, func(fd int) error {
		for _, name := range slices.Sorted(maps.Keys(have)) {
			if _, keep := want[name]; keep {
				continue
			}
			if err := unix.Fremovexattr(fd, name); err != nil && !isNoXattr(err) {
				return &xattrError{name, err}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if cur, ok := have[name]; ok && bytes.Equal(cur, want[name]) {
				continue
			}
			if err := unix.Fsetxattr(fd, name, want[name], 0); err != nil {
				return &xattrError{name, err}
			}
		}
		return nil
	})
}

func listXattrs(fd int) ([]string, error) {
	for range xattrRetries {
		size, err := unix.Flistxattr(fd, nil)
		if err != nil || size == 0 {
			if err != nil && !isNoXattr(err) {
				return nil, err
			}
			return nil, nil
		}
		buf := make([]byte, size)
		n, err := unix.Flistxattr(fd, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return strings.FieldsFunc(string(buf[:n]), func(r rune) bool { return r == 0 }), nil
	}
	return nil, unix.ERANGE
}

// getXattr returns the value of name; ok is false when f has no such attribute.
func getXattr(fd int, name string) (val []byte, ok bool, err error) {
	for range xattrRetries {
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			if isNoXattr(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		buf := make([]byte, size)
		n, err := unix.Fgetxattr(fd, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			if isNoXattr(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return buf[:n], true, nil
	}
	return nil, false, unix.ERANGE
}

// xattrError names the attribute that couldn't be read or written.
type xattrError struct {
	name string
	err  error
}

func (e *xattrError) Error() string { return "extended attribute " + e.name + ": " + reason(e.err) }
func (e *xattrError) Unwrap() error { return e.err }
