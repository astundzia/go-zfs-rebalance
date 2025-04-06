package integration

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/internal/database"
	"github.com/astundzia/go-zfs-rebalance/internal/fileutil"
	"github.com/astundzia/go-zfs-rebalance/pkg/rebalance"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestFile is a helper function to create a test file with specific content
// in the given directory. It uses require.NoError to fail the test immediately
// if file creation fails.
func createTestFile(t *testing.T, dir, name, content string) string {
	filePath := filepath.Join(dir, name)
	err := os.WriteFile(filePath, []byte(content), 0644)
	require.NoError(t, err, "Failed to create test file %s", name)
	return filePath
}

// getInode returns the inode number for a given file path.
// It skips the test on Windows as inodes are a Unix-specific concept.
// It uses require.NoError and require.True to fail the test on errors.
func getInode(t *testing.T, path string) uint64 {
	if runtime.GOOS == "windows" {
		t.Skip("Inode check skipped on Windows")
	}

	// Use the fileutil.GetInode function, which is platform-aware
	ino, err := fileutil.GetInode(path)
	require.NoError(t, err, "Failed to get inode for %s", path)
	return ino
}

// setupTestDirWithFiles creates a temporary directory and populates it with
// a set of test files, including some with duplicate content for testing
// deduplication/hardlinking. It returns the path to the created directory.
// It uses require.NoError to fail the test if directory creation fails.
func setupTestDirWithFiles(t *testing.T) string {
	testDir, err := os.MkdirTemp("", "rebalance_flags_test_")
	require.NoError(t, err, "Failed to create test directory")

	createTestFile(t, testDir, "file1.txt", "content1")
	createTestFile(t, testDir, "file2.txt", "content2")
	createTestFile(t, testDir, "file3_dup.txt", "duplicate content")
	createTestFile(t, testDir, "file4_dup.txt", "duplicate content")

	return testDir
}

// runRebalancer executes the rebalance process with the provided configuration.
// It ensures the root path exists, sets up a temporary database, initializes
// a logger (discarding output by default), runs the rebalancer, and cleans up
// the database afterwards. It returns any error encountered during the run.
func runRebalancer(t *testing.T, config *rebalance.Config) error {
	// Ensure the root path exists
	_, err := os.Stat(config.RootPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("root path %s does not exist", config.RootPath)
	} else if err != nil {
		return fmt.Errorf("failed to stat root path %s: %w", config.RootPath, err)
	}

	db, err := database.OpenSQLiteDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close(true) // Cleanup DB after run

	// Use a test-specific logger if none provided
	if config.Logger == nil {
		logger := log.New()
		logger.SetOutput(io.Discard) // Default to discard

		// To enable logs for debugging:
		// logger.SetOutput(os.Stderr)
		// logger.SetLevel(log.DebugLevel)
		config.Logger = logger
	}

	r := rebalance.NewRebalancer(config, db)
	var progressChan chan<- int = nil // No progress reporting needed for tests

	err = r.Run(progressChan)
	if err != nil {
		// Log the error before returning
		config.Logger.Errorf("Rebalancer failed: %v", err)
	}
	return err
}

// calculateSHA256 helper from large_rebalance_test.go - REMOVED (Dead code)
/*
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
*/

// calculateChecksums helper from large_rebalance_test.go - REMOVED (Dead code)
/*
func calculateChecksums(dirPath string) (map[string]string, error) {
	checksums := make(map[string]string)
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err // Propagate errors from walking
		}
		if info.IsDir() || info.Name() == database.DefaultDBFileName { // Skip directories and the DB file
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
*/

