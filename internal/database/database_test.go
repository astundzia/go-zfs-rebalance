package database

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenSQLiteDB(t *testing.T) {
	// Open database
	db, err := OpenSQLiteDB()
	require.NoError(t, err, "Should open DB without error")
	defer db.Close(true)

	// Verify that the database path exists
	if _, err := os.Stat(db.Path); os.IsNotExist(err) {
		t.Errorf("Database file does not exist at path: %s", db.Path)
	}

	// Verify that the database is in a temp directory
	if !filepath.HasPrefix(db.Path, os.TempDir()) {
		t.Errorf("Database file not in temp directory. Path: %s", db.Path)
	}

	// Verify that we can ping the database
	if err := db.DB.Ping(); err != nil {
		t.Fatalf("Failed to ping database: %v", err)
	}
}

func TestRebalanceCountFunctions(t *testing.T) {
	// Open database
	db, err := OpenSQLiteDB()
	if err != nil {
		t.Fatalf("OpenSQLiteDB failed: %v", err)
	}
	defer db.Close(true)

	testPath := "/test/path/file.txt"

	// Test GetRebalanceCount on non-existent entry
	count, err := db.GetRebalanceCount(testPath)
	if err != nil {
		t.Errorf("GetRebalanceCount failed on non-existent entry: %v", err)
	}
	if count != 0 {
		t.Errorf("Expected count 0 for non-existent entry, got %d", count)
	}

	// Test SetRebalanceCount
	err = db.SetRebalanceCount(testPath, 5)
	if err != nil {
		t.Errorf("SetRebalanceCount failed: %v", err)
	}

	// Verify count was set
	count, err = db.GetRebalanceCount(testPath)
	if err != nil {
		t.Errorf("GetRebalanceCount failed after set: %v", err)
	}
	if count != 5 {
		t.Errorf("Expected count 5 after set, got %d", count)
	}

	// Test update of existing entry
	err = db.SetRebalanceCount(testPath, 10)
	if err != nil {
		t.Errorf("SetRebalanceCount update failed: %v", err)
	}

	// Verify count was updated
	count, err = db.GetRebalanceCount(testPath)
	if err != nil {
		t.Errorf("GetRebalanceCount failed after update: %v", err)
	}
	if count != 10 {
		t.Errorf("Expected count 10 after update, got %d", count)
	}
}

func TestDBClose(t *testing.T) {
	// Open database
	db, err := OpenSQLiteDB()
	if err != nil {
		t.Fatalf("OpenSQLiteDB failed: %v", err)
	}

	// Remember the database directory
	dbDir := filepath.Dir(db.Path)

	// Test Close with removeDir=false
	err = db.Close(false)
	if err != nil {
		t.Errorf("Close(false) failed: %v", err)
	}

	// Verify directory still exists
	if _, err := os.Stat(dbDir); os.IsNotExist(err) {
		t.Errorf("DB directory was removed despite removeDir=false")
	}

	// Open again
	db, err = OpenSQLiteDB()
	if err != nil {
		t.Fatalf("OpenSQLiteDB failed: %v", err)
	}

	dbDir = filepath.Dir(db.Path)

	// Test Close with removeDir=true
	err = db.Close(true)
	if err != nil {
		t.Errorf("Close(true) failed: %v", err)
	}

	// Verify directory was removed
	if _, err := os.Stat(dbDir); !os.IsNotExist(err) {
		t.Errorf("DB directory was not removed despite removeDir=true")
		// Clean up manually in case this test fails
		_ = os.RemoveAll(dbDir)
	}
}
