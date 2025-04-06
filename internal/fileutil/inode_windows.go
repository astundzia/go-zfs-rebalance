//go:build windows
// +build windows

package fileutil

import (
	"fmt"
	"os"
)

// GetInode returns a dummy value for Windows as inodes are a Unix-specific concept
func GetInode(path string) (uint64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("inodes not supported on Windows")
}

// GetInodeFromFileInfo returns a dummy value for Windows
func GetInodeFromFileInfo(info os.FileInfo) (uint64, error) {
	return 0, fmt.Errorf("inodes not supported on Windows")
} 