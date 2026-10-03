//go:build linux || darwin

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/database"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
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
	unresolved := testDir(t)
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

// Without root, a file of the user's own in a folder they can't write to is skipped, with a hint
// to use sudo, rather than reported as a failure.
func TestUnwritableFolderIsSkippedWithoutRoot(t *testing.T) {
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
	if s.Skipped[SkipNoPermission] != 1 || s.Failed != 0 || s.Rebalanced != 0 || s.Stopped != None {
		t.Errorf("summary = %+v", s)
	}
	sk := logs.op("skipped")
	if len(sk) != 1 || sk[0].level != logrus.InfoLevel || sk[0].data["path"] != "locked/f" || sk[0].data["reason"] != reasonNoPermission {
		t.Errorf("skipped log lines = %+v", sk)
	}
	if f := logs.op("failed"); len(f) != 0 {
		t.Errorf("failed log lines = %+v", f)
	}
	checkUntouched(t, before, snapshot(t, root), "locked/f")
}

// Being refused permission to open a file, or to add or swap a copy in its folder, is something
// only root can get past: without root the file is skipped, with root it is a failure. Errors of
// fileutil's own kinds keep their meaning even when a permission error caused them.
func TestPermissionErrorsDependOnRoot(t *testing.T) {
	denied := fmt.Errorf("couldn't open the file (permission denied): %w",
		&os.PathError{Op: "open", Path: "a", Err: syscall.EACCES})
	notPermitted := fmt.Errorf("couldn't swap in the new copy (operation not permitted): %w",
		&os.LinkError{Op: "rename", Old: ".zfs-rebalance.0123456789ab.tmp", New: "a", Err: syscall.EPERM})
	metadata := fmt.Errorf("%w (timestamps: operation not permitted): %w", fileutil.ErrMetadata, syscall.EPERM)
	ownership := fmt.Errorf("%w: %w", fileutil.ErrOwnership, syscall.EPERM)

	tests := []struct {
		name   string
		err    error
		asRoot bool
		skip   SkipReason // "" means it fails
		reason string
	}{
		{"open refused", denied, false, SkipNoPermission, reasonNoPermission},
		{"rename refused", notPermitted, false, SkipNoPermission, reasonNoPermission},
		{"open refused to root", denied, true, "", denied.Error()},
		{"rename refused to root", notPermitted, true, "", notPermitted.Error()},
		{"metadata", metadata, false, SkipMetadata, metadata.Error()},
		{"ownership", ownership, false, SkipOwnerNotKept, ownership.Error()},
		{"ownership as root", ownership, true, SkipOwnerNotKept, ownership.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tempRoot(t)
			for _, n := range []string{"a", "b"} {
				writeFile(t, root, n, []byte(n))
			}
			stubReplace(t, "a", tt.err)
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1})
			r.as = ownerOf(t, filepath.Join(root, "a"))
			if tt.asRoot {
				r.as = account{root: true}
			}

			_, s := scanAndExecute(t, r)
			if s.Rebalanced != 1 || s.Stopped != None {
				t.Errorf("summary = %+v", s)
			}
			op := "skipped"
			if tt.skip == "" {
				op = "failed"
				if s.Failed != 1 {
					t.Errorf("failed = %d, want 1", s.Failed)
				}
			} else if s.Skipped[tt.skip] != 1 || s.Failed != 0 {
				t.Errorf("skipped %v, failed %d; want 1 %q", s.Skipped, s.Failed, tt.skip)
			}
			if l := logs.op(op); len(l) != 1 || l[0].data["path"] != "a" || l[0].data["reason"] != tt.reason {
				t.Errorf("%s log lines = %+v", op, l)
			}
		})
	}
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
		{err: fileutil.ErrOwnership, skip: SkipOwnerNotKept, level: logrus.WarnLevel},
		{err: fileutil.ErrModified, skip: SkipChanged, level: logrus.InfoLevel},
		{err: fileutil.ErrLinkMismatch, skip: SkipLinksChanged, level: logrus.InfoLevel},
		{err: fileutil.ErrNotRegular, skip: SkipNotRegular, level: logrus.InfoLevel},
		{err: fileutil.ErrChecksum, failed: 1, level: logrus.ErrorLevel},
		{err: fileutil.ErrMetadata, skip: SkipMetadata, level: logrus.WarnLevel},
		{err: fileutil.ErrProjectID, skip: SkipProjectID, level: logrus.InfoLevel},
		{err: fileutil.ErrImmutable, skip: SkipImmutable, level: logrus.InfoLevel},
		{err: fileutil.ErrUndeletable, skip: SkipUndeletable, level: logrus.InfoLevel},
		{err: fileutil.ErrBusy, skip: SkipBusy, level: logrus.InfoLevel},
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
			if tt.skip != "" && (s.Skipped[tt.skip] != 1 || len(s.Skipped) != 1 || s.RunSkipped[tt.skip] != 1) {
				t.Errorf("skipped = %v (in the run %v), want only 1 %q", s.Skipped, s.RunSkipped, tt.skip)
			}
			lines := append(logs.op("skipped"), logs.op("failed")...)
			if len(lines) != 1 || lines[0].level != tt.level || lines[0].data["path"] != "a" || lines[0].data["reason"] != err.Error() {
				t.Errorf("log lines = %+v", lines)
			}
		})
	}
}

