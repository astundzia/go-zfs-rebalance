package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// calculateSHA256 helper calculates the SHA256 checksum of a file.
func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to calculate SHA256 for %s: %w", filePath, err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// calculateChecksums helper calculates SHA256 checksums for relevant files in a directory.
func calculateChecksums(dirPath string) (map[string]string, error) {
	checksums := make(map[string]string)
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err // Propagate errors from walking
		}
		if info.IsDir() || info.Name() == "rebalance.db" { // Skip directories and the DB file
			return nil
		}
		if filepath.Ext(path) == ".balance" { // Skip balance files
			return nil
		}

		checksum, err := calculateSHA256(path)
		if err != nil {
			// Wrap error with file path info
			return fmt.Errorf("failed to calculate checksum for %s: %w", path, err)
		}
		// Use relative path for map key
		relPath, err := filepath.Rel(dirPath, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}
		checksums[relPath] = checksum
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed during checksum calculation walk in %s: %w", dirPath, err)
	}

	return checksums, nil
}
