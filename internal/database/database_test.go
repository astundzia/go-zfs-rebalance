package database

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRoot = "/tank/media"

// mustOpen opens path and closes it when the test ends.
func mustOpen(t *testing.T, path, root string, resume bool) (*DB, OpenInfo) {
	t.Helper()
	db, info, err := Open(path, root, resume)
	if err != nil {
		t.Fatalf("Open(%q, %q, %v): %v", path, root, resume, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, info
}

func mustIncrement(t *testing.T, db *DB, rel string, times int) {
	t.Helper()
	for range times {
		if _, err := db.Increment(rel); err != nil {
			t.Fatalf("Increment(%q): %v", rel, err)
		}
	}
}

func mustCounts(t *testing.T, db *DB) map[string]int {
	t.Helper()
	counts, err := db.Counts()
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	return counts
}

func requireCounts(t *testing.T, db *DB, want map[string]int) {
	t.Helper()
	if got := mustCounts(t, db); !maps.Equal(got, want) {
		t.Fatalf("Counts() = %v, want %v", got, want)
	}
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %v, want %v", path, got, want)
	}
}

// requireOurHeader fails unless path's own header (not a -wal file) marks it
// as one of our state files.
func requireOurHeader(t *testing.T, path string) {
	t.Helper()
	ours, err := hasOurHeader(path)
	if err != nil || !ours {
		t.Errorf("%s doesn't carry our application ID in its header (err = %v)", path, err)
	}
}

// ourDamagedFile is a file whose header says it's one of ours but whose
// contents SQLite can't read.
func ourDamagedFile() []byte {
	b := append(slices.Clone(sqliteMagic), bytes.Repeat([]byte{0xff}, 4096)...)
	binary.BigEndian.PutUint32(b[appIDOffset:], applicationID)
	return b
}

func requireMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s should not exist (Lstat err = %v)", path, err)
	}
}

func TestOpenFreshCreatesSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "nested")
	path := filepath.Join(dir, "x.db")

	db, info := mustOpen(t, path, testRoot, false)

	if info != (OpenInfo{}) {
		t.Errorf("info = %+v, want zero value for a brand-new file", info)
	}
	if db.Path() != path {
		t.Errorf("Path() = %q, want %q", db.Path(), path)
	}
	requireMode(t, dir, 0o700)
	requireMode(t, path, 0o600)
	requireCounts(t, db, map[string]int{})

	meta := map[string]string{}
	rows, err := db.conn.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		meta[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"root": testRoot, "schema_version": "1"}; !maps.Equal(meta, want) {
		t.Errorf("meta = %v, want %v", meta, want)
	}

	var mode string
	if err := db.conn.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var id int64
	if err := db.conn.QueryRow(`PRAGMA application_id`).Scan(&id); err != nil || id != applicationID {
		t.Errorf("application_id = %#x, %v; want %#x", id, err, applicationID)
	}
	requireOurHeader(t, path)
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".x.db.new-*")); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestOpenCleansRootAndPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	db, _ := mustOpen(t, "rel.db", "/tank/media/", false)

	// t.TempDir may sit behind a symlink (macOS /var), so compare by identity.
	want, err := os.Stat(filepath.Join(dir, "rel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(db.Path()) {
		t.Fatalf("Path() = %q, want an absolute path", db.Path())
	}
	got, err := os.Stat(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("Path() = %q is not %s", db.Path(), want.Name())
	}
	var root string
	if err := db.conn.QueryRow(`SELECT value FROM meta WHERE key = 'root'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if root != testRoot {
		t.Errorf("stored root = %q, want %q", root, testRoot)
	}
}

func TestOpenRejectsRelativeRoot(t *testing.T) {
	if _, _, err := Open(filepath.Join(t.TempDir(), "x.db"), "tank/media", false); err == nil {
		t.Fatal("Open with a relative root should fail")
	}
}

func TestOpenPathWithURICharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd ?#%&= name", "state?.db")

	db, _ := mustOpen(t, path, testRoot, false)
	mustIncrement(t, db, "a", 1)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created at the exact path: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, info := mustOpen(t, path, testRoot, true)
	if !info.Resumed || info.PreviousEntries != 1 {
		t.Errorf("info = %+v, want Resumed with 1 entry", info)
	}
	requireCounts(t, db2, map[string]int{"a": 1})
}

func TestIncrementAndCounts(t *testing.T) {
	db, _ := mustOpen(t, filepath.Join(t.TempDir(), "x.db"), testRoot, false)

	steps := []struct {
		rel  string
		want int
	}{
		{"a.txt", 1},
		{"a.txt", 2},
		{"dir/b.bin", 1},
		{"a.txt", 3},
		{"ünïcode dir/ file with spaces", 1},
		{"dir/b.bin", 2},
	}
	for _, s := range steps {
		got, err := db.Increment(s.rel)
		if err != nil {
			t.Fatalf("Increment(%q): %v", s.rel, err)
		}
		if got != s.want {
			t.Errorf("Increment(%q) = %d, want %d", s.rel, got, s.want)
		}
	}
	requireCounts(t, db, map[string]int{"a.txt": 3, "dir/b.bin": 2, "ünïcode dir/ file with spaces": 1})
}

func TestConcurrentIncrementsAreExact(t *testing.T) {
	const goroutines, perGoroutine, keys = 64, 50, 8
	db, _ := mustOpen(t, filepath.Join(t.TempDir(), "x.db"), testRoot, false)

	var mu sync.Mutex
	returned := map[string][]int{}
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				rel := fmt.Sprintf("file-%d", (g+i)%keys)
				n, err := db.Increment(rel)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				returned[rel] = append(returned[rel], n)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Increment: %v", err)
	}

	perKey := goroutines * perGoroutine / keys
	want := map[string]int{}
	for k := range keys {
		want[fmt.Sprintf("file-%d", k)] = perKey
	}
	requireCounts(t, db, want)

	// Every call must have seen a distinct new count: exactly 1..perKey.
	for rel, got := range returned {
		slices.Sort(got)
		for i, n := range got {
			if n != i+1 {
				t.Fatalf("%s: returned counts are not 1..%d (position %d is %d)", rel, perKey, i, n)
			}
		}
	}
}

func TestResumeReusesCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, testRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	mustIncrement(t, db, "a", 2)
	mustIncrement(t, db, "b", 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, info := mustOpen(t, path, testRoot, true)

	if want := (OpenInfo{Resumed: true, PreviousEntries: 2}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	requireCounts(t, db2, map[string]int{"a": 2, "b": 1})
	if n, err := db2.Increment("a"); err != nil || n != 3 {
		t.Errorf("Increment after resume = %d, %v; want 3", n, err)
	}
}

// TestFreshDiscardsCrashLeftovers copies a live database (main file plus its
// -wal and -shm) to simulate a run that was killed, then starts fresh over it.
func TestFreshDiscardsCrashLeftovers(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	crashed := filepath.Join(dir, "crashed.db")

	db, _ := mustOpen(t, live, testRoot, false)
	mustIncrement(t, db, "a", 1)
	mustIncrement(t, db, "b", 2)
	mustIncrement(t, db, "c", 1)
	// The file itself must already say it's ours, even though the progress is
	// still only in the -wal file: a run killed now must not leave a file the
	// next run refuses to touch.
	requireOurHeader(t, live)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(live + suffix)
		if err != nil {
			t.Fatalf("expected a live %q file: %v", suffix, err)
		}
		if suffix == "-wal" && len(data) == 0 {
			t.Fatal("expected the progress to still be in the -wal file")
		}
		if err := os.WriteFile(crashed+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(crashed+"-journal", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	fresh, info := mustOpen(t, crashed, testRoot, false)

	if want := (OpenInfo{Discarded: true, PreviousEntries: 3}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	requireMissing(t, crashed+"-journal")
	// If the old -wal had survived, its rows would reappear here.
	requireCounts(t, fresh, map[string]int{})

	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range sidecarFiles(crashed) {
		requireMissing(t, name)
	}
}

// SQLite already ignores most leftovers next to an empty database, so check
// the removal itself.
func TestCreateFreshRemovesOldFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for _, name := range append(sidecarFiles(path), path) {
		if err := os.WriteFile(name, []byte("old progress"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := createFresh(path, testRoot); err != nil {
		t.Fatal(err)
	}

	for _, name := range sidecarFiles(path) {
		requireMissing(t, name)
	}
	requireMode(t, path, 0o600)
	requireOurHeader(t, path)
	saved, err := readState(path)
	if err != nil || saved != (savedState{root: testRoot, version: schemaVersion}) {
		t.Errorf("readState = %+v, %v; want an empty state for %s", saved, err, testRoot)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".x.db.new-*")); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestResumeAfterCrash resumes from a copy of a live database (main file plus
// its -wal and -shm), as left by a run that was killed.
func TestResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	crashed := filepath.Join(dir, "crashed.db")
	db, _ := mustOpen(t, live, testRoot, false)
	mustIncrement(t, db, "a", 2)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(live + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(crashed+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	resumed, info := mustOpen(t, crashed, testRoot, true)

	if want := (OpenInfo{Resumed: true, PreviousEntries: 1}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	requireCounts(t, resumed, map[string]int{"a": 2})
}

func TestFreshDiscardsClosedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, testRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	mustIncrement(t, db, "a", 1)
	mustIncrement(t, db, "b", 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, info := mustOpen(t, path, testRoot, false)

	if want := (OpenInfo{Discarded: true, PreviousEntries: 2}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	requireCounts(t, db2, map[string]int{})
}

// TestFreshForAnotherFolderSaysWhichFolder checks that a fresh run reusing a
// --db file made for another folder says whose progress it replaced.
func TestFreshForAnotherFolderSaysWhichFolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, "/tank/a", false)
	if err != nil {
		t.Fatal(err)
	}
	mustIncrement(t, db, "f", 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, info := mustOpen(t, path, "/tank/b", false)

	if want := (OpenInfo{Discarded: true, PreviousEntries: 1, PreviousRoot: "/tank/a"}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	requireCounts(t, db2, map[string]int{})
}

// TestDamagedStateIsLeftAlone checks that one of our files that SQLite can't
// read is never deleted: the user is told to delete it themselves.
func TestDamagedStateIsLeftAlone(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.db")
			damaged := ourDamagedFile()
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatal(err)
			}

			_, _, err := Open(path, testRoot, resume)

			for _, want := range []string{"couldn't read the saved progress in " + fmt.Sprintf("%q", path), "left alone", "delete it yourself"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to contain %q", err, want)
				}
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, damaged) {
				t.Error("the damaged file was changed")
			}
			requireMissing(t, path+"-wal")
		})
	}
}

// TestStateInUseIsLeftAlone checks that a state file another program holds an
// exclusive lock on is reported as in use, not as damaged, and isn't touched.
func TestStateInUseIsLeftAlone(t *testing.T) {
	busyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { busyTimeout = 5 * time.Second })
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, testRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	mustIncrement(t, db, "a", 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open(driverName, dsn(path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec(`PRAGMA locking_mode = EXCLUSIVE; BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}

	for _, resume := range []bool{false, true} {
		_, _, err = Open(path, testRoot, resume)
		if err == nil || !strings.Contains(err.Error(), "is being used by another program right now, so it was left alone") {
			t.Errorf("resume=%v: err = %v, want an in-use refusal", resume, err)
		}
	}
	if _, err := holder.Exec(`COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	db2, info := mustOpen(t, path, testRoot, true)
	if !info.Resumed {
		t.Errorf("info = %+v, want Resumed", info)
	}
	requireCounts(t, db2, map[string]int{"a": 1})
}

func TestResumeMissingFileStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")

	db, info := mustOpen(t, path, testRoot, true)

	if want := (OpenInfo{MissingForResume: true}); info != want {
		t.Errorf("info = %+v, want %+v", info, want)
	}
	mustIncrement(t, db, "a", 1)
	requireCounts(t, db, map[string]int{"a": 1})
}

func TestResumeRootMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, "/tank/a", false)
	if err != nil {
		t.Fatal(err)
	}
	mustIncrement(t, db, "f", 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = Open(path, "/tank/b", true)

	if !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("err = %v, want ErrRootMismatch", err)
	}
	for _, want := range []string{"/tank/a", "/tank/b", "--resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	// The saved progress is untouched and still resumable for its own root.
	db2, info := mustOpen(t, path, "/tank/a", true)
	if !info.Resumed {
		t.Errorf("info = %+v, want Resumed", info)
	}
	requireCounts(t, db2, map[string]int{"f": 1})
}

func TestResumeOtherSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _, err := Open(path, testRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, _, err = Open(path, testRoot, true)

	if err == nil || !strings.Contains(err.Error(), "different version") {
		t.Fatalf("err = %v, want a schema version error", err)
	}
}

// TestForeignFilesAreLeftAlone makes sure a --db pointing at someone else's
// file never deletes or changes it, with or without --resume.
func TestForeignFilesAreLeftAlone(t *testing.T) {
	foreignSQLite := func(t *testing.T, path string) {
		conn, err := sql.Open(driverName, dsn(path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Exec(`CREATE TABLE photos (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO photos (name) VALUES ('cat.jpg')`); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"text file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("my important notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"short binary file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("SQL"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty file", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"another app's SQLite database", foreignSQLite},
		{"another app's damaged SQLite database", func(t *testing.T, path string) {
			damaged := append(slices.Clone(sqliteMagic), bytes.Repeat([]byte{0xff}, 4096)...)
			if err := os.WriteFile(path, damaged, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a database with our tables but not our ID", func(t *testing.T, path string) {
			conn, err := sql.Open(driverName, dsn(path, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
				CREATE TABLE rebalances (path TEXT PRIMARY KEY, count INTEGER NOT NULL, updated_at INTEGER NOT NULL);
				INSERT INTO meta VALUES ('root', '/tank/media'), ('schema_version', '1')`); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume=%v", tt.name, resume), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "x.db")
				tt.setup(t, path)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}

				_, _, err = Open(path, testRoot, resume)

				if err == nil || !strings.Contains(err.Error(), "left alone") {
					t.Fatalf("err = %v, want a refusal that leaves the file alone", err)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Error("the file was changed")
				}
				requireMissing(t, path+"-wal")
			})
		}
	}

	// The case from the review: another app holds an exclusive lock on its
	// database. It must be refused straight away, without waiting for the lock.
	t.Run("another app's locked SQLite database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.db")
		foreignSQLite(t, path)
		holder, err := sql.Open(driverName, dsn(path, nil))
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Close()
		holder.SetMaxOpenConns(1)
		if _, err := holder.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, _, err = Open(path, testRoot, false)
		if err == nil || !strings.Contains(err.Error(), "isn't a rebalance progress file, so it was left alone") {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if waited := time.Since(start); waited > time.Second {
			t.Errorf("waited %v for the other app's lock", waited)
		}
		if _, err := holder.Exec(`COMMIT`); err != nil {
			t.Fatal(err)
		}
		var name string
		if err := holder.QueryRow(`SELECT name FROM photos`).Scan(&name); err != nil || name != "cat.jpg" {
			t.Errorf("the other app's data is gone: %q, %v", name, err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "x.db")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Open(path, testRoot, false); err == nil {
			t.Fatal("Open over a directory should fail")
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			t.Errorf("the directory should be left in place (err = %v)", err)
		}
	})
}

func TestFilesListsSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, _ := mustOpen(t, path, testRoot, false)

	want := []string{path, path + "-wal", path + "-shm", path + "-journal"}
	if got := db.Files(); !slices.Equal(got, want) {
		t.Errorf("Files() = %v, want %v", got, want)
	}
}

func TestDefaultDir(t *testing.T) {
	tests := []struct {
		name    string
		xdg     string // "<tmp>" is replaced by a temp dir
		wantRel string // relative to HOME, or to the XDG dir when it is used
		useXDG  bool
	}{
		{name: "XDG_STATE_HOME set", xdg: "<tmp>", useXDG: true, wantRel: "go-zfs-rebalance"},
		{name: "XDG_STATE_HOME unset", xdg: "", wantRel: ".local/state/go-zfs-rebalance"},
		{name: "relative XDG_STATE_HOME is ignored", xdg: "relative/state", wantRel: ".local/state/go-zfs-rebalance"},
	}
	// Root uses its own home folder (see TestDefaultDirAsRoot).
	oldEUID := geteuid
	t.Cleanup(func() { geteuid = oldEUID })
	geteuid = func() int { return 1000 }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			xdg := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", strings.ReplaceAll(tt.xdg, "<tmp>", xdg))

			got, err := DefaultDir()
			if err != nil {
				t.Fatal(err)
			}

			base := home
			if tt.useXDG {
				base = xdg
			}
			if want := filepath.Join(base, tt.wantRel); got != want {
				t.Errorf("DefaultDir() = %q, want %q", got, want)
			}
			requireMode(t, got, 0o700)
		})
	}
}

