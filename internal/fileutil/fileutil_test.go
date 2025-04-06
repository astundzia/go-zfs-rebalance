package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileOperations(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "fileutil_test")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Test file paths
	srcPath := filepath.Join(tempDir, "source.txt")
	dstPath := filepath.Join(tempDir, "dest.txt")

	// Test data
	testData := []byte("test data for fileutil tests")

	// Create test file
	err = os.WriteFile(srcPath, testData, 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Test CopyFile
	t.Run("CopyFile", func(t *testing.T) {
		err := CopyFile(srcPath, dstPath)
		if err != nil {
			t.Fatalf("CopyFile failed: %v", err)
		}

		// Verify file exists
		if _, err := os.Stat(dstPath); os.IsNotExist(err) {
			t.Fatalf("Destination file doesn't exist after copy")
		}

		// Read copied content
		content, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatalf("Failed to read copied file: %v", err)
		}

		if string(content) != string(testData) {
			t.Errorf("Copied content doesn't match original. Got: %s, Want: %s", string(content), string(testData))
		}
	})

	// Test CheckAttributes
	t.Run("CheckAttributes", func(t *testing.T) {
		ok, reason := CheckAttributes(srcPath, dstPath)
		if !ok {
			t.Errorf("CheckAttributes failed: %s", reason)
		}

		// Modify the destination file's permissions to cause attribute mismatch
		err := os.Chmod(dstPath, 0600)
		if err != nil {
			t.Fatalf("Failed to change file permissions: %v", err)
		}

		ok, reason = CheckAttributes(srcPath, dstPath)
		if ok {
			t.Errorf("CheckAttributes should have failed due to mode mismatch, but it passed")
		}
		if reason != "mode mismatch" {
			t.Errorf("Expected reason 'mode mismatch', got: %s", reason)
		}
	})

	// Test CompareFileMD5
	t.Run("CompareFileMD5", func(t *testing.T) {
		// Reset the destination file to match source
		err = CopyFile(srcPath, dstPath)
		if err != nil {
			t.Fatalf("Failed to reset destination file: %v", err)
		}

		ok, reason := CompareFileMD5(srcPath, dstPath)
		if !ok {
			t.Errorf("CompareFileMD5 failed: %s", reason)
		}

		// Modify destination file to cause MD5 mismatch
		err = os.WriteFile(dstPath, []byte("modified content"), 0644)
		if err != nil {
			t.Fatalf("Failed to modify destination file: %v", err)
		}

		ok, reason = CompareFileMD5(srcPath, dstPath)
		if ok {
			t.Errorf("CompareFileMD5 should have failed due to content mismatch, but it passed")
		}
	})

	// Test FileHashMD5
	t.Run("FileHashMD5", func(t *testing.T) {
		hash1, err := FileHashMD5(srcPath)
		if err != nil {
			t.Fatalf("FileHashMD5 failed: %v", err)
		}

		// Re-compute hash - should be the same
		hash2, err := FileHashMD5(srcPath)
		if err != nil {
			t.Fatalf("FileHashMD5 failed on second call: %v", err)
		}

		if hash1 != hash2 {
			t.Errorf("FileHashMD5 produced different hashes for the same file. Got: %s and %s", hash1, hash2)
		}
	})

	// Test CompareFileSHA256 and CompareFileChecksum
	t.Run("CompareFileSHA256", func(t *testing.T) {
		// Reset the destination file to match source
		err = CopyFile(srcPath, dstPath)
		if err != nil {
			t.Fatalf("Failed to reset destination file: %v", err)
		}

		ok, reason := CompareFileSHA256(srcPath, dstPath)
		if !ok {
			t.Errorf("CompareFileSHA256 failed: %s", reason)
		}

		// Modify destination file to cause SHA256 mismatch
		err = os.WriteFile(dstPath, []byte("modified content"), 0644)
		if err != nil {
			t.Fatalf("Failed to modify destination file: %v", err)
		}

		ok, reason = CompareFileSHA256(srcPath, dstPath)
		if ok {
			t.Errorf("CompareFileSHA256 should have failed due to content mismatch, but it passed")
		}

		// Test CompareFileChecksum with SHA256
		err = CopyFile(srcPath, dstPath)
		if err != nil {
			t.Fatalf("Failed to reset destination file: %v", err)
		}

		ok, reason = CompareFileChecksum(srcPath, dstPath, ChecksumSHA256)
		if !ok {
			t.Errorf("CompareFileChecksum with SHA256 failed: %s", reason)
		}

		// Test CompareFileChecksum with MD5
		ok, reason = CompareFileChecksum(srcPath, dstPath, ChecksumMD5)
		if !ok {
			t.Errorf("CompareFileChecksum with MD5 failed: %s", reason)
		}

		// Test default behavior (should use SHA256)
		ok, reason = CompareFileChecksum(srcPath, dstPath, "")
		if !ok {
			t.Errorf("CompareFileChecksum with default failed: %s", reason)
		}
	})

	// Test FileHashSHA256
	t.Run("FileHashSHA256", func(t *testing.T) {
		hash1, err := FileHashSHA256(srcPath)
		if err != nil {
			t.Fatalf("FileHashSHA256 failed: %v", err)
		}

		// Re-compute hash - should be the same
		hash2, err := FileHashSHA256(srcPath)
		if err != nil {
			t.Fatalf("FileHashSHA256 failed on second call: %v", err)
		}

		if hash1 != hash2 {
			t.Errorf("FileHashSHA256 produced different hashes for the same file. Got: %s and %s", hash1, hash2)
		}

		// Verify that no errors occur for nonexistent file
		_, err = FileHashSHA256(filepath.Join(tempDir, "nonexistent.txt"))
		if err == nil {
			t.Errorf("FileHashSHA256 should fail for non-existent file but it didn't")
		}
	})
}

func TestGetLinkCount(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "linkcount_test")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	filePath := filepath.Join(tempDir, "test.txt")
	err = os.WriteFile(filePath, []byte("link count test"), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Test initial link count (should be 1)
	count, err := GetLinkCount(filePath)
	if err != nil {
		t.Fatalf("GetLinkCount failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected link count 1, got %d", count)
	}

	// Test non-existent file
	_, err = GetLinkCount(filepath.Join(tempDir, "nonexistent.txt"))
	if err == nil {
		t.Errorf("GetLinkCount should have failed for non-existent file, but it passed")
	}
}
