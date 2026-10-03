//go:build linux || darwin

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/database"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

func TestPassesCapAcrossRuns(t *testing.T) {
	names := []string{"a", "b", "sub/c"}
	for _, passes := range []int{1, 2} {
		t.Run(fmt.Sprintf("passes %d", passes), func(t *testing.T) {
			root := tempRoot(t)
			for _, n := range names {
				writeFile(t, root, n, []byte(n))
			}
			r, _ := newRebalancer(t, Config{Root: root, Passes: passes})

			for run := 1; run <= 3; run++ {
				before := snapshot(t, root)
				p, s := scanAndExecute(t, r)
				after := snapshot(t, root)
				if run <= passes {
					if s.Rebalanced != 3 || p.TotalFiles != 3 {
						t.Errorf("run %d: summary %+v", run, s)
					}
					checkRewritten(t, before, after, names...)
				} else {
					if s.Rebalanced != 0 || p.TotalFiles != 0 || s.Skipped[SkipAlreadyDone] != 3 {
						t.Errorf("run %d: summary %+v", run, s)
					}
					checkUntouched(t, before, after, names...)
				}
			}
		})
	}
}

func TestResumeWithDatabase(t *testing.T) {
	// The state file lives inside the folder being rebalanced, reached through an unresolved path
	// (on macOS the temp folder is behind the /var symlink), so it must be recognised and left alone.
	unresolved := t.TempDir()
	root, err := filepath.EvalSymlinks(unresolved)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"a", "b", "sub/c"}
	for _, n := range names {
		writeFile(t, root, n, []byte(n))
	}
	dbPath := filepath.Join(unresolved, "state", "progress.db")

	db, _, err := database.Open(dbPath, root, false)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newRebalancer(t, Config{Root: root, State: db, Exclude: db.Files()})
	before := snapshot(t, root)
	p, s := scanAndExecute(t, r)
	if got := itemNames(p); fmt.Sprint(got) != fmt.Sprint(names) || s.Rebalanced != 3 {
		t.Errorf("first run planned %q, summary %+v", got, s)
	}
	after := snapshot(t, root)
	checkRewritten(t, before, after, names...)
	if before["state/progress.db"].id != after["state/progress.db"].id {
		t.Error("the progress file inside the folder was rewritten")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, info, err := database.Open(dbPath, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !info.Resumed || info.PreviousEntries != 3 {
		t.Errorf("open info = %+v", info)
	}
	r2, _ := newRebalancer(t, Config{Root: root, State: db, Exclude: db.Files()})
	p, s = scanAndExecute(t, r2)
	if p.TotalFiles != 0 || s.Rebalanced != 0 || s.Skipped[SkipAlreadyDone] != 3 {
		t.Errorf("resumed run: plan %q, summary %+v", itemNames(p), s)
	}
	checkUntouched(t, after, snapshot(t, root), names...)
}

func TestStopMidRunFinishesCleanly(t *testing.T) {
	root := tempRoot(t)
	for i := range 30 {
		writeFile(t, root, fmt.Sprintf("f%02d", i), randomBytes(64<<10))
	}
	before := snapshot(t, root)
	state := newHookState()
	r, _ := newRebalancer(t, Config{Root: root, State: state})
	state.onFirst = r.Stop

	_, s := scanAndExecute(t, r)
	if s.Stopped != Interrupted || s.Rebalanced < 1 || s.Remaining < 1 || s.Rebalanced+s.Remaining != 30 {
		t.Errorf("summary = %+v", s)
	}
	checkSameContents(t, before, snapshot(t, root))
	checkNoTemps(t, root)
}

func TestCancelMidRunLeavesOriginals(t *testing.T) {
	root := tempRoot(t)
	for i := range 6 {
		writeFile(t, root, fmt.Sprintf("big%d", i), randomBytes(4<<20))
	}
	before := snapshot(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := newHookState()
	state.onFirst = cancel
	r, _ := newRebalancer(t, Config{Root: root, State: state})

	s := execute(t, ctx, r, scan(t, r))
	if s.Stopped != Interrupted || s.Remaining < 1 || s.Failed != 0 {
		t.Errorf("summary = %+v", s)
	}
	checkSameContents(t, before, snapshot(t, root))
	checkNoTemps(t, root)
}

func TestMissingFile(t *testing.T) {
	for _, halt := range []bool{false, true} {
		t.Run(fmt.Sprintf("halt %v", halt), func(t *testing.T) {
			root := tempRoot(t)
			for i := range 5 {
				writeFile(t, root, fmt.Sprintf("f%d", i), []byte("data"))
			}
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1, HaltOnMissing: halt})
			p := scan(t, r)
			gone := p.Items[0].Names[0]
			if err := os.Remove(filepath.Join(root, gone)); err != nil {
				t.Fatal(err)
			}

			s := execute(t, context.Background(), r, p)
			if s.Skipped[SkipMissing] != 1 || s.Failed != 0 {
				t.Errorf("summary = %+v", s)
			}
			if halt && (s.Stopped != MissingFile || s.Rebalanced != 0 || s.Remaining != 4) {
				t.Errorf("halting summary = %+v", s)
			}
			if !halt && (s.Stopped != None || s.Rebalanced != 4 || s.Remaining != 0) {
				t.Errorf("summary = %+v", s)
			}
			if sk := logs.op("skipped"); len(sk) != 1 || sk[0].data["path"] != gone {
				t.Errorf("skipped log lines = %+v", sk)
			}
		})
	}
}

