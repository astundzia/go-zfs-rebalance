package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/internal/fileutil"
)

// TestFileOperationsFromTestDir tests file operations from the tests directory
func TestFileOperationsFromTestDir(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "fileutil_test_dir")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create test files
	testFiles := []struct {
		Name    string
		Content string
	}{
		{"file1.txt", "This is the content of file 1"},
		{"file2.txt", "This is the content of file 2 which is longer"},
		{"file3.txt", "File 3 has the longest content in this test suite"},
	}

	// Create and verify files
	for _, tf := range testFiles {
		filePath := filepath.Join(tempDir, tf.Name)
		err := os.WriteFile(filePath, []byte(tf.Content), 0644)
		if err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.Name, err)
		}

		// Compute MD5 hash
		hash, err := fileutil.FileHashMD5(filePath)
		if err != nil {
			t.Errorf("Failed to compute MD5 for %s: %v", tf.Name, err)
		}
		t.Logf("File %s has MD5: %s", tf.Name, hash)
	}

	// Test copying files
	for _, tf := range testFiles {
		srcPath := filepath.Join(tempDir, tf.Name)
		dstPath := filepath.Join(tempDir, tf.Name+".copy")

		err := fileutil.CopyFile(srcPath, dstPath)
		if err != nil {
			t.Errorf("Failed to copy file %s: %v", tf.Name, err)
		}

		// Check attributes
		ok, reason := fileutil.CheckAttributes(srcPath, dstPath)
		if !ok {
			t.Errorf("Attribute check failed for %s: %s", tf.Name, reason)
		}

		// Check MD5
		ok, reason = fileutil.CompareFileMD5(srcPath, dstPath)
		if !ok {
			t.Errorf("MD5 check failed for %s: %s", tf.Name, reason)
		}

		// Check SHA256
		ok, reason = fileutil.CompareFileSHA256(srcPath, dstPath)
		if !ok {
			t.Errorf("SHA256 check failed for %s: %s", tf.Name, reason)
		}

		// Check with CompareFileChecksum using default (SHA256)
		ok, reason = fileutil.CompareFileChecksum(srcPath, dstPath, "")
		if !ok {
			t.Errorf("Default checksum check failed for %s: %s", tf.Name, reason)
		}

		// Check with CompareFileChecksum using MD5
		ok, reason = fileutil.CompareFileChecksum(srcPath, dstPath, fileutil.ChecksumMD5)
		if !ok {
			t.Errorf("MD5 checksum check via CompareFileChecksum failed for %s: %s", tf.Name, reason)
		}

		// Check with CompareFileChecksum using SHA256
		ok, reason = fileutil.CompareFileChecksum(srcPath, dstPath, fileutil.ChecksumSHA256)
		if !ok {
			t.Errorf("SHA256 checksum check via CompareFileChecksum failed for %s: %s", tf.Name, reason)
		}
	}

	// Test link count
	firstFile := filepath.Join(tempDir, testFiles[0].Name)
	linkCount, err := fileutil.GetLinkCount(firstFile)
	if err != nil {
		t.Errorf("Failed to get link count: %v", err)
	}
	t.Logf("File %s has %d links", testFiles[0].Name, linkCount)

	// Test non-existent file
	_, err = fileutil.GetLinkCount(filepath.Join(tempDir, "nonexistent.txt"))
	if err == nil {
		t.Errorf("Expected error for non-existent file, but got none")
	}
}
