//go:build !windows
// +build !windows

package fileutil

import (
	"fmt"
	"os"
	"syscall"
)

var _ syscall.Stat_t // Force usage of syscall package

// GetInode returns the inode number for a file
func GetInode(path string) (uint64, error) {
	// Call syscall.Stat directly to ensure the import is used
	var stat syscall.Stat_t
	err := syscall.Stat(path, &stat)
	if err != nil {
		return 0, fmt.Errorf("failed to stat file: %w", err)
	}
	return stat.Ino, nil
}

// GetInodeFromFileInfo extracts the inode number from file info
func GetInodeFromFileInfo(info os.FileInfo) (uint64, error) {
	sysInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unable to get stat_t info")
	}
	return sysInfo.Ino, nil
} 