func TestFilesChangedAfterScanAreSkipped(t *testing.T) {
	root := tempRoot(t)
	for _, n := range []string{"becomes-fifo", "gets-a-link", "ok"} {
		writeFile(t, root, n, []byte(n))
	}
	r, _ := newRebalancer(t, Config{Root: root})
	p := scan(t, r)

	fifo := filepath.Join(root, "becomes-fifo")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "gets-a-link"), filepath.Join(root, "new-link")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	s := execute(t, context.Background(), r, p)
	if s.Rebalanced != 1 || s.Skipped[SkipNotRegular] != 1 || s.Skipped[SkipLinksChanged] != 1 || s.Failed != 0 {
		t.Errorf("summary = %+v", s)
	}
	checkUntouched(t, before, snapshot(t, root), "gets-a-link", "new-link")
}

func TestUnwritableFolderCountsAsFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to any folder")
	}
	root := tempRoot(t)
	writeFile(t, root, "locked/f", []byte("x"))
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root})

	_, s := scanAndExecute(t, r)
	if s.Failed != 1 || s.Rebalanced != 0 || s.Stopped != None {
		t.Errorf("summary = %+v", s)
	}
	f := logs.op("failed")
	if len(f) != 1 || f[0].level != logrus.ErrorLevel || f[0].data["path"] != "locked/f" ||
		!strings.Contains(fmt.Sprint(f[0].data["reason"]), "permission denied") {
		t.Errorf("failed log lines = %+v", f)
	}
	checkUntouched(t, before, snapshot(t, root), "locked/f")
}

func TestOutcomeOfEachError(t *testing.T) {
	tests := []struct {
		err       error
		skip      SkipReason
		failed    int
		level     logrus.Level
		stopped   StopReason
		remaining int
	}{
		{err: fileutil.ErrOwnership, skip: SkipOwner, level: logrus.WarnLevel},
		{err: fileutil.ErrModified, skip: SkipChanged, level: logrus.InfoLevel},
		{err: fileutil.ErrLinkMismatch, skip: SkipLinksChanged, level: logrus.InfoLevel},
		{err: fileutil.ErrNotRegular, skip: SkipNotRegular, level: logrus.InfoLevel},
		{err: fileutil.ErrChecksum, failed: 1, level: logrus.ErrorLevel},
		{err: fileutil.ErrMetadata, skip: SkipMetadata, level: logrus.WarnLevel},
		{err: fileutil.ErrNoSpace, failed: 1, level: logrus.ErrorLevel, stopped: NoSpace, remaining: 2},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			root := tempRoot(t)
			for _, n := range []string{"a", "b", "c"} {
				writeFile(t, root, n, []byte(n))
			}
			err := fmt.Errorf("%w (detail)", tt.err)
			stubReplace(t, "a", err)
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1})

			_, s := scanAndExecute(t, r)
			wantRebalanced := 2 - tt.remaining
			if s.Failed != tt.failed || s.Stopped != tt.stopped || s.Remaining != tt.remaining || s.Rebalanced != wantRebalanced {
				t.Errorf("summary = %+v", s)
			}
			if tt.skip != "" && s.Skipped[tt.skip] != 1 {
				t.Errorf("skipped = %v, want 1 %q", s.Skipped, tt.skip)
			}
			lines := append(logs.op("skipped"), logs.op("failed")...)
			if len(lines) != 1 || lines[0].level != tt.level || lines[0].data["path"] != "a" || lines[0].data["reason"] != err.Error() {
				t.Errorf("log lines = %+v", lines)
			}
		})
	}
}

func TestSmallFilesLoggedAtDebug(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "small", []byte("tiny"))
	writeFile(t, root, "large", randomBytes(2<<20))
	r, logs := newRebalancer(t, Config{Root: root, SizeThresholdMB: 1})
	scanAndExecute(t, r)

	levels := map[string]logrus.Level{}
	for _, e := range logs.op("rebalanced") {
		levels[e.data["path"].(string)] = e.level
	}
	if levels["small"] != logrus.DebugLevel || levels["large"] != logrus.InfoLevel {
		t.Errorf("levels = %v", levels)
	}
}

// failingState loads fine but can't save.
type failingState struct{}

func (failingState) Counts() (map[string]int, error) { return map[string]int{}, nil }
func (failingState) Increment(string) (int, error)   { return 0, errors.New("disk I/O error") }