// A quota error while there is still plenty of room on the filesystem can only be a per-user or
// per-group quota, so that file is skipped and the run goes on. Otherwise the dataset or pool is
// full and the run stops.
func TestQuotaOrFullPool(t *testing.T) {
	quota := fmt.Errorf("%w (its owner or group is over their quota): %w", fileutil.ErrNoSpace,
		&os.PathError{Op: "chown", Path: "x", Err: syscall.EDQUOT})
	full := fmt.Errorf("%w: %w", fileutil.ErrNoSpace, &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})
	enough := func(need uint64) (uint64, error) { return need + 1, nil }

	tests := []struct {
		name     string
		err      error
		free     func(need uint64) (uint64, error)
		wantSkip bool
	}{
		{"owner over quota", quota, enough, true},
		{"quota with no room left", quota, func(need uint64) (uint64, error) { return need, nil }, false},
		{"room unknown", quota, func(uint64) (uint64, error) { return 0, errors.New("statfs failed") }, false},
		{"pool full", full, enough, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tempRoot(t)
			writeFile(t, root, "sub/a", randomBytes(200<<10))
			writeFile(t, root, "b", []byte("b"))
			writeFile(t, root, "c", []byte("c"))
			stubReplace(t, "sub/a", tt.err)
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1})
			p := scan(t, r)
			var alloc int64 = -1
			for _, it := range p.Items {
				if it.Names[0] == "sub/a" {
					alloc = it.Allocated
				}
			}
			if alloc < 0 {
				t.Fatalf("sub/a isn't in the plan: %+v", p.Items)
			}
			var asked []string
			t.Cleanup(func() { freeBytes = folderFreeBytes })
			freeBytes = func(_ *os.Root, dir string) (uint64, error) {
				asked = append(asked, dir)
				return tt.free(2*uint64(alloc) + quotaMargin)
			}

			s := execute(t, context.Background(), r, p)
			if want := map[bool]string{true: "[sub]", false: "[]"}[tt.err == quota]; fmt.Sprint(asked) != want {
				t.Errorf("free space checked in %q, want %s", asked, want)
			}
			if tt.wantSkip {
				if s.Skipped[SkipQuota] != 1 || s.Failed != 0 || s.Stopped != None || s.Rebalanced != 2 {
					t.Errorf("summary = %+v", s)
				}
				if sk := logs.op("skipped"); len(sk) != 1 || sk[0].level != logrus.WarnLevel || sk[0].data["reason"] != reasonOverQuota {
					t.Errorf("skipped log lines = %+v", sk)
				}
				return
			}
			if s.Failed != 1 || s.Stopped != NoSpace || s.Skipped[SkipQuota] != 0 {
				t.Errorf("summary = %+v", s)
			}
		})
	}
}

