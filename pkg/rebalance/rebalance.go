package rebalance

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/astundzia/go-zfs-rebalance/internal/database"
	"github.com/astundzia/go-zfs-rebalance/internal/fileutil"
	log "github.com/sirupsen/logrus"
)

// Config holds configuration for the rebalance operation
type Config struct {
	SkipHardlinks       bool
	PassesLimit         int
	Concurrency         int
	RootPath            string
	Logger              *log.Logger
	CleanupBalanceFiles bool
	RandomOrder         bool
	SizeThresholdMB     int
	ChecksumType        fileutil.ChecksumType
	HaltOnFileMissing   bool
	ShowFullPaths       bool
}

// Rebalancer holds the state for a rebalance operation
type Rebalancer struct {
	config       *Config
	db           *database.DB
	logger       *log.Logger
	shutdownChan chan struct{}
	wg           *sync.WaitGroup
}

// NewRebalancer creates a new Rebalancer instance
func NewRebalancer(config *Config, db *database.DB) *Rebalancer {
	return &Rebalancer{
		config:       config,
		db:           db,
		logger:       config.Logger,
		shutdownChan: make(chan struct{}),
		wg:           &sync.WaitGroup{},
	}
}

// RebalanceFile copies a file, checks attributes and checksum, then removes the original and renames the copy.
// If the passesLimit is > 0, it tracks how many times a file has been rebalanced in the SQLite DB.
func (r *Rebalancer) RebalanceFile(filePath string) error {
	// Skip files that already have .balance extension
	if strings.HasSuffix(filePath, ".balance") {
		r.logger.Infof("Skipping temporary .balance file: %s", filePath)
		return nil
	}

	// Check for hardlinks - skip by default
	if r.config.SkipHardlinks {
		linkCount, err := fileutil.GetLinkCount(filePath)
		if err != nil {
			// If the file doesn't exist, it might have been deleted since gathering
			if os.IsNotExist(err) {
				r.logger.Warnf("File no longer on disk: %s", filePath)
				if r.config.HaltOnFileMissing {
					r.logger.Warnf("Initiating shutdown due to missing file (HaltOnFileMissing=true)")
					r.InitiateShutdown()
				}
				return nil
			}
			return fmt.Errorf("hardlink check failed for %s: %w", filePath, err)
		}
		if linkCount > 1 {
			r.logger.Infof("Skipping hard-linked file (use --process-hardlinks to include): %s", filePath)
			return nil
		}
	}

	// Check if passes are exceeded
	oldCount, err := r.db.GetRebalanceCount(filePath)
	if err != nil {
		return fmt.Errorf("db read error: %w", err)
	}

	if r.config.PassesLimit > 0 && oldCount >= r.config.PassesLimit {
		r.logger.Infof("Pass count (%d) reached, skipping: %s", r.config.PassesLimit, filePath)
		return nil
	}

	// Check if file exists
	srcInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			r.logger.Warnf("File no longer on disk: %s", filePath)
			if r.config.HaltOnFileMissing {
				r.logger.Warnf("Initiating shutdown due to missing file (HaltOnFileMissing=true)")
				r.InitiateShutdown()
			}
			return nil
		}
		return fmt.Errorf("failed to stat: %s => %w", filePath, err)
	}

	if !srcInfo.Mode().IsRegular() {
		r.logger.Infof("Skipping non-regular file: %s", filePath)
		return nil
	}

	// Store original file permissions and timestamp
	originalMode := srcInfo.Mode()
	originalTime := srcInfo.ModTime()
	fileSize := srcInfo.Size()

	tmpFilePath := filePath + ".balance"
	r.logger.Infof("Copying '%s' to '%s'...", filePath, tmpFilePath)

	// Step 1: Copy file to file.balance
	startTime := time.Now()

	// Check for shutdown before starting a long operation
	if r.isShuttingDown() {
		r.logger.Infof("Shutdown requested, skipping file: %s", filePath)
		return nil
	}

	if err := fileutil.CopyFile(filePath, tmpFilePath); err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}

	// Log copy speed for informational purposes
	elapsed := time.Since(startTime).Seconds()
	speedMBps := 0.0
	if elapsed > 0 {
		bytesPerSec := float64(fileSize) / elapsed
		speedMBps = bytesPerSec / (1024 * 1024)
	}

	// Step 2: Check checksums - Don't log the start of verification
	checksumType := r.config.ChecksumType
	if checksumType == "" {
		checksumType = fileutil.ChecksumSHA256 // Default to SHA256 if not specified
	}

	ok, reason := fileutil.CompareFileChecksum(filePath, tmpFilePath, checksumType)
	if !ok {
		// Clean up the temporary file on checksum mismatch
		os.Remove(tmpFilePath)
		r.logger.Errorf("Checksum mismatch for file: %s", filePath)
		return fmt.Errorf("%s checksum mismatch for file %s: %s", checksumType, filePath, reason)
	}

	// Step 3: Remove original file
	r.logger.Infof("Removing original '%s'...", filePath)
	if err := os.Remove(filePath); err != nil {
		// Clean up the temporary file on error
		os.Remove(tmpFilePath)

		// Check if file was removed by another process
		if os.IsNotExist(err) {
			r.logger.Warnf("Original file no longer on disk: %s", filePath)
			if r.config.HaltOnFileMissing {
				r.logger.Warnf("Initiating shutdown due to missing file (HaltOnFileMissing=true)")
				r.InitiateShutdown()
			}
			return nil
		}

		return fmt.Errorf("remove failed: %w", err)
	}

	// Step 4: Rename temporary copy to original name
	_, fileName := filepath.Split(filePath)
	r.logger.Infof("Renaming '%s.balance' to '%s'", fileName, fileName)
	if err := os.Rename(tmpFilePath, filePath); err != nil {
		// This is a critical failure - we've removed the original but can't rename the temp file
		// Try to put the temp file in a safe location
		emergencyPath := filePath + ".recovered"
		os.Rename(tmpFilePath, emergencyPath)
		return fmt.Errorf("CRITICAL: rename failed, data saved to %s: %w", emergencyPath, err)
	}

	// Step 5: Check permissions are the same as when it started
	newInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			r.logger.Warnf("File disappeared after rename: %s", filePath)
			return fmt.Errorf("file disappeared after rename")
		}
		return fmt.Errorf("failed to stat file after rename: %w", err)
	}

	if newInfo.Mode() != originalMode {
		// Log permission mismatches only in debug mode
		r.logger.Debugf("Permission mismatch: original=%v, new=%v", originalMode, newInfo.Mode())

		// Fix permissions quietly
		if err := os.Chmod(filePath, originalMode); err != nil {
			return fmt.Errorf("failed to fix permissions: %w", err)
		}

		// Only log at debug level
		r.logger.Debugf("Fixed permissions for '%s'", filePath)
	}

	if newInfo.ModTime() != originalTime {
		// Fix timestamps quietly
		if err := os.Chtimes(filePath, originalTime, originalTime); err != nil {
			return fmt.Errorf("failed to fix timestamps: %w", err)
		}

		// Only log at debug level
		r.logger.Debugf("Fixed timestamps for '%s'", filePath)
	}

	// Update DB if passesLimit is in use
	if r.config.PassesLimit > 0 {
		newCount := oldCount + 1
		err := r.db.SetRebalanceCount(filePath, newCount)
		if err != nil {
			return fmt.Errorf("db update error: %w", err)
		}
	}

	// Log success - check file size against threshold
	fileSizeMB := float64(fileSize) / (1024 * 1024)
	if r.config.SizeThresholdMB > 0 && fileSizeMB < float64(r.config.SizeThresholdMB) {
		// For small files, only log at debug level
		r.logger.WithField("show_full_paths", r.config.ShowFullPaths).Debugf("Successfully rebalanced %s at %.2f MB/s", filePath, speedMBps)
	} else {
		// For larger files, or if threshold is disabled (0), log at warning level to show in normal output
		r.logger.WithField("show_full_paths", r.config.ShowFullPaths).Warnf("Successfully rebalanced %s at %.2f MB/s", filePath, speedMBps)
	}
	return nil
}

