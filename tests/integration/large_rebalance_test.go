package integration

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/internal/database"
	"github.com/astundzia/go-zfs-rebalance/pkg/rebalance"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// TestLargeRebalanceWithChecksums creates a directory of files with various sizes,
// runs rebalancing over it, and verifies that all files are intact with matching checksums.
func TestLargeRebalanceWithChecksums(t *testing.T) {
	// Skip this test in normal runs as it's resource intensive
	if testing.Short() {
		t.Skip("Skipping large file test in short mode.")
	}

	// Create a test directory
	testDir, err := os.MkdirTemp("", "rebalance_large_test_")
	require.NoError(t, err, "Failed to create test directory")
	defer os.RemoveAll(testDir)

	// Setup test files of different sizes
	fileSizes := []int64{
		1024,               // 1KB
		10 * 1024,          // 10KB
		100 * 1024,         // 100KB
		1024 * 1024,        // 1MB
		10 * 1024 * 1024,   // 10MB
		100 * 1024 * 1024,  // 100MB
		1024 * 1024 * 1024, // 1GB - Commented out to avoid excessive test duration
	}

	// Create 100 files with random data and sizes
	totalFiles := 100
	for i := 0; i < totalFiles; i++ {
		// Select a random size from the first 5 size options to avoid creating too many large files
		sizeIndex := rand.Intn(5)
		size := fileSizes[sizeIndex]

		fileName := filepath.Join(testDir, fmt.Sprintf("file_%d_%d_bytes.dat", i, size))
		err := os.MkdirAll(filepath.Dir(fileName), 0755)
		require.NoError(t, err, "Failed to create directory")

		err = createFileWithSize(fileName, size)
		require.NoError(t, err, "Failed to create test file")
	}

	// Calculate SHA256 checksums before rebalancing
	t.Log("Calculating SHA256 checksums before rebalancing...")
	beforeChecksums, err := calculateChecksums(testDir)
	require.NoError(t, err, "Failed to calculate checksums before rebalancing")

	// Create and configure rebalancer
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)

	db, err := database.OpenSQLiteDB()
	require.NoError(t, err, "Failed to open SQLite DB")
	defer db.Close(true)

	t.Logf("Rebalancing directory with %d files", len(beforeChecksums))
	startTime := time.Now()

	config := &rebalance.Config{
		SkipHardlinks:       true,
		PassesLimit:         1,
		Concurrency:         8, // Use 8 workers
		RootPath:            testDir,
		Logger:              logger,
		CleanupBalanceFiles: true,
		RandomOrder:         false,
		SizeThresholdMB:     0,
	}

	r := rebalance.NewRebalancer(config, db)

	var progressChan chan<- int = nil

	err = r.Run(progressChan)
	if err != nil {
		t.Fatalf("Failed to run rebalancer: %v", err)
	}

	duration := time.Since(startTime)
	t.Logf("Rebalance completed in %v", duration)

	// Calculate checksums after rebalancing
	t.Log("Calculating SHA256 checksums after rebalancing...")
	afterChecksums, err := calculateChecksums(testDir)
	require.NoError(t, err, "Failed to calculate checksums after rebalancing")

	// Verify all files are present and checksums match
	if len(beforeChecksums) != len(afterChecksums) {
		t.Errorf("File count mismatch: before=%d, after=%d", len(beforeChecksums), len(afterChecksums))
	}

	verificationErrors := 0
	for fileName, beforeChecksum := range beforeChecksums {
		afterChecksum, exists := afterChecksums[fileName]
		if !exists {
			t.Errorf("File %s exists before rebalancing but not after", fileName)
			verificationErrors++
			continue
		}

		if beforeChecksum != afterChecksum {
			t.Errorf("Checksum mismatch for file %s: before=%s, after=%s",
				fileName, beforeChecksum, afterChecksum)
			verificationErrors++
		}
	}

	if verificationErrors > 0 {
		t.Fatalf("SHA256 verification failed with %d errors", verificationErrors)
	} else {
		t.Log("SHA256 verification successful: all file checksums match before and after rebalancing")
	}

	// Validate total size
	var totalSize int64
	files, err := os.ReadDir(testDir)
	require.NoError(t, err, "Failed to read directory")

	for _, file := range files {
		info, err := file.Info()
		require.NoError(t, err, "Failed to get file info")
		totalSize += info.Size()
	}

	t.Logf("Final directory has %d files with total size of %.2f GB",
		len(files), float64(totalSize)/(1024*1024*1024))
}

// createFileWithSize creates a file with the specified size filled with random data
func createFileWithSize(path string, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	// Create a buffer of random data (1MB)
	const bufSize = 1024 * 1024
	buffer := make([]byte, bufSize)
	rand.Read(buffer)

	// Write the buffer repeatedly until we reach the desired size
	remaining := size
	for remaining > 0 {
		writeSize := int64(bufSize)
		if remaining < writeSize {
			writeSize = remaining
		}

		_, err := file.Write(buffer[:writeSize])
		if err != nil {
			return err
		}

		remaining -= writeSize
	}

	return nil
}