func TestStateSaveFailureIsOnlyAWarning(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "a", []byte("a"))
	writeFile(t, root, "b", []byte("b"))
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root, State: failingState{}})

	_, s := scanAndExecute(t, r)
	if s.Rebalanced != 2 || s.Failed != 0 {
		t.Errorf("summary = %+v", s)
	}
	checkRewritten(t, before, snapshot(t, root), "a", "b")
	if w := logs.op("warning"); len(w) != 2 || w[0].level != logrus.WarnLevel {
		t.Errorf("warnings = %+v", w)
	}
}

func TestStateLoadFailureStopsScan(t *testing.T) {
	r, _ := newRebalancer(t, Config{Root: tempRoot(t), State: brokenState{}})
	if _, err := r.Scan(context.Background()); err == nil || !strings.Contains(err.Error(), "saved progress") {
		t.Fatalf("Scan = %v", err)
	}
}

type brokenState struct{ failingState }

func (brokenState) Counts() (map[string]int, error) { return nil, errors.New("file is not a database") }

func TestFolderTimesRestored(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "a", []byte("a"))
	writeFile(t, root, "d1/b", []byte("b"))
	writeFile(t, root, "d1/d2/c", []byte("c"))
	writeFile(t, root, "d3/"+filepath.Base(staleTemp), []byte("half a copy"))
	if err := os.Mkdir(filepath.Join(root, "untouched"), 0o755); err != nil {
		t.Fatal(err)
	}

	type times struct{ atime, mtime time.Time }
	dirs := []string{".", "d1", "d1/d2", "d3", "untouched"}
	want := make(map[string]times)
	for i, dir := range dirs {
		want[dir] = times{
			atime: time.Date(2001, 2, 3, 4, 5, 6, 700+i, time.UTC),
			mtime: time.Date(2000, 1, 2, 3, 4, 5, 600+i, time.UTC),
		}
		if err := os.Chtimes(filepath.Join(root, dir), want[dir].atime, want[dir].mtime); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := newRebalancer(t, Config{Root: root, Concurrency: 4, Cleanup: true})

	_, s := scanAndExecute(t, r)
	if s.Rebalanced != 3 {
		t.Fatalf("summary = %+v", s)
	}
	for _, dir := range dirs {
		got := lstatInfo(t, filepath.Join(root, dir))
		if !got.Mtime.Equal(want[dir].mtime) {
			t.Errorf("%s: modified time %v, want %v", dir, got.Mtime, want[dir].mtime)
		}
		// Reading a folder may update its access time, so only folders the run changed have
		// theirs put back.
		if dir != "untouched" && !got.Atime.Equal(want[dir].atime) {
			t.Errorf("%s: access time %v, want %v", dir, got.Atime, want[dir].atime)
		}
	}
}

func TestFolderTimesRestoredAfterStop(t *testing.T) {
	root := tempRoot(t)
	for i := range 10 {
		writeFile(t, root, fmt.Sprintf("d/f%d", i), []byte("data"))
	}
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "d"), old, old); err != nil {
		t.Fatal(err)
	}
	state := newHookState()
	r, _ := newRebalancer(t, Config{Root: root, Concurrency: 1, State: state})
	state.onFirst = r.Stop

	_, s := scanAndExecute(t, r)
	if s.Stopped != Interrupted || s.Remaining == 0 {
		t.Fatalf("summary = %+v", s)
	}
	if got := lstatInfo(t, filepath.Join(root, "d")).Mtime; !got.Equal(old) {
		t.Errorf("modified time %v, want %v", got, old)
	}
}

// TestLargeTree replaces the old large-file integration test: about 64 MiB of mixed files,
// including a sparse-looking one, rewritten several at a time.
func TestLargeTree(t *testing.T) {
	if testing.Short() {
		t.Skip("writes about 64 MiB")
	}
	root := tempRoot(t)
	sizes := []int{0, 1, 1 << 10, 10 << 10, 100 << 10, 1 << 20, 3<<20 + 7, 10 << 20, 10 << 20}
	for i, size := range sizes {
		writeFile(t, root, fmt.Sprintf("set%d/file_%d.dat", i%3, i), randomBytes(size))
	}
	holes := make([]byte, 24<<20)
	copy(holes[5<<20:], randomBytes(1<<20))
	copy(holes[20<<20:], randomBytes(300<<10))
	writeFile(t, root, "holes.img", holes)
	for i := range 40 {
		writeFile(t, root, fmt.Sprintf("small/%02d.txt", i), randomBytes(i*100))
	}
	before := snapshot(t, root)
	r, _ := newRebalancer(t, Config{Root: root, Concurrency: 4, RandomOrder: true})

	p, s := scanAndExecute(t, r)
	if s.Rebalanced != len(before) || s.Failed != 0 || s.Bytes != p.TotalBytes {
		t.Errorf("summary = %+v, %d files, %d bytes planned", s, len(before), p.TotalBytes)
	}
	after := snapshot(t, root)
	for rel := range before {
		checkRewritten(t, before, after, rel)
	}
	checkNoTemps(t, root)
}
