//go:build linux

package fileutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// aclXattrs are the extended attributes that hold a file's ACL: an NFSv4 ACL, which OpenZFS keeps
// in system.nfs4_acl_xdr (TrueNAS SCALE), or a POSIX ACL.
var aclXattrs = []string{"system.nfs4_acl_xdr", "system.posix_acl_access"}

// probedXattrs are read even when listxattr doesn't report them, which OpenZFS doesn't for
// system.nfs4_acl_xdr.
var probedXattrs = aclXattrs

// kernelManagedXattr reports attributes the kernel sets on every new file and silently refuses to
// change. Linux has none.
func kernelManagedXattr(string) bool { return false }

// isNoXattr reports whether err means "no such attribute" or "attributes aren't supported here".
func isNoXattr(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