func TestFolderFreeBytes(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "sub/f", []byte("x"))
	r, _ := newRebalancer(t, Config{Root: root})
	for _, dir := range []string{".", "sub"} {
		if free, err := folderFreeBytes(r.root, dir); err != nil || free == 0 {
			t.Errorf("%s: free space %d, %v", dir, free, err)
		}
	}
	if _, err := folderFreeBytes(r.root, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing folder: %v", err)
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

// callbackState is an in-memory StateStore that runs a function when a given file is recorded as
// done, which is after its rewrite has finished and before its folder's times are put back.
type callbackState struct {
	memoryState
	on map[string]func()
}

func (c *callbackState) Increment(rel string) (int, error) {
	n, err := c.memoryState.Increment(rel)
	if f := c.on[rel]; f != nil {
		f()
	}
	return n, err
}

// A folder another program changes while the run is using it keeps its new time, so tools that
// look at folder times (media servers, backups) still notice the change. Here the change is a new
// file, as a download would add, at each point the run can tell it apart from its own changes.
func TestFolderTimesKeptWhenAnotherProgramChangesTheFolder(t *testing.T) {
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, when := range []string{"after the scan", "between two of the run's files", "after the run's last file"} {
		t.Run(when, func(t *testing.T) {
			root := tempRoot(t)
			for _, rel := range []string{"media/a.mkv", "media/b.mkv", "other/c.mkv"} {
				writeFile(t, root, rel, randomBytes(32<<10))
			}
			for _, dir := range []string{"media", "other"} {
				if err := os.Chtimes(filepath.Join(root, dir), old, old); err != nil {
					t.Fatal(err)
				}
			}
			// A real download takes longer than a tick of the clock Linux stamps files with, so the
			// folder's times change too; TestStampSame covers a change within the same tick.
			download := func() {
				time.Sleep(20 * time.Millisecond)
				writeFile(t, root, "media/new-episode.mkv", []byte("new"))
			}
			state := &callbackState{memoryState: memoryState{counts: map[string]int{}}, on: map[string]func(){}}
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1, State: state})
			p := scan(t, r)
			switch when {
			case "after the scan":
				download()
			case "between two of the run's files":
				state.on["media/a.mkv"] = download
			default:
				state.on["media/b.mkv"] = download
			}

			s := execute(t, context.Background(), r, p)
			if s.Rebalanced != 3 || s.FolderTimesNotRestored != 0 {
				t.Errorf("summary = %+v", s)
			}
			if got := lstatInfo(t, filepath.Join(root, "media")).Mtime; got.Equal(old) {
				t.Error("media's modified time was put back, hiding the new file from tools that look at it")
			}
			if got := lstatInfo(t, filepath.Join(root, "other")).Mtime; !got.Equal(old) {
				t.Errorf("other's modified time is %v, want it put back to %v", got, old)
			}
			var left []logEntry
			for _, e := range logs.about("media") {
				if strings.Contains(e.msg, "another program changed") {
					left = append(left, e)
				}
			}
			if len(left) != 1 || left[0].level != logrus.DebugLevel {
				t.Errorf("log lines about leaving media's times = %+v", left)
			}
			if w := logs.op("warning"); len(w) != 0 {
				t.Errorf("warnings = %+v", w)
			}
		})
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

// A folder's stamp must change with any entry added or removed, even one that leaves its times as
// they were because it came within the same tick of the clock as the run's own change.
func TestStampSame(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 4_000_000, time.UTC)
	base := stamp{id: fileutil.FileID{Dev: 1, Ino: 2}, mtime: t0, ctime: t0, size: 5, nlink: 2}
	tests := []struct {
		name   string
		change func(*stamp)
		same   bool
	}{
		{"unchanged", func(*stamp) {}, true},
		{"replaced", func(s *stamp) { s.id.Ino++ }, false},
		{"modified later", func(s *stamp) { s.mtime = s.mtime.Add(time.Millisecond) }, false},
		{"attributes changed later", func(s *stamp) { s.ctime = s.ctime.Add(time.Nanosecond) }, false},
		{"file added in the same tick", func(s *stamp) { s.size++ }, false},
		{"file removed in the same tick", func(s *stamp) { s.size-- }, false},
		{"subfolder added in the same tick", func(s *stamp) { s.nlink++ }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := base
			tt.change(&other)
			if got := base.same(other); got != tt.same {
				t.Errorf("same = %v, want %v", got, tt.same)
			}
		})
	}
}

