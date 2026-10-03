// Package database keeps each folder's rebalance progress in a small SQLite
// file, so an interrupted run can be resumed, and provides the global run lock
// that stops two rebalance runs from working at the same time.
package database

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	driverName    = "sqlite"
	appDirName    = "go-zfs-rebalance"
	lockFileName  = "run.lock"
	schemaVersion = "1"

	// applicationID marks a SQLite file as one of our state files ("ZRBL"). SQLite keeps it in
	// the file's header, so it can be checked without opening the database, even when the file
	// is damaged or locked by another program.
	applicationID = 0x5A52424C
	// appIDOffset is where SQLite keeps the application ID: 4 bytes, big-endian.
	appIDOffset = 68
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

	// errInUse marks a state file that another program has locked.
	errInUse = errors.New("progress file in use")
)

// SQLite result codes that mean another connection holds a lock on the file.
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// Test seams: who the process runs as, where root's home folder is, and how
// long SQLite waits for another connection's lock before giving up.
var (
	geteuid     = os.Geteuid
	rootHome    = lookupRootHome
	busyTimeout = 5 * time.Second
)

// sqliteMagic is the header every SQLite 3 database file starts with.
var sqliteMagic = []byte("SQLite format 3\x00")

// sidecarSuffixes are the extra files SQLite may keep next to a database.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// DefaultDir returns the folder that holds state files and the run lock:
// $XDG_STATE_HOME/go-zfs-rebalance, or ~/.local/state/go-zfs-rebalance when
// XDG_STATE_HOME is unset. The folder is created (mode 0700) if needed.
//
// When running as root, ~ is root's own home folder from the user database,
// not $HOME: sudo on macOS keeps the user's HOME, and root-owned folders made
// there would get in the way of the user's own programs.
func DefaultDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	// The XDG spec says relative values must be ignored.
	if !filepath.IsAbs(base) {
		home, err := homeDir()
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

// homeDir is the home folder of the user the process runs as.
func homeDir() (string, error) {
	if geteuid() == 0 {
		if home, err := rootHome(); err == nil {
			return home, nil
		}
	}
	return os.UserHomeDir()
}

// lookupRootHome returns root's home folder from the user database, such as
// /root on Linux or /var/root on macOS.
func lookupRootHome() (string, error) {
	u, err := user.LookupId("0")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(u.HomeDir) {
		return "", fmt.Errorf("root's home folder %q isn't a full path", u.HomeDir)
	}
	return u.HomeDir, nil
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
	Resumed          bool   // existing state was reused
	Discarded        bool   // existing state file was deleted because resume=false
	PreviousEntries  int    // rows in the discarded/resumed DB
	PreviousRoot     string // the folder the discarded state was for, when it wasn't this one
	MissingForResume bool   // resume requested but no state file existed (started fresh)
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
//
// Only a file that is positively one of our state files (by the application
// ID in its header) is ever reused or deleted. Anything else, an empty file,
// one of ours that can't be read, or one another program has locked, is left
// alone and Open returns an error saying what to do.
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
	switch {
	case errors.Is(err, fs.ErrNotExist):
		info.MissingForResume = resume
	case errors.Is(err, errForeign):
		return nil, OpenInfo{}, fmt.Errorf("%q isn't a rebalance progress file, so it was left alone. Choose a different --db file", abs)
	case errors.Is(err, errInUse):
		return nil, OpenInfo{}, fmt.Errorf("the progress file %q is being used by another program right now, so it was left alone. Close that program and try again, or choose a different --db file", abs)
	case errors.Is(err, errDamaged):
		return nil, OpenInfo{}, fmt.Errorf("couldn't read the saved progress in %q (%s), so it was left alone. It may be damaged. If you don't need it, delete it yourself (with any -wal or -shm file next to it), or choose a different --db file", abs, reasonOf(err))
	case err != nil:
		return nil, OpenInfo{}, fmt.Errorf("couldn't check the progress file %q: %w", abs, err)
	case !resume:
		info.Discarded = true
		info.PreviousEntries = saved.entries
		if saved.root != root {
			info.PreviousRoot = saved.root
		}
	case saved.root != root:
		return nil, OpenInfo{}, fmt.Errorf("%w: %q holds progress for %q, but this run is for %q. Run without --resume to start fresh, or choose a different --db file",
			ErrRootMismatch, abs, saved.root, root)
	case saved.version != schemaVersion:
		return nil, OpenInfo{}, fmt.Errorf("the saved progress in %q was made by a different version of rebalance. Run again without --resume to start fresh", abs)
	default:
		info.Resumed = true
		info.PreviousEntries = saved.entries
	}

	if !info.Resumed {
		if err := createFresh(abs, root); err != nil {
			return nil, OpenInfo{}, err
		}
	}
	db, err := openRW(abs)
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
}

// inspect reads an existing file without changing it. It returns an error
// wrapping fs.ErrNotExist when there is no file, errForeign when the file isn't
// one of ours, errInUse when another program has it locked, and errDamaged when
// it is one of ours but can't be read.
func inspect(path string) (savedState, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return savedState{}, err
	}
	if !fi.Mode().IsRegular() {
		return savedState{}, errForeign
	}
	if ours, err := hasOurHeader(path); err != nil {
		return savedState{}, err
	} else if !ours {
		return savedState{}, errForeign
	}
	s, err := readState(path)
	var coded interface{ Code() int }
	switch {
	case err == nil:
		return s, nil
	case errors.As(err, &coded) && (coded.Code()&0xff == sqliteBusy || coded.Code()&0xff == sqliteLocked):
		return savedState{}, &unreadableError{kind: errInUse, err: err}
	}
	return savedState{}, &unreadableError{kind: errDamaged, err: err}
}

