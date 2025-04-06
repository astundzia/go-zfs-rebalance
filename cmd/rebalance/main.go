package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/astundzia/go-zfs-rebalance/internal/database"
	"github.com/astundzia/go-zfs-rebalance/internal/fileutil"
	"github.com/astundzia/go-zfs-rebalance/pkg/rebalance"
	"github.com/sirupsen/logrus"
)

// Version information
const (
	VERSION = "1.0.0"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorBold   = "\033[1m"
)

// CustomFormatter is a custom logrus formatter that uses a simpler timestamp format
type CustomFormatter struct {
	logrus.TextFormatter
}

// Format implements logrus.Formatter interface
func (f *CustomFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	// Use a timestamp format with seconds: "11:25:59 PM"
	timestamp := entry.Time.Format("3:04:05 PM")

	// Get operation type and file path from the message
	operation := ""
	filePath := ""
	color := ""

	// Extract speed information if available
	speedStr := ""

	// Set color based on log level
	switch entry.Level {
	case logrus.ErrorLevel:
		color = colorRed
	case logrus.WarnLevel:
		// Only use yellow for warnings, success messages get special handling
		if !strings.Contains(entry.Message, "Successfully rebalanced") {
			color = colorYellow
		}
	}

	// Check if message contains copy speed
	if strings.Contains(entry.Message, "completed at") {
		parts := strings.Split(entry.Message, "completed at")
		if len(parts) > 1 {
			speedPart := strings.TrimSpace(parts[1])
			if strings.HasSuffix(speedPart, "MB/s") {
				operation = "Copying"
				speedStr = fmt.Sprintf("at %.2f MB/s", parseSpeed(speedPart))
			}
		}
	} else if strings.Contains(entry.Message, "Copying '") {
		operation = "Copying"

		// Extract file path
		parts := strings.Split(entry.Message, "Copying '")
		if len(parts) > 1 {
			pathParts := strings.Split(parts[1], "' to '")
			if len(pathParts) > 0 {
				filePath = pathParts[0]
			}
		}
	} else if strings.Contains(entry.Message, "Removing original") {
		operation = "Removing"

		// Extract file path
		parts := strings.Split(entry.Message, "Removing original '")
		if len(parts) > 1 {
			pathParts := strings.Split(parts[1], "'...")
			if len(pathParts) > 0 {
				filePath = pathParts[0]
			}
		}
	} else if strings.Contains(entry.Message, "Renaming") {
		operation = "Renaming"

		// Extract filenames from the format: Renaming 'source.ext.balance' to 'dest.ext'
		parts := strings.Split(entry.Message, "Renaming '")
		if len(parts) > 1 {
			pathParts := strings.Split(parts[1], "' to '")
			if len(pathParts) > 1 {
				sourceFile := pathParts[0]
				destFile := strings.TrimSuffix(pathParts[1], "'")
				// Use both source and destination in the formatted message
				filePath = fmt.Sprintf("%s to %s", sourceFile, destFile)
			}
		}
	} else if strings.Contains(entry.Message, "Failed to rebalance") {
		operation = "Error"
		color = colorRed

		// Extract file path
		parts := strings.Split(entry.Message, "Failed to rebalance ")
		if len(parts) > 1 {
			pathParts := strings.Split(parts[1], ":")
			if len(pathParts) > 0 {
				filePath = pathParts[0]
			}
		}
	} else if strings.Contains(entry.Message, "Successfully rebalanced") {
		operation = "Success"
		color = colorGreen // Always green for success

		// Extract file path and get just the filename
		parts := strings.Split(entry.Message, "Successfully rebalanced ")
		if len(parts) > 1 {
			fullPath := parts[1]

			// If path contains a speed component, remove it before extracting filename
			if strings.Contains(fullPath, " at ") {
				fullPath = strings.Split(fullPath, " at ")[0]
			}

			// Check if we should show full paths
			// We need to check entry.Data for custom fields passed from rebalancer
			showFullPathsVal, ok := entry.Data["show_full_paths"]
			showFullPaths := false
			if ok {
				if boolVal, ok := showFullPathsVal.(bool); ok {
					showFullPaths = boolVal
				}
			}

			if showFullPaths {
				// Use the full path directly
				filePath = fullPath
			} else {
				// Extract just the filename from the full path
				_, filePath = filepath.Split(fullPath)
			}

			// If there's a speed measurement, preserve it
			if strings.Contains(parts[1], " at ") {
				speedPart := strings.Split(parts[1], " at ")[1]
				speedStr = "at " + speedPart
			}
		}
	} else if strings.Contains(entry.Message, "permission") {
		color = colorYellow
	} else if strings.Contains(entry.Message, "File missing") ||
		strings.Contains(entry.Message, "no longer on disk") {
		color = colorYellow
	}

	// Construct the formatted log message
	var msg string
	if operation != "" && filePath != "" {
		// Format with double quotes around filename and hyphens between elements
		if speedStr != "" {
			if operation == "Success" {
				// Bold success messages
				msg = fmt.Sprintf("%s - %s%s%s%s - \"%s\" %s\n", timestamp, color, colorBold, operation, colorReset, filePath, speedStr)
			} else {
				msg = fmt.Sprintf("%s - %s%s%s - \"%s\" %s\n", timestamp, color, operation, colorReset, filePath, speedStr)
			}
		} else {
			if operation == "Success" {
				// Bold success messages
				msg = fmt.Sprintf("%s - %s%s%s%s - \"%s\"\n", timestamp, color, colorBold, operation, colorReset, filePath)
			} else {
				msg = fmt.Sprintf("%s - %s%s%s - \"%s\"\n", timestamp, color, operation, colorReset, filePath)
			}
		}
	} else {
		// For other messages apply any color if set, with hyphens
		if color != "" {
			msg = fmt.Sprintf("%s - %s%s%s\n", timestamp, color, entry.Message, colorReset)
		} else {
			msg = fmt.Sprintf("%s - %s\n", timestamp, entry.Message)
		}
	}

	return []byte(msg), nil
}