// After a run stops because the dataset is full or at its quota, ZFS frees the abandoned copies'
// space a little later, and until then even setting a folder's times fails. The run waits a
// little for that before giving up, but never for long in all.
func TestFolderTimesWaitForSpace(t *testing.T) {
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name     string
		err      error
		failures int // how many attempts fail before the space is freed; -1 if it never is
		calls    map[string]int
		warned   int
	}{
		{"quota frees up", syscall.EDQUOT, 2, map[string]int{"a": 3, "b": 1}, 0},
		{"pool frees up", syscall.ENOSPC, 1, map[string]int{"a": 2, "b": 1}, 0},
		{"never frees up", syscall.EDQUOT, -1, map[string]int{"a": 4, "b": 2}, 2},
		{"refused", syscall.EPERM, -1, map[string]int{"a": 1, "b": 1}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tempRoot(t)
			for _, rel := range []string{"a/f", "b/g"} {
				writeFile(t, root, rel, []byte(rel))
			}
			for _, dir := range []string{"a", "b"} {
				if err := os.Chtimes(filepath.Join(root, dir), old, old); err != nil {
					t.Fatal(err)
				}
			}
			delays, budget := noSpaceDelays, noSpaceBudget
			t.Cleanup(func() { noSpaceDelays, noSpaceBudget, setFolderTimes = delays, budget, fileutil.SetTimes })
			noSpaceDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
			noSpaceBudget = 4 * time.Millisecond
			calls := map[string]int{}
			failed := 0
			setFolderTimes = func(d *os.File, atime, mtime time.Time) error {
				dir := filepath.Base(d.Name())
				calls[dir]++
				if tt.failures < 0 || failed < tt.failures {
					failed++
					return &os.PathError{Op: "utimensat", Path: dir, Err: tt.err}
				}
				return fileutil.SetTimes(d, atime, mtime)
			}
			r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1})

			_, s := scanAndExecute(t, r)
			if s.Rebalanced != 2 || s.FolderTimesNotRestored != tt.warned {
				t.Errorf("summary = %+v", s)
			}
			if fmt.Sprint(calls) != fmt.Sprint(tt.calls) {
				t.Errorf("attempts per folder = %v, want %v", calls, tt.calls)
			}
			if w := logs.op("warning"); len(w) != tt.warned {
				t.Errorf("warnings = %+v", w)
			}
			for _, dir := range []string{"a", "b"} {
				if restored := lstatInfo(t, filepath.Join(root, dir)).Mtime.Equal(old); restored != (tt.warned == 0) {
					t.Errorf("%s: modified time put back = %v", dir, restored)
				}
			}
		})
	}
}

