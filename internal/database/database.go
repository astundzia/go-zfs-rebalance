// Package database keeps each folder's rebalance progress in a small SQLite
// file, so an interrupted run can be resumed, and provides the global run lock
// that stops two rebalance runs from working at the same time.
package database

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	driverName    = "sqlite"
	appDirName    = "go-zfs-rebalance"
	lockFileName  = "run.lock"
	schemaVersion = "1"
)

var (
	// ErrRootMismatch means the saved progress belongs to a different folder
	// than the one being rebalanced.
	ErrRootMismatch = errors.New("the saved progress is for a different folder")

	// ErrLocked means another rebalance run is holding the run lock.
	ErrLocked = errors.New("another rebalance is already running")

	// errForeign marks an existing file that is not one of our state files.
	errForeign = errors.New("not a rebalance progress file")

	// errDamaged marks a state file that is ours but can't be read.
	errDamaged = errors.New("unreadable progress file")
)

// sqliteMagic is the header every SQLite 3 database file starts with.
var sqliteMagic = []byte("SQLite format 3\x00")

// sidecarSuffixes are the extra files SQLite may keep next to a database.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// DefaultDir returns the folder that holds state files and the run lock:
// $XDG_STATE_HOME/go-zfs-rebalance, or ~/.local/state/go-zfs-rebalance when
// XDG_STATE_HOME is unset. The folder is created (mode 0700) if needed.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	// The XDG spec says relative values must be ignored.
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("couldn't find your home folder to keep progress in (use --db to choose a file): %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, appDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("couldn't create the folder %q to keep progress in: %w", dir, err)
	}
	return dir, nil
}

// DefaultPath returns the state file for root inside DefaultDir. The name is
// the first 12 hex digits of the SHA-256 of the cleaned root path, so every
// folder gets its own stable file.
func DefaultPath(root string) (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:12]+".db"), nil
}

// OpenInfo describes what Open found on disk.
type OpenInfo struct {
	Resumed          bool // existing state was reused
	Discarded        bool // existing state file was deleted because resume=false
	PreviousEntries  int  // rows in the discarded/resumed DB
	MissingForResume bool // resume requested but no state file existed (started fresh)
}

// DB is an open state file. It is safe for concurrent use.
type DB struct {
	conn *sql.DB
	inc  *sql.Stmt
	path string
}

// Open opens the state file at path for the folder root.
//
// With resume=false any existing state (and its SQLite sidecar files) is
// deleted and a fresh file is created. With resume=true an existing file is
// reused, provided it was made for the same root; otherwise ErrRootMismatch is
// returned. A missing file on resume starts fresh and sets MissingForResume.
// A file that isn't one of our state files is never deleted or changed.
func Open(path, root string, resume bool) (*DB, OpenInfo, error) {
	if !filepath.IsAbs(root) {
		return nil, OpenInfo{}, fmt.Errorf("database: root %q must be an absolute path", root)
	}
	root = filepath.Clean(root)
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, OpenInfo{}, fmt.Errorf("couldn't use %q to keep progress in: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, OpenInfo{}, fmt.Errorf("couldn't create the folder for the progress file %q: %w", abs, err)
	}

	var info OpenInfo
	saved, err := inspect(abs)
	damaged := errors.Is(err, errDamaged)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		info.MissingForResume = resume
	case errors.Is(err, errForeign):
		return nil, OpenInfo{}, fmt.Errorf("%q doesn't look like a rebalance progress file, so it was left alone. Choose a different --db file", abs)
	case err != nil && !damaged:
		return nil, OpenInfo{}, fmt.Errorf("couldn't check the progress file %q: %w", abs, err)
	case damaged && resume:
		return nil, OpenInfo{}, fmt.Errorf("couldn't read the saved progress in %q, it may be damaged. Run again without --resume to start fresh (%w)", abs, err)
	case resume && saved.empty:
		info.MissingForResume = true
	case resume && saved.root != root:
		return nil, OpenInfo{}, fmt.Errorf("%w: %q holds progress for %q, but this run is for %q. Run without --resume to start fresh, or choose a different --db file",
			ErrRootMismatch, abs, saved.root, root)
	case resume && saved.version != schemaVersion:
		return nil, OpenInfo{}, fmt.Errorf("the saved progress in %q was made by a different version of rebalance. Run again without --resume to start fresh", abs)
	case resume:
		info.Resumed = true
		info.PreviousEntries = saved.entries
	default:
		// A damaged file can't be counted, but it is still ours to replace.
		info.Discarded = true
		info.PreviousEntries = saved.entries
	}

	if !info.Resumed {
		if err := createFresh(abs); err != nil {
			return nil, OpenInfo{}, err
		}
	}
	db, err := openRW(abs, root)
	if err != nil {
		return nil, OpenInfo{}, err
	}
	return db, info, nil
}

