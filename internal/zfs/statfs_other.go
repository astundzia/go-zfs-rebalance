//go:build !linux && !darwin

package zfs

import "errors"

// OnZFS reports whether path is on a ZFS filesystem. Only Linux and macOS are
// supported; elsewhere it returns errors.ErrUnsupported.
func OnZFS(path string) (bool, error) {
	return false, errors.ErrUnsupported
}