// A folder's times are only ever set on the folder the run worked in, through the folder it opened
// and checked; never on another folder that a symlink in its place leads to.
func TestFolderTimesNeverSetThroughASymlink(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "media/a.mkv", []byte("a"))
	if err := os.Mkdir(filepath.Join(root, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, dir := range []string{"media", "elsewhere"} {
		if err := os.Chtimes(filepath.Join(root, dir), old, old); err != nil {
			t.Fatal(err)
		}
	}
	state := &callbackState{memoryState: memoryState{counts: map[string]int{}}, on: map[string]func(){}}
	state.on["media/a.mkv"] = func() {
		if err := os.Rename(filepath.Join(root, "media"), filepath.Join(root, "media-moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", filepath.Join(root, "media")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, "elsewhere"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	r, logs := newRebalancer(t, Config{Root: root, Concurrency: 1, State: state})

	_, s := scanAndExecute(t, r)
	if s.Rebalanced != 1 || s.FolderTimesNotRestored != 0 {
		t.Errorf("summary = %+v", s)
	}
	got := lstatInfo(t, filepath.Join(root, "elsewhere"))
	if !got.Mtime.Equal(old) || !got.Atime.Equal(old) {
		t.Errorf("the folder behind the symlink got times %v, %v", got.Atime, got.Mtime)
	}
	if w := logs.op("warning"); len(w) != 0 {
		t.Errorf("warnings = %+v", w)
	}
}

// A file that another program or run has locked is left alone, and the rest of the run goes on.
func TestLockedFileIsSkippedAsBusy(t *testing.T) {
	root := tempRoot(t)
	for _, n := range []string{"busy", "free"} {
		writeFile(t, root, n, randomBytes(64<<10))
	}
	f, err := os.Open(filepath.Join(root, "busy"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root})

	_, s := scanAndExecute(t, r)
	if s.Rebalanced != 1 || s.Skipped[SkipBusy] != 1 || s.Failed != 0 {
		t.Errorf("summary = %+v", s)
	}
	after := snapshot(t, root)
	checkUntouched(t, before, after, "busy")
	checkRewritten(t, before, after, "free")
	sk := logs.op("skipped")
	if len(sk) != 1 || sk[0].level != logrus.InfoLevel || sk[0].data["reason"] != fileutil.ErrBusy.Error() {
		t.Errorf("skipped log lines = %+v", sk)
	}
}

// A resumed run that finds only files skipped again for reasons that last says so plainly, and
// still tries them, in case the reason has been dealt with.
func TestResumeWithOnlyLastingSkipsSaysNothingNew(t *testing.T) {
	root := tempRoot(t)
	for _, n := range []string{"a", "b", "locked", "projdir/own-id"} {
		writeFile(t, root, n, []byte(n))
	}
	tried := map[string]int{}
	t.Cleanup(func() { replace = fileutil.ReplaceGroup })
	replace = func(ctx context.Context, root *os.Root, rels []string, opts fileutil.Options) (fileutil.Result, error) {
		tried[rels[0]]++
		switch rels[0] {
		case "locked":
			return fileutil.Result{}, fmt.Errorf("%w (detail)", fileutil.ErrImmutable)
		case "projdir/own-id":
			return fileutil.Result{}, fmt.Errorf("%w (detail)", fileutil.ErrProjectID)
		}
		return fileutil.ReplaceGroup(ctx, root, rels, opts)
	}
	state := &memoryState{counts: map[string]int{}}
	r, _ := newRebalancer(t, Config{Root: root, Concurrency: 1, State: state})

	_, first := scanAndExecute(t, r)
	if first.Rebalanced != 2 || first.NothingNew() != "" {
		t.Errorf("first run: %+v, %q", first, first.NothingNew())
	}
	r2, _ := newRebalancer(t, Config{Root: root, Concurrency: 1, State: state})
	_, again := scanAndExecute(t, r2)
	want := "Nothing new to rebalance: the 2 remaining files were skipped again " +
		"(1 file marked immutable or append-only, 1 file whose project ID differs from its folder's)."
	if again.Total != 2 || again.Rebalanced != 0 || again.NothingNew() != want {
		t.Errorf("second run: %+v, %q", again, again.NothingNew())
	}
	if tried["locked"] != 2 || tried["projdir/own-id"] != 2 {
		t.Errorf("tries = %v, want both skipped files tried on each run", tried)
	}
}