// Counts returns the rebalance count of every recorded file, keyed by its
// slash-separated path relative to the root.
func (db *DB) Counts() (map[string]int, error) {
	rows, err := db.conn.Query(`SELECT path, count FROM rebalances`)
	if err != nil {
		return nil, fmt.Errorf("couldn't load the saved progress: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var rel string
		var n int
		if err := rows.Scan(&rel, &n); err != nil {
			return nil, fmt.Errorf("couldn't load the saved progress: %w", err)
		}
		counts[rel] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("couldn't load the saved progress: %w", err)
	}
	return counts, nil
}

// Increment adds one to the count for rel and returns the new count.
// It is safe to call from many goroutines at once.
func (db *DB) Increment(rel string) (int, error) {
	var n int
	if err := db.inc.QueryRow(rel, time.Now().Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("couldn't save progress: %w", err)
	}
	return n, nil
}

// Files returns the absolute paths of the state file and the sidecar files
// SQLite may create next to it, whether or not they exist right now.
func (db *DB) Files() []string {
	return append([]string{db.path}, sidecarFiles(db.path)...)
}

// Path returns the absolute path of the state file.
func (db *DB) Path() string { return db.path }

// Close flushes and closes the state file.
func (db *DB) Close() error {
	return errors.Join(db.inc.Close(), db.conn.Close())
}

// savedState is what inspect read from an existing state file.
type savedState struct {
	root    string
	version string
	entries int
	empty   bool // the file exists but nothing was ever saved in it
}

// inspect reads an existing file without changing it. It returns an error
// wrapping fs.ErrNotExist when there is no file, errForeign when the file is
// clearly not one of ours, and errDamaged when it is an SQLite database that
// can't be read.
func inspect(path string) (savedState, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return savedState{}, err
	}
	if !fi.Mode().IsRegular() {
		return savedState{}, errForeign
	}
	if fi.Size() == 0 {
		return savedState{empty: true}, nil
	}
	if ok, err := hasSQLiteHeader(path); err != nil {
		return savedState{}, err
	} else if !ok {
		return savedState{}, errForeign
	}
	s, err := readState(path)
	if err != nil && !errors.Is(err, errForeign) {
		return savedState{}, fmt.Errorf("%w: %w", errDamaged, err)
	}
	return s, err
}

// readState reads the recorded root, schema version and row count. It opens
// the file read-only and without our pragmas, so a database that isn't ours
// is never changed (for example, switched to WAL mode).
func readState(path string) (savedState, error) {
	conn, err := sql.Open(driverName, dsn(path, url.Values{
		"mode":    {"ro"},
		"_pragma": {"busy_timeout(5000)"},
	}))
	if err != nil {
		return savedState{}, err
	}
	defer conn.Close()

	var tables, ours int
	err = conn.QueryRow(`SELECT count(*), coalesce(sum(name IN ('meta', 'rebalances')), 0)
		FROM sqlite_master WHERE type = 'table'`).Scan(&tables, &ours)
	switch {
	case err != nil:
		return savedState{}, err
	case tables == 0:
		return savedState{empty: true}, nil
	case ours != 2:
		return savedState{}, errForeign
	}

	var s savedState
	rows, err := conn.Query(`SELECT key, value FROM meta WHERE key IN ('root', 'schema_version')`)
	if err != nil {
		return savedState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return savedState{}, err
		}
		if k == "root" {
			s.root = v
		} else {
			s.version = v
		}
	}
	if err := rows.Err(); err != nil {
		return savedState{}, err
	}
	if s.root == "" {
		return savedState{}, errors.New("no folder is recorded in it")
	}
	if err := conn.QueryRow(`SELECT count(*) FROM rebalances`).Scan(&s.entries); err != nil {
		return savedState{}, err
	}
	return s, nil
}

// hasSQLiteHeader reports whether path starts with the SQLite 3 file header.
func hasSQLiteHeader(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(f, header); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(header, sqliteMagic), nil
}

// createFresh deletes any old state file and its sidecars, then creates an
// empty, private (0600) file. Sidecars go first, so a failed removal never
// leaves an old -wal or -journal beside a new database.
func createFresh(path string) error {
	var errs []error
	for _, name := range append(sidecarFiles(path), path) {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("couldn't remove the old progress file %q: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("couldn't create the progress file %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("couldn't create the progress file %q: %w", path, err)
	}
	return nil
}

func sidecarFiles(path string) []string {
	files := make([]string, 0, len(sidecarSuffixes))
	for _, s := range sidecarSuffixes {
		files = append(files, path+s)
	}
	return files
}

// openRW opens path for writing, makes sure the schema exists and records root
// if the file is new.
func openRW(path, root string) (*DB, error) {
	conn, err := sql.Open(driverName, dsn(path, url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)"},
	}))
	if err != nil {
		return nil, fmt.Errorf("couldn't open the progress file %q: %w", path, err)
	}
	// One connection serialises all writers, so concurrent Increments never
	// see "database is locked".
	conn.SetMaxOpenConns(1)

	db := &DB{conn: conn, path: path}
	if err := db.init(root); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("couldn't set up the progress file %q: %w", path, err)
	}
	return db, nil
}

func (db *DB) init(root string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE IF NOT EXISTS rebalances (path TEXT PRIMARY KEY, count INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('root', ?), ('schema_version', ?)
		ON CONFLICT (key) DO NOTHING`, root, schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.inc, err = db.conn.Prepare(`INSERT INTO rebalances (path, count, updated_at) VALUES (?1, 1, ?2)
		ON CONFLICT (path) DO UPDATE SET count = count + 1, updated_at = excluded.updated_at
		RETURNING count`)
	return err
}

// dsn builds a file: URI so paths containing '?', '#' or '%' are passed to
// SQLite intact.
func dsn(path string, query url.Values) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	return u.String()
}
