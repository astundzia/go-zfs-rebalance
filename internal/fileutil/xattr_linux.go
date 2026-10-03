//go:build linux

package fileutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// probedXattrs are read even when listxattr doesn't report them: OpenZFS keeps NFSv4 ACLs
// (TrueNAS SCALE) in a hidden system.nfs4_acl_xdr attribute.
var probedXattrs = []string{"system.nfs4_acl_xdr", "system.posix_acl_access"}

// kernelManagedXattr reports attributes the kernel sets on every new file and silently refuses to
// change. Linux has none.
func kernelManagedXattr(string) bool { return false }

// isNoXattr reports whether err means "no such attribute" or "attributes aren't supported here".
func isNoXattr(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