// TestDefaultDirAsRoot checks that a run as root keeps its state in root's
// own home folder, not in the $HOME that sudo on macOS leaves pointing at the
// user's home.
func TestDefaultDirAsRoot(t *testing.T) {
	userHome, rootsHome, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", userHome)
	oldEUID, oldHome := geteuid, rootHome
	t.Cleanup(func() { geteuid, rootHome = oldEUID, oldHome })
	geteuid = func() int { return 0 }
	rootHome = func() (string, error) { return rootsHome, nil }

	t.Setenv("XDG_STATE_HOME", "")
	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(rootsHome, ".local/state/go-zfs-rebalance"); got != want {
		t.Errorf("DefaultDir() = %q, want %q", got, want)
	}
	if entries, _ := os.ReadDir(userHome); len(entries) > 0 {
		t.Errorf("something was created in the user's home: %v", entries)
	}

	// XDG_STATE_HOME still wins, as it does for everyone.
	t.Setenv("XDG_STATE_HOME", xdg)
	if got, err := DefaultDir(); err != nil || got != filepath.Join(xdg, "go-zfs-rebalance") {
		t.Errorf("with XDG_STATE_HOME: DefaultDir() = %q, %v", got, err)
	}

	// If root's home can't be found, $HOME is the fallback.
	t.Setenv("XDG_STATE_HOME", "")
	rootHome = func() (string, error) { return "", errors.New("no such user") }
	if got, err := DefaultDir(); err != nil || got != filepath.Join(userHome, ".local/state/go-zfs-rebalance") {
		t.Errorf("without root's home: DefaultDir() = %q, %v", got, err)
	}
}

func TestLookupRootHome(t *testing.T) {
	home, err := lookupRootHome()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(home) {
		t.Errorf("root's home %q isn't a full path", home)
	}
	t.Logf("root's home is %s", home)
}

func TestDefaultPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)

	a1, err := DefaultPath("/tank/a")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := DefaultPath("/tank/a/")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DefaultPath("/tank/b")
	if err != nil {
		t.Fatal(err)
	}

	if a1 != a2 {
		t.Errorf("DefaultPath is not stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("different roots share a state file: %q", a1)
	}
	sum := sha256.Sum256([]byte("/tank/a"))
	if want := filepath.Join(xdg, "go-zfs-rebalance", hex.EncodeToString(sum[:])[:12]+".db"); a1 != want {
		t.Errorf("DefaultPath = %q, want %q", a1, want)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}\.db$`).MatchString(filepath.Base(b)) {
		t.Errorf("unexpected file name %q", filepath.Base(b))
	}

	other := t.TempDir()
	t.Setenv("XDG_STATE_HOME", other)
	moved, err := DefaultPath("/tank/a")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(moved) != filepath.Join(other, "go-zfs-rebalance") {
		t.Errorf("DefaultPath ignores XDG_STATE_HOME: %q", moved)
	}
}