// InitiateShutdown signals the rebalancer to gracefully shut down
func (r *Rebalancer) InitiateShutdown() {
	r.logger.Info("Initiating graceful shutdown - waiting for in-progress files to complete...")
	close(r.shutdownChan)
}

// isShuttingDown checks if a shutdown has been requested
func (r *Rebalancer) isShuttingDown() bool {
	select {
	case <-r.shutdownChan:
		return true
	default:
		return false
	}
}

// GetFiles returns the list of files to be processed
func (r *Rebalancer) GetFiles() ([]string, error) {
	return r.GatherFiles()
}

// GetPassInfo returns the current pass number and total passes
func (r *Rebalancer) GetPassInfo() (current, total int) {
	// Get current pass from the first file in DB, or default to 1
	current = 1

	files, err := r.GatherFiles()
	if err != nil || len(files) == 0 {
		return 1, r.config.PassesLimit
	}

	// Try to get the count from the first file to estimate current pass
	if len(files) > 0 {
		count, err := r.db.GetRebalanceCount(files[0])
		if err == nil {
			current = count + 1 // +1 because we're about to do this pass
		}
	}

	// If passes limit is 0, it means unlimited - return a large number
	if r.config.PassesLimit <= 0 {
		return current, 999
	}

	return current, r.config.PassesLimit
}