// TestConcurrencyFlag verifies that the rebalancer works correctly with different
// levels of concurrency, ensuring the final state (checksums) is consistent.
func TestConcurrencyFlag(t *testing.T) {
	// --- Setup ---
	testDir := setupTestDirWithFiles(t)
	defer os.RemoveAll(testDir)

	initialChecksums, err := calculateChecksums(testDir)
	require.NoError(t, err, "Failed to calculate initial checksums")

	// --- Test Cases ---
	// Test with concurrency = 1
	t.Run("Concurrency=1", func(t *testing.T) {
		// Setup isolated directory for this subtest
		tempDir1, err := os.MkdirTemp("", "rebalance_concurrency1_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir1)
		err = copyDir(testDir, tempDir1)
		require.NoError(t, err, "Failed to copy test dir for concurrency=1")

		// Configure and run rebalancer
		config := &rebalance.Config{
			RootPath:            tempDir1,
			Concurrency:         1,
			SkipHardlinks:       false,
			PassesLimit:         1,
			CleanupBalanceFiles: true,
		}
		err = runRebalancer(t, config)
		require.NoError(t, err, "Rebalancer failed with concurrency=1")

		// Verify results
		finalChecksums1, err := calculateChecksums(tempDir1)
		require.NoError(t, err, "Failed to calculate final checksums for concurrency=1")
		assert.Equal(t, initialChecksums, finalChecksums1, "Checksums mismatch after rebalance with concurrency=1")
	})

	// Test with concurrency = 4
	t.Run("Concurrency=4", func(t *testing.T) {
		// Setup isolated directory for this subtest
		tempDir4, err := os.MkdirTemp("", "rebalance_concurrency4_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDir4)
		err = copyDir(testDir, tempDir4)
		require.NoError(t, err, "Failed to copy test dir for concurrency=4")

		// Configure and run rebalancer
		config := &rebalance.Config{
			RootPath:            tempDir4,
			Concurrency:         4,
			SkipHardlinks:       false,
			PassesLimit:         1,
			CleanupBalanceFiles: true,
		}
		err = runRebalancer(t, config)
		require.NoError(t, err, "Rebalancer failed with concurrency=4")

		// Verify results
		finalChecksums4, err := calculateChecksums(tempDir4)
		require.NoError(t, err, "Failed to calculate final checksums for concurrency=4")
		assert.Equal(t, initialChecksums, finalChecksums4, "Checksums mismatch after rebalance with concurrency=4")
	})
}

// TestSkipHardlinksFlag verifies the functionality of the SkipHardlinks flag.
// It checks that hardlinks are NOT created when the flag is true, and implicitly
// tests that they *would* be created (or attempted) when false (though that
// specific check is currently skipped as the hardlinking feature isn't fully implemented).
func TestSkipHardlinksFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Hardlink test skipped on Windows")
	}

	// --- Setup ---
	testDir := setupTestDirWithFiles(t) // Includes duplicate files
	defer os.RemoveAll(testDir)

	// --- Test Cases ---
	// Test WITH hardlinks (default behavior - currently skipped)
	t.Run("WithHardlinks", func(t *testing.T) {
		t.Skip("Skipping hardlink creation test as the feature is not implemented.")
	})

	// Test WITHOUT hardlinks (--skip-hardlinks)
	t.Run("SkipHardlinks", func(t *testing.T) {
		// Setup isolated directory
		tempDirSkip, err := os.MkdirTemp("", "rebalance_skiphardlink_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDirSkip)
		err = copyDir(testDir, tempDirSkip)
		require.NoError(t, err, "Failed to copy test dir for skip hardlink test")

		// Configure and run rebalancer with skip-hardlinks
		config := &rebalance.Config{
			RootPath:            tempDirSkip,
			Concurrency:         1,
			SkipHardlinks:       true, // Explicitly true
			PassesLimit:         1,
			CleanupBalanceFiles: true,
		}
		err = runRebalancer(t, config)
		require.NoError(t, err, "Rebalancer failed with skip-hardlinks enabled")

		// Verify hardlinks were NOT created (inodes should differ)
		// Get inodes of the potentially hardlinked files *after* the run.
		inode1 := getInode(t, filepath.Join(tempDirSkip, "file3_dup.txt"))
		inode2 := getInode(t, filepath.Join(tempDirSkip, "file4_dup.txt"))

		// Sanity check: ensure files still exist and are regular files.
		info1, err1 := os.Stat(filepath.Join(tempDirSkip, "file3_dup.txt"))
		info2, err2 := os.Stat(filepath.Join(tempDirSkip, "file4_dup.txt"))
		require.NoError(t, err1)
		require.NoError(t, err2)
		require.True(t, info1.Mode().IsRegular())
		require.True(t, info2.Mode().IsRegular())

		// Assert that inodes are different, proving no hardlink was created.
		assert.NotEqual(t, inode1, inode2, "Inodes should differ when skip-hardlinks is true")
	})
}

// TestBalanceFileHandling verifies the creation, cleanup, and detection of
// temporary `.balance` files used during the rebalancing process.
func TestBalanceFileHandling(t *testing.T) {
	// --- Setup ---
	testDir := setupTestDirWithFiles(t)
	defer os.RemoveAll(testDir)

	initialChecksums, err := calculateChecksums(testDir)
	require.NoError(t, err, "Failed to calculate initial checksums")

	// --- Test Cases ---
	// Test normal run with cleanup enabled (default)
	t.Run("NormalCleanup", func(t *testing.T) {
		// Setup isolated directory
		tempDirNormal, err := os.MkdirTemp("", "rebalance_balance_normal_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDirNormal)
		err = copyDir(testDir, tempDirNormal)
		require.NoError(t, err, "Failed to copy test dir for normal balance test")

		// Configure and run rebalancer with cleanup enabled
		config := &rebalance.Config{
			RootPath:            tempDirNormal,
			Concurrency:         1,
			SkipHardlinks:       false,
			PassesLimit:         1,
			CleanupBalanceFiles: true, // Explicitly true (default)
		}
		err = runRebalancer(t, config)
		require.NoError(t, err, "Rebalancer failed during normal run")

		// Verify checksums are unchanged
		finalChecksums, err := calculateChecksums(tempDirNormal)
		require.NoError(t, err, "Failed to calculate final checksums")
		assert.Equal(t, initialChecksums, finalChecksums, "Checksums mismatch after normal run")

		// Verify no .balance files remain
		balanceFiles, err := filepath.Glob(filepath.Join(tempDirNormal, "*.balance"))
		require.NoError(t, err, "Failed to glob for .balance files")
		assert.Empty(t, balanceFiles, "Expected no .balance files after successful run with cleanup")
	})

	// Test run with cleanup disabled
	t.Run("NoCleanup", func(t *testing.T) {
		// Setup isolated directory
		tempDirNoCleanup, err := os.MkdirTemp("", "rebalance_balance_noclean_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDirNoCleanup)
		err = copyDir(testDir, tempDirNoCleanup)
		require.NoError(t, err, "Failed to copy test dir for no cleanup test")

		// Configure and run rebalancer with cleanup disabled
		config := &rebalance.Config{
			RootPath:            tempDirNoCleanup,
			Concurrency:         1,
			SkipHardlinks:       false,
			PassesLimit:         1,
			CleanupBalanceFiles: false, // Explicitly false
		}
		err = runRebalancer(t, config)
		require.NoError(t, err, "Rebalancer failed during run with no cleanup")

		// Verify checksums are unchanged
		finalChecksums, err := calculateChecksums(tempDirNoCleanup)
		require.NoError(t, err, "Failed to calculate final checksums")
		assert.Equal(t, initialChecksums, finalChecksums, "Checksums mismatch after run with no cleanup")

		// Verify no .balance files remain (internal cleanup still happens)
		// Note: Even with CleanupBalanceFiles=false, the individual RebalanceFile operation
		// cleans up its own temporary file upon success. This flag mainly controls
		// the *initial* cleanup pass at the start of Run().
		balanceFiles, err := filepath.Glob(filepath.Join(tempDirNoCleanup, "*.balance"))
		require.NoError(t, err, "Failed to glob for .balance files")
		assert.Empty(t, balanceFiles, "Expected no .balance files to remain from the run itself, even with CleanupBalanceFiles=false")
	})

	// Test detection and cleanup of pre-existing .balance files
	t.Run("DetectExisting", func(t *testing.T) {
		// Setup isolated directory
		tempDirDetect, err := os.MkdirTemp("", "rebalance_balance_detect_")
		require.NoError(t, err)
		defer os.RemoveAll(tempDirDetect)
		err = copyDir(testDir, tempDirDetect)
		require.NoError(t, err, "Failed to copy test dir for detect existing test")

		// --- Simulate leftover file ---
		// Run first with no cleanup (doesn't matter for this test, but simulates a scenario)
		configNoCleanup := &rebalance.Config{
			RootPath:            tempDirDetect,
			Concurrency:         1,
			SkipHardlinks:       false,
			PassesLimit:         1,
			CleanupBalanceFiles: false,
		}
		err = runRebalancer(t, configNoCleanup)
		require.NoError(t, err, "Rebalancer failed during initial run (no cleanup)")

		// Manually create a dummy .balance file
		dummyBalanceFile := filepath.Join(tempDirDetect, "dummy_file.txt.balance")
		err = os.WriteFile(dummyBalanceFile, []byte("dummy content"), 0644)
		require.NoError(t, err, "Failed to create dummy .balance file")
		_, err = os.Stat(dummyBalanceFile) // Verify it exists
		require.NoError(t, err, "Dummy .balance file does not exist after creation")

		// --- Run again with cleanup enabled ---
		configCleanup := &rebalance.Config{
			RootPath:            tempDirDetect, // Same directory
			Concurrency:         1,
			SkipHardlinks:       false,
			PassesLimit:         1,    // Run again
			CleanupBalanceFiles: true, // Cleanup now enabled
		}
		err = runRebalancer(t, configCleanup)
		require.NoError(t, err, "Rebalancer failed during second run (with cleanup)")

		// --- Verification ---
		// Verify checksums again
		finalChecksums, err := calculateChecksums(tempDirDetect)
		require.NoError(t, err, "Failed to calculate final checksums after second run")
		assert.Equal(t, initialChecksums, finalChecksums, "Checksums mismatch after second run")

		// Verify the dummy .balance file was removed by the initial cleanup pass
		balanceFilesAfter, err := filepath.Glob(filepath.Join(tempDirDetect, "*.balance"))
		require.NoError(t, err, "Failed to glob for .balance files after second run")
		assert.Empty(t, balanceFilesAfter, "Expected no .balance files after second run with cleanup enabled")
	})

}

// copyDir recursively copies a directory structure from src to dst.
// It's used here to create isolated environments for different test runs.
// It preserves directory permissions. File permissions are handled by copyFile.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Calculate the relative path
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		// Construct the destination path
		dstPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			// Create the directory in the destination
			return os.MkdirAll(dstPath, info.Mode())
		}

		// Copy the file
		return copyFile(path, dstPath)
	})
}

// copyFile copies a single file from src to dst, including its permissions.
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", src, err)
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file %s: %w", dst, err)
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		return fmt.Errorf("failed to copy data from %s to %s: %w", src, dst, err)
	}

	// Ensure permissions are copied too
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed to stat source file %s: %w", src, err)
	}
	return os.Chmod(dst, info.Mode())

}

// Note: Testing interrupted runs leaving .balance files is complex without
// internal hooks or process simulation. The "DetectExisting" test case provides
// confidence that leftover files are handled correctly if they occur.
