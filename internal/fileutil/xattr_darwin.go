//go:build darwin

package fileutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// probedXattrs are read even when listxattr doesn't report them. macOS lists every attribute.
var probedXattrs []string

// aclXattrs is empty: a macOS ACL is not an extended attribute (see acl_darwin.go).
var aclXattrs []string

// kernelManagedXattr reports attributes the kernel sets on every new file and silently refuses to
// change, so they can be neither copied nor compared.
func kernelManagedXattr(name string) bool { return name == "com.apple.provenance" }

// isNoXattr reports whether err means "no such attribute" or "attributes aren't supported here".
func isNoXattr(err error) bool {
	return errors.Is(err, unix.ENOATTR) || errors.Is(err, unix.ENODATA) ||
		errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