// Run executes the rebalance operation on all files in the root path
func (r *Rebalancer) Run(progressChan chan<- int) error {
	// Check if we need to clean up existing .balance files first
	if r.config.CleanupBalanceFiles {
		r.logger.Info("Cleaning up existing .balance files...")
		err := r.cleanupBalanceFiles()
		if err != nil {
			return fmt.Errorf("failed to cleanup .balance files: %w", err)
		}
	}

	files, err := r.GatherFiles()
	if err != nil {
		return fmt.Errorf("failed to gather files: %w", err)
	}

	r.logger.Infof("File count: %d", len(files))

	if len(files) == 0 {
		r.logger.Info("No files to process.")
		return nil
	}

	// Randomize file order by default unless disabled
	if r.config.RandomOrder {
		r.logger.Info("Randomizing file processing order...")
		// Seed the random number generator with current time
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(files), func(i, j int) {
			files[i], files[j] = files[j], files[i]
		})
	}

	fileChan := make(chan string, len(files))
	resultChan := make(chan error, len(files))
	processedCount := 0

	// Create a mutex to protect the processed count
	var countMutex sync.Mutex

	// Launch workers
	r.logger.Infof("Starting %d workers...", r.config.Concurrency)
	for i := 0; i < r.config.Concurrency; i++ {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for f := range fileChan {
				// Check if we're shutting down before starting a new file
				if r.isShuttingDown() {
					break
				}

				r.logger.Infof("Processing file: %s", f)
				e := r.RebalanceFile(f)

				if e != nil {
					r.logger.Errorf("Failed to rebalance %s: %v", f, e)
				}

				// Update processed count and send to progress channel
				countMutex.Lock()
				processedCount++
				if progressChan != nil {
					progressChan <- processedCount
				}
				countMutex.Unlock()

				resultChan <- e
			}
		}()
	}

	// Enqueue files for processing, but allow for interruption
	for _, f := range files {
		// Check for shutdown signal before adding more files to the queue
		if r.isShuttingDown() {
			break
		}

		fileChan <- f
	}
	close(fileChan)

	// Wait for workers to finish
	r.wg.Wait()
	close(resultChan)

	// Final cleanup of any remaining .balance files if we're shutting down
	if r.isShuttingDown() {
		r.logger.Info("Performing final cleanup of .balance files during shutdown...")
		if err := r.cleanupBalanceFiles(); err != nil {
			r.logger.Errorf("Error cleaning up .balance files: %v", err)
		}
	}

	// Final update to progress
	if progressChan != nil {
		progressChan <- processedCount
	}

	// Check for errors
	failed := false
	for e := range resultChan {
		if e != nil {
			failed = true
		}
	}

	if failed {
		return fmt.Errorf("some files failed to rebalance")
	}

	r.logger.Info("All files processed successfully")
	return nil
}

// GatherFiles collects all regular files in the given directory path
func (r *Rebalancer) GatherFiles() ([]string, error) {
	var files []string
	r.logger.Infof("Scanning directory: %s", r.config.RootPath)
	err := filepath.Walk(r.config.RootPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// If we cannot read a dir, skip it
			r.logger.Warnf("Cannot access path %s: %v", path, walkErr)
			return nil
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})

	return files, err
}

// truncatePath shortens a path for display purposes
func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}

	// Split the path to get the filename
	_, filename := filepath.Split(path)

	// If just the filename is too long, truncate it
	if len(filename) >= maxLen-3 {
		return "..." + filename[len(filename)-(maxLen-3):]
	}

	// Otherwise, keep the filename and add as much of the path as will fit
	remainingLen := maxLen - len(filename) - 3
	dirs := strings.Split(filepath.Dir(path), string(filepath.Separator))

	var result string
	for i := len(dirs) - 1; i >= 0; i-- {
		if len(dirs[i]) <= remainingLen {
			remainingLen -= len(dirs[i]) + 1 // +1 for the separator
			result = dirs[i] + string(filepath.Separator) + result
		} else {
			break
		}
	}

	return "..." + string(filepath.Separator) + result + filename
}

// cleanupBalanceFiles finds and removes any existing .balance files
func (r *Rebalancer) cleanupBalanceFiles() error {
	var balanceFiles []string

	// Find all .balance files
	err := filepath.Walk(r.config.RootPath, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			r.logger.Warnf("Cannot access path %s: %v", path, walkErr)
			return nil
		}
		if info.Mode().IsRegular() && strings.HasSuffix(path, ".balance") {
			balanceFiles = append(balanceFiles, path)
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Report the number of .balance files found
	r.logger.Infof("Found %d .balance files to clean up", len(balanceFiles))

	// Remove each .balance file
	for _, path := range balanceFiles {
		_, fileName := filepath.Split(path)
		r.logger.Infof("Removing stale balance file: %s", fileName)
		err := os.Remove(path)
		if err != nil {
			r.logger.Warnf("Failed to remove %s: %v", path, err)
		}
	}

	return nil
}
