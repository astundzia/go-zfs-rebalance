package rebalance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/internal/database"
	_ "github.com/mattn/go-sqlite3"
	log "github.com/sirupsen/logrus"
)

func setupTest(t *testing.T) (*Rebalancer, *database.DB, string, func()) {
	// Create a test directory
	testDir, err := os.MkdirTemp("", "rebalance_test")
	if err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Create a test file
	testFile := filepath.Join(testDir, "test_file.txt")
	err = os.WriteFile(testFile, []byte("rebalance test data"), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Open database
	db, err := database.OpenSQLiteDB()
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}

	// Create logger
	logger := log.New()
	logger.SetOutput(os.Stdout)
	logger.SetLevel(log.DebugLevel)

	// Create config
	config := &Config{
		SkipHardlinks: false,
		PassesLimit:   3,
		Concurrency:   2,
		RootPath:      testDir,
		Logger:        logger,
	}

	// Create rebalancer
	r := NewRebalancer(config, db)

	// Return cleanup function
	cleanup := func() {
		os.RemoveAll(testDir)
		db.Close(true)
	}

	return r, db, testFile, cleanup
}

func TestRebalanceFile(t *testing.T) {
	r, _, testFile, cleanup := setupTest(t)
	defer cleanup()

	// Test rebalancing a file
	err := r.RebalanceFile(testFile)
	if err != nil {
		t.Errorf("RebalanceFile failed: %v", err)
	}

	// Verify the file still exists after rebalancing
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Errorf("File does not exist after rebalancing")
	}

	// Read the file to verify its content is intact
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Errorf("Failed to read rebalanced file: %v", err)
	}

	if string(content) != "rebalance test data" {
		t.Errorf("File content changed after rebalancing. Got: %s, Want: rebalance test data", string(content))
	}
}

func TestRebalanceCounting(t *testing.T) {
	r, db, testFile, cleanup := setupTest(t)
	defer cleanup()

	// Run rebalancing 3 times (passes limit is set to 3)
	for i := 0; i < 3; i++ {
		err := r.RebalanceFile(testFile)
		if err != nil {
			t.Errorf("RebalanceFile failed on pass %d: %v", i+1, err)
		}
	}

	// Check the count in the database
	count, err := db.GetRebalanceCount(testFile)
	if err != nil {
		t.Errorf("Failed to get rebalance count: %v", err)
	}

	if count != 3 {
		t.Errorf("Expected rebalance count 3, got %d", count)
	}

	// Try rebalancing a 4th time (should be skipped due to passes limit)
	err = r.RebalanceFile(testFile)
	if err != nil {
		t.Errorf("RebalanceFile failed on 4th pass: %v", err)
	}

	// Count should still be 3
	count, err = db.GetRebalanceCount(testFile)
	if err != nil {
		t.Errorf("Failed to get rebalance count after 4th pass: %v", err)
	}

	if count != 3 {
		t.Errorf("Expected rebalance count to remain 3, got %d", count)
	}
}

func TestGatherFiles(t *testing.T) {
	r, _, testFile, cleanup := setupTest(t)
	defer cleanup()

	// Create an additional file in a subdirectory
	subDir := filepath.Join(r.config.RootPath, "subdir")
	err := os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	subFile := filepath.Join(subDir, "subfile.txt")
	err = os.WriteFile(subFile, []byte("subdir test data"), 0644)
	if err != nil {
		t.Fatalf("Failed to create file in subdirectory: %v", err)
	}

	// Test GatherFiles
	files, err := r.GatherFiles()
	if err != nil {
		t.Errorf("GatherFiles failed: %v", err)
	}

	// Should find 2 files
	if len(files) != 2 {
		t.Errorf("Expected 2 files, got %d", len(files))
	}

	// Files should include the test file and subdir file
	foundTestFile := false
	foundSubFile := false

	for _, f := range files {
		if f == testFile {
			foundTestFile = true
		} else if f == subFile {
			foundSubFile = true
		}
	}

	if !foundTestFile {
		t.Errorf("GatherFiles did not find the test file")
	}

	if !foundSubFile {
		t.Errorf("GatherFiles did not find the file in the subdirectory")
	}
}

func TestRun(t *testing.T) {
	r, _, _, cleanup := setupTest(t)
	defer cleanup()

	// Create nil channel since we don't need progress updates in the test
	var progressChan chan<- int = nil

	// Test Run
	err := r.Run(progressChan)
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}
}