// parseSpeed extracts a float speed value from a string like "110.04 MB/s"
func parseSpeed(speedStr string) float64 {
	speedStr = strings.TrimSuffix(strings.TrimSpace(speedStr), "MB/s")
	speedStr = strings.TrimSpace(speedStr)
	speed, _ := strconv.ParseFloat(speedStr, 64)
	return speed
}

// printUsage prints a detailed help message with examples
func printUsage() {
	fmt.Println("go-zfs-rebalance")
	fmt.Println("===============================")
	fmt.Println("A tool for reducing ZFS fragmentation by copying files in-place.")
	fmt.Println("This helps redistribute data blocks and can improve performance on fragmented pools.")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  rebalance [options] <path>")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --process-hardlinks  Process files with multiple hardlinks (skipped by default)")
	fmt.Println("  --passes X           Number of times a file may be rebalanced (default: 10, 0 for unlimited)")
	fmt.Println("  --concurrency X      Number of files to process concurrently (default: auto - half of CPU cores, minimum 2)")
	fmt.Println("  --no-cleanup-balance Disable automatic removal of stale .balance files (enabled by default)")
	fmt.Println("  --no-random          Process files in directory order instead of random order (default)")
	fmt.Println("  --debug              Enable debug logging (shows all operations, not just successes/errors)")
	fmt.Println("  --size-threshold X   Only show success messages for files >= X MB (default: 0)")
	fmt.Println("  --checksum TYPE      Checksum type to use (sha256 or md5, default: sha256)")
	fmt.Println("  --halt-on-missing    Halt processing when a file is no longer on disk")
	fmt.Println("  --filename-only      Display only filenames instead of full paths in logs (full paths by default)")
	fmt.Println("  --version            Show version information")
	fmt.Println("  --help               Show this help message")
	fmt.Println()
	fmt.Println("Features:")
	fmt.Println("  * Files are verified using SHA256 checksums (or MD5 if specified) to ensure data integrity")
	fmt.Println("  * File attributes (permissions, timestamps, ownership) are preserved")
	fmt.Println("  * Graceful shutdown on CTRL+C - finishes in-progress files")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  # Rebalance all files in a directory with default settings")
	fmt.Println("  rebalance /path/to/data")
	fmt.Println()
	fmt.Println("  # Process hardlinks as well (potentially increasing space usage)")
	fmt.Println("  rebalance --process-hardlinks --concurrency 8 /path/to/data")
	fmt.Println()
	fmt.Println("  # Rebalance files multiple times (useful for severely fragmented pools)")
	fmt.Println("  rebalance --passes 3 /path/to/data")
	fmt.Println()
	fmt.Println("  # Disable random file processing order")
	fmt.Println("  rebalance --no-random /path/to/data")
	fmt.Println()
	fmt.Println("  # Disable automatic cleanup of stale .balance files")
	fmt.Println("  rebalance --no-cleanup-balance /path/to/data")
	fmt.Println()
	fmt.Println("  # Enable verbose debugging output")
	fmt.Println("  rebalance --debug /path/to/data")
	fmt.Println()
	fmt.Println("  # Only show success messages for files 20MB or larger")
	fmt.Println("  rebalance --size-threshold 20 /path/to/data")
	fmt.Println()
	fmt.Println("  # Halt processing when a file is found to be missing during rebalance")
	fmt.Println("  rebalance --halt-on-missing /path/to/data")
}

