package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// DB represents a database connection and its path
type DB struct {
	*sql.DB
	Path string
}

// OpenSQLiteDB creates a temporary directory for the SQLite file and returns a DB.
func OpenSQLiteDB() (*DB, error) {
	tmpDir, err := os.MkdirTemp("", "rebalance_db_")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	dbPath := filepath.Join(tmpDir, "rebalance.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create table if not exists
	createTable := `
    CREATE TABLE IF NOT EXISTS rebalances (
        file_path TEXT PRIMARY KEY,
        count INT
    );`
	_, err = db.Exec(createTable)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	return &DB{DB: db, Path: dbPath}, nil
}

// GetRebalanceCount retrieves the current rebalance count for a file from the SQLite DB.
func (db *DB) GetRebalanceCount(filePath string) (int, error) {
	row := db.DB.QueryRow("SELECT count FROM rebalances WHERE file_path = ?", filePath)
	var count int
	err := row.Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}

// SetRebalanceCount updates (or inserts) the rebalance count for a file in the DB.
func (db *DB) SetRebalanceCount(filePath string, newCount int) error {
	_, err := db.DB.Exec(`
        INSERT INTO rebalances (file_path, count)
        VALUES (?, ?)
        ON CONFLICT(file_path) DO UPDATE SET
        count = excluded.count
    `, filePath, newCount)
	return err
}

// Close closes the database and optionally removes the database directory
func (db *DB) Close(removeDir bool) error {
	err := db.DB.Close()
	if removeDir && err == nil {
		err = os.RemoveAll(filepath.Dir(db.Path))
	}
	return err
}