// unreadableError is one of our state files that couldn't be read. It matches
// its kind (errDamaged or errInUse) with errors.Is.
type unreadableError struct {
	kind error
	err  error // what went wrong, as SQLite reported it
}

func (e *unreadableError) Error() string        { return e.kind.Error() + ": " + e.err.Error() }
func (e *unreadableError) Is(target error) bool { return target == e.kind }
func (e *unreadableError) Unwrap() error        { return e.err }

// readState reads the recorded root, schema version and row count. It opens
// the file read-only and without our pragmas, so the file is never changed.
func readState(path string) (savedState, error) {
	conn, err := sql.Open(driverName, dsn(path, url.Values{
		"mode":    {"ro"},
		"_pragma": {busyPragma()},
	}))
	if err != nil {
		return savedState{}, err
	}
	defer conn.Close()

	var ours int
	err = conn.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('meta', 'rebalances')`).Scan(&ours)
	switch {
	case err != nil:
		return savedState{}, err
	case ours != 2:
		return savedState{}, errors.New("its tables are missing")
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

// hasOurHeader reports whether path starts with the SQLite 3 file header and
// carries our application ID.
func hasOurHeader(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, appIDOffset+4)
	if _, err := io.ReadFull(f, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return bytes.HasPrefix(header, sqliteMagic) &&
		binary.BigEndian.Uint32(header[appIDOffset:]) == applicationID, nil
}

// createFresh replaces any old state file and its sidecars with a new, empty
// one for root. The new file is set up under a temporary name and renamed into
// place, so path never holds a half-made file that wouldn't be recognised as
// ours. Old sidecars go first, so an old -wal is never applied to the new file.
func createFresh(path, root string) error {
	var errs []error
	for _, name := range sidecarFiles(path) {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("couldn't remove the old progress file %q: %w", path, err)
	}

	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-*")
	if err != nil {
		return fmt.Errorf("couldn't create the progress file %q: %w", path, err)
	}
	tmp := f.Name()
	err = errors.Join(f.Close(), initFile(tmp, root), os.Rename(tmp, path))
	if err != nil {
		for _, name := range append(sidecarFiles(tmp), tmp) {
			_ = os.Remove(name)
		}
		return fmt.Errorf("couldn't create the progress file %q: %w", path, err)
	}
	return nil
}

// initFile writes our schema, root and application ID into the empty file at
// path. It uses SQLite's default rollback journal, so everything lands in the
// file itself, never only in a -wal file beside it; openRW switches the file to
// WAL mode afterwards.
func initFile(path, root string) error {
	conn, err := sql.Open(driverName, dsn(path, url.Values{"_pragma": {busyPragma()}}))
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1)
	defer conn.Close()

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE rebalances (path TEXT PRIMARY KEY, count INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		fmt.Sprintf(`PRAGMA application_id = %d`, applicationID),
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('root', ?), ('schema_version', ?)`, root, schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return conn.Close()
}

func sidecarFiles(path string) []string {
	files := make([]string, 0, len(sidecarSuffixes))
	for _, s := range sidecarSuffixes {
		files = append(files, path+s)
	}
	return files
}

// openRW opens one of our state files for writing.
func openRW(path string) (*DB, error) {
	conn, err := sql.Open(driverName, dsn(path, url.Values{
		"_pragma": {busyPragma(), "journal_mode(WAL)", "synchronous(NORMAL)"},
	}))
	if err != nil {
		return nil, fmt.Errorf("couldn't open the progress file %q: %w", path, err)
	}
	// One connection serialises all writers, so concurrent Increments never
	// see "database is locked".
	conn.SetMaxOpenConns(1)

	db := &DB{conn: conn, path: path}
	db.inc, err = conn.Prepare(`INSERT INTO rebalances (path, count, updated_at) VALUES (?1, 1, ?2)
		ON CONFLICT (path) DO UPDATE SET count = count + 1, updated_at = excluded.updated_at
		RETURNING count`)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("couldn't set up the progress file %q: %w", path, err)
	}
	return db, nil
}

// reasonOf is the text of the innermost error, such as "file is not a
// database (26)".
func reasonOf(err error) string {
	for next := errors.Unwrap(err); next != nil; next = errors.Unwrap(err) {
		err = next
	}
	return err.Error()
}

func busyPragma() string { return fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()) }

// dsn builds a file: URI so paths containing '?', '#' or '%' are passed to
// SQLite intact.
func dsn(path string, query url.Values) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	return u.String()
}