// concurrencyStr returns a string representation of the concurrency setting
func concurrencyStr(concurrency int) string {
	if concurrency <= 0 {
		return "auto"
	}
	return fmt.Sprintf("%d", concurrency)
}

// calculateConcurrency determines the number of worker threads to use
// If auto is specified (concurrency <= 0), it uses half the number of CPU cores with a minimum of 2
func calculateConcurrency(concurrency int) int {
	if concurrency > 0 {
		return concurrency
	}

	// Auto concurrency: half the number of CPU cores, minimum 2
	cpuCount := runtime.NumCPU()
	autoConcurrency := cpuCount / 2
	if autoConcurrency < 2 {
		autoConcurrency = 2
	}
	return autoConcurrency
}

func main() {
	// Set up the logger with our custom format
	log := logrus.New()
	log.Formatter = &CustomFormatter{
		TextFormatter: logrus.TextFormatter{
			DisableColors: false,
			ForceColors:   true,
		},
	}

	var (
		processHardlinks  bool
		passesFlag        int
		concurrency       int
		showHelp          bool
		noCleanupBalance  bool
		noRandomOrder     bool
		debugLogging      bool
		sizeThreshold     int
		showVersion       bool
		checksumType      string
		haltOnFileMissing bool
		showFullPaths     bool
	)

	flag.BoolVar(&processHardlinks, "process-hardlinks", false, "Process files with multiple hardlinks")
	flag.IntVar(&passesFlag, "passes", 10, "Number of times a file may be rebalanced (0 for unlimited)")
	flag.IntVar(&concurrency, "concurrency", 0, "Number of files to process concurrently (default: auto - half of CPU cores, minimum 2)")
	flag.BoolVar(&showHelp, "help", false, "Show usage")
	flag.BoolVar(&noCleanupBalance, "no-cleanup-balance", false, "Disable automatic removal of stale .balance files")
	flag.BoolVar(&noRandomOrder, "no-random", false, "Process files in directory order instead of random order")
	flag.BoolVar(&debugLogging, "debug", false, "Enable debug logging")
	flag.IntVar(&sizeThreshold, "size-threshold", 0, "Only show success messages for files >= this size in MB")
	flag.StringVar(&checksumType, "checksum", "sha256", "Checksum type to use (sha256 or md5)")
	flag.BoolVar(&showVersion, "version", false, "Show version information")
	flag.BoolVar(&haltOnFileMissing, "halt-on-missing", false, "Halt processing when a file is no longer on disk")
	flag.BoolVar(&showFullPaths, "filename-only", false, "Display only filenames in logs instead of full paths (default: show full paths)")
	flag.Parse()

	if showVersion {
		fmt.Printf("go-zfs-rebalance version %s\n", VERSION)
		os.Exit(0)
	}

	if showHelp || flag.NArg() < 1 {
		printUsage()
		os.Exit(0)
	}

	rootPath := flag.Arg(0)

	// Open DB in a temp directory
	db, err := database.OpenSQLiteDB()
	if err != nil {
		log.Errorf("Failed to open SQLite DB: %v", err)
		os.Exit(1)
	}

	// Clean up
	defer func() {
		_ = db.Close(true) // true to remove the temp DB directory
	}()

	log.Infof("Start rebalancing at %s", time.Now().Format("2006-01-02 15:04:05"))
	log.Infof("OS: %s", runtime.GOOS)
	log.Infof("Path: %s", rootPath)
	log.Infof("Passes: %d", passesFlag)
	log.Infof("Process Hardlinks: %t", processHardlinks)
	log.Infof("Concurrency: %s", concurrencyStr(concurrency))
	log.Infof("Cleanup Balance Files: %t", !noCleanupBalance)
	log.Infof("Random Order: %t", !noRandomOrder)
	log.Infof("Debug Logging: %t", debugLogging)
	log.Infof("Size Threshold: %d MB", sizeThreshold)
	log.Infof("Checksum Type: %s", checksumType)
	log.Infof("Halt On Missing Files: %t", haltOnFileMissing)
	log.Infof("Show Full Paths: %t", !showFullPaths)
	log.Infof("SQLite DB Path: %s", db.Path)

	// Set up log level filtering
	if !debugLogging {
		// Only show important messages when not in debug mode
		log.SetLevel(logrus.WarnLevel) // Only show warnings and errors by default
	} else {
		log.SetLevel(logrus.InfoLevel) // Show all messages in debug mode
	}

	// Convert checksum string to ChecksumType
	var checksumTypeEnum fileutil.ChecksumType
	switch strings.ToLower(checksumType) {
	case "md5":
		checksumTypeEnum = fileutil.ChecksumMD5
	case "sha256":
		checksumTypeEnum = fileutil.ChecksumSHA256
	default:
		log.Errorf("Invalid checksum type: %s. Must be sha256 or md5", checksumType)
		os.Exit(1)
	}

	// Calculate the actual concurrency to use
	actualConcurrency := calculateConcurrency(concurrency)

	// If auto concurrency was used, log the actual number of workers
	if concurrency <= 0 {
		log.Infof("Auto concurrency selected: using %d workers based on %d CPUs", actualConcurrency, runtime.NumCPU())
	}

	config := &rebalance.Config{
		SkipHardlinks:       !processHardlinks,
		PassesLimit:         passesFlag,
		Concurrency:         actualConcurrency,
		RootPath:            rootPath,
		Logger:              log,
		CleanupBalanceFiles: !noCleanupBalance,
		RandomOrder:         !noRandomOrder,
		SizeThresholdMB:     sizeThreshold,
		ChecksumType:        checksumTypeEnum,
		HaltOnFileMissing:   haltOnFileMissing,
		ShowFullPaths:       !showFullPaths,
	}

	rebalancer := rebalance.NewRebalancer(config, db)

	// Set up signal handling for graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// Create a done channel that will be closed when we need to force exit
	done := make(chan struct{})

	// Handle signals in a separate goroutine
	go func() {
		sig := <-signalChan
		log.Warnf("%sReceived signal %v, initiating graceful shutdown...%s", colorYellow, sig, colorReset)

		// Signal the rebalancer to start graceful shutdown
		rebalancer.InitiateShutdown()

		// Start a timer to force exit if shutdown takes too long
		go func() {
			// Give processes 90 seconds to clean up
			time.Sleep(90 * time.Second)
			log.Warn("Shutdown timeout reached, forcing exit")
			close(done)
		}()
	}()

	// Create a channel for rebalancer completion
	rebalanceDone := make(chan struct{})

	// Create a shared progress tracker
	progressChan := make(chan int, 100)
	files, err := rebalancer.GetFiles()
	if err != nil {
		log.Errorf("Error getting file list: %v", err)
		os.Exit(1)
	}
	totalFiles := len(files)
	processedFiles := 0

	// Get pass information
	currentPass, totalPasses := rebalancer.GetPassInfo()

	// Function to print progress report
	printProgress := func() {
		// Calculate completion percentage for the current pass
		currentPassPercentage := 0
		if totalFiles > 0 {
			currentPassPercentage = int(float64(processedFiles) / float64(totalFiles) * 100)
		}

		// Calculate overall completion percentage across all passes
		overallPercentage := 0
		if totalPasses > 0 && totalFiles > 0 {
			passWeight := 100.0 / float64(totalPasses)
			overallPercentage = int(float64(currentPass-1)*passWeight + float64(currentPassPercentage)*passWeight/100.0)
		}

		// Print progress in blue and bold with pass information
		fmt.Printf("%s %s%s%sPass %d of %d: %d/%d files (%d%% of pass, %d%% overall)%s\n",
			time.Now().Format("3:04:05 PM"),
			colorBlue, colorBold, "",
			currentPass, totalPasses,
			processedFiles, totalFiles,
			currentPassPercentage,
			overallPercentage,
			colorReset)
	}

	// Show initial progress
	printProgress()

	// Start a periodic progress reporter
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				printProgress()

			case count := <-progressChan:
				processedFiles = count

			case <-rebalanceDone:
				return
			}
		}
	}()

	// Run the rebalancer in a goroutine
	go func() {
		err = rebalancer.Run(progressChan)
		close(rebalanceDone)
	}()

	// Wait for either rebalancer to finish or a forced exit
	select {
	case <-rebalanceDone:
		// Normal completion - print final progress
		printProgress()

		// Show completion message
		if err != nil {
			log.Error(err)
			os.Exit(1)
		} else {
			log.Info("All files processed successfully.")
		}
	case <-done:
		// Forced exit due to timeout
		log.Error("Forced exit: rebalance operation did not complete gracefully in time")
		os.Exit(1)
	}
}
