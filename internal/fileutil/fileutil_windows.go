//go:build windows

package fileutil

import (
	"fmt"
	"os"
)

// getLinkCountForPlatform returns the number of hardlinks for Windows systems
// Note: Windows doesn't easily expose link count in the same way, so we always return 1
func getLinkCountForPlatform(info os.FileInfo) (uint64, error) {
	// Windows doesn't easily expose link count in the same way as Unix
	// For simplicity, we'll return 1 (assumed to be one link)
	return 1, nil
}

// getFileOwnership returns dummy values for Windows
// Windows doesn't have the same UID/GID concept as Unix
func getFileOwnership(info os.FileInfo) (uint32, uint32, error) {
	// Windows doesn't have the same UID/GID concept as Unix
	return 0, 0, fmt.Errorf("ownership not supported on Windows")
}
