//go:build unix
// +build unix

package fileutil

import (
	"fmt"
	"os"
	"syscall"
)

// Use syscall package to avoid import errors
var _ = syscall.Stat

// getLinkCountForPlatform returns the number of hardlinks for Unix-like systems
func getLinkCountForPlatform(info os.FileInfo) (uint64, error) {
	sysInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unable to get stat_t info")
	}
	
	return uint64(sysInfo.Nlink), nil
}

// getFileOwnership returns the UID and GID of a file for Unix-like systems
func getFileOwnership(info os.FileInfo) (uint32, uint32, error) {
	sysInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("unable to get stat_t info")
	}
	
	return sysInfo.Uid, sysInfo.Gid, nil
}
