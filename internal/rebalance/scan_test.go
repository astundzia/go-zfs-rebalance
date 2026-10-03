//go:build linux || darwin

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const staleTemp = "dir/sub/.zfs-rebalance.0123456789ab.tmp"

// buildTree makes a folder with every kind of entry the walk has to handle, plus a file outside
// it that a symlink points to. It returns the names that should be rewritten.
func buildTree(t *testing.T) (root, outside string, rewritten []string) {
	t.Helper()
	root = tempRoot(t)
	outside = filepath.Join(tempRoot(t), "outside.txt")
	if err := os.WriteFile(outside, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	rewritten = []string{
		"a.txt",
		"empty",
		"dir/sub/deep.bin",
		"dir/name with spaces.txt",
		`dir/odd at name: "x".txt`,
		"notes",
		"notes.balance",                       // a user's file that happens to end in .balance
		"dir/.zfs-rebalance.ABCDEF012345.tmp", // looks like a temp, but uppercase
		"dir/.zfs-rebalance.0123.tmp",         // looks like a temp, but too short
	}
	for _, rel := range rewritten {
		data := []byte("contents of " + rel)
		switch rel {
		case "empty":
			data = nil
		case "dir/sub/deep.bin":
			data = randomBytes(300 << 10)
		}
		writeFile(t, root, rel, data)
	}
	writeFile(t, root, "x.balance", []byte("maybe the only copy"))
	writeFile(t, root, staleTemp, []byte("half a copy"))
	writeFile(t, root, ".zfs/snapshot/s1/a.txt", []byte("snapshot"))
	writeFile(t, root, "dir/.zfs/inner", []byte("snapshot"))
	if err := os.Symlink(outside, filepath.Join(root, "link-to-outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "dir/fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside, rewritten
}

func TestRewritesEveryFileOnce(t *testing.T) {
	for _, concurrency := range []int{1, 8} {
		t.Run(fmt.Sprintf("concurrency %d", concurrency), func(t *testing.T) {
			root, outside, want := buildTree(t)
			before := snapshot(t, root)
			outsideBefore := lstatInfo(t, outside)
			state := newHookState()
			r, logs := newRebalancer(t, Config{
				Root: root, Concurrency: concurrency, Cleanup: true, RandomOrder: true, State: state,
			})

			p, s := scanAndExecute(t, r)

			slices.Sort(want)
			if got := itemNames(p); !slices.Equal(got, want) {
				t.Errorf("planned files:\n got %q\nwant %q", got, want)
			}
			if !slices.Equal(p.StaleTemps, []string{staleTemp}) {
				t.Errorf("stale temps = %q", p.StaleTemps)
			}
			if !slices.Equal(p.LegacyBalance, []string{"notes.balance", "x.balance"}) {
				t.Errorf("legacy .balance files = %q", p.LegacyBalance)
			}
			if !slices.Equal(p.OrphanBalance, []string{"x.balance"}) {
				t.Errorf("orphan .balance files = %q", p.OrphanBalance)
			}
			if p.TotalFiles != len(want) || p.LargestFile != 300<<10 {
				t.Errorf("plan totals: %d files, largest %d", p.TotalFiles, p.LargestFile)
			}
			wantSkipped := map[SkipReason]int{SkipOrphanBalance: 1}
			if s.Rebalanced != len(want) || s.Failed != 0 || s.Remaining != 0 || s.Stopped != None ||
				fmt.Sprint(s.Skipped) != fmt.Sprint(wantSkipped) {
				t.Errorf("summary = %+v", s)
			}

			after := snapshot(t, root)
			checkRewritten(t, before, after, want...)
			checkUntouched(t, before, after, "x.balance", ".zfs/snapshot/s1/a.txt", "dir/.zfs/inner")
			for _, rel := range want {
				if n := state.count(rel); n != 1 {
					t.Errorf("%s: progress count %d, want 1", rel, n)
				}
			}
			if _, ok := after[staleTemp]; ok {
				t.Error("the stale temporary file wasn't removed")
			}
			checkNoTemps(t, root)

			if lstatInfo(t, outside) != outsideBefore {
				t.Error("the file behind a symlink was changed")
			}
			if target, err := os.Readlink(filepath.Join(root, "link-to-outside")); err != nil || target != outside {
				t.Errorf("symlink changed: %q, %v", target, err)
			}
			if fi, err := os.Lstat(filepath.Join(root, "dir/fifo")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("FIFO changed: %v, %v", fi, err)
			}

			rebalanced := logs.op("rebalanced")
			if len(rebalanced) != len(want) {
				t.Errorf("%d rebalanced log lines, want %d", len(rebalanced), len(want))
			}
			for _, e := range rebalanced {
				if e.level != logrus.InfoLevel || e.data["size"] == nil || e.data["mbps"] == nil {
					t.Errorf("rebalanced log line = %+v", e)
				}
			}
			if removed := logs.op("removed-temp"); len(removed) != 1 || removed[0].data["path"] != staleTemp {
				t.Errorf("removed-temp log lines = %+v", removed)
			}
			logs.checkNoPathsInMessages(t)
		})
	}
}

func TestStaleTempsKeptWithoutCleanup(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "f", []byte("data"))
	writeFile(t, root, staleTemp, []byte("half a copy"))
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root})

	p, s := scanAndExecute(t, r)
	if !slices.Equal(p.StaleTemps, []string{staleTemp}) || s.Rebalanced != 1 {
		t.Errorf("stale temps %q, summary %+v", p.StaleTemps, s)
	}
	after := snapshot(t, root)
	checkUntouched(t, before, after, staleTemp)
	checkRewritten(t, before, after, "f")
	if n := len(logs.op("removed-temp")); n != 0 {
		t.Errorf("%d removal log lines without cleanup", n)
	}
}

func TestHardlinksSkippedByDefault(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "a", []byte("shared"))
	writeFile(t, root, "c", []byte("alone"))
	if err := os.Link(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	r, _ := newRebalancer(t, Config{Root: root})

	_, s := scanAndExecute(t, r)
	if s.Skipped[SkipHardlinked] != 2 || s.Rebalanced != 1 {
		t.Errorf("summary = %+v", s)
	}
	after := snapshot(t, root)
	checkUntouched(t, before, after, "a", "b")
	checkRewritten(t, before, after, "c")
}

func TestHardlinkGroupInsideRoot(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "one", randomBytes(70<<10))
	writeFile(t, root, "solo", []byte("alone"))
	for _, rel := range []string{"d1/two", "d2/three"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "one"), filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, root)
	state := newHookState()
	r, logs := newRebalancer(t, Config{Root: root, ProcessHardlinks: true, State: state})

	p, s := scanAndExecute(t, r)
	if len(p.Items) != 2 || p.TotalFiles != 4 || p.TotalBytes != 70<<10+5 {
		t.Errorf("plan = %+v", p)
	}
	if s.Rebalanced != 4 || s.Failed != 0 || len(s.Skipped) != 0 {
		t.Errorf("summary = %+v", s)
	}
	// The group's line names all of it, so the user can see that every name was switched.
	wantNames := []string{"d1/two", "d2/three", "one"}
	for _, e := range logs.op("rebalanced") {
		names, isGroup := e.data["names"].([]string)
		switch e.data["path"] {
		case "solo":
			if isGroup {
				t.Errorf("a single file's line lists names: %+v", e)
			}
		case wantNames[0]:
			if !slices.Equal(names, wantNames) || !strings.Contains(e.msg, "3 hardlinked names") {
				t.Errorf("the group's line = %+v, want names %q", e, wantNames)
			}
		default:
			t.Errorf("unexpected rebalanced line %+v", e)
		}
	}
	after := snapshot(t, root)
	checkRewritten(t, before, after, "one", "d1/two", "d2/three", "solo")
	info := lstatInfo(t, filepath.Join(root, "one"))
	for _, rel := range []string{"d1/two", "d2/three"} {
		if after[rel].id != info.ID {
			t.Errorf("%s no longer shares an inode with one", rel)
		}
	}
	if info.Nlink != 3 {
		t.Errorf("link count = %d, want 3", info.Nlink)
	}
	for _, rel := range []string{"one", "d1/two", "d2/three"} {
		if n := state.count(rel); n != 1 {
			t.Errorf("%s: progress count %d, want 1", rel, n)
		}
	}
	checkNoTemps(t, root)
}

func TestHardlinkGroupOutsideRootLeftAlone(t *testing.T) {
	base := tempRoot(t)
	root := filepath.Join(base, "root")
	writeFile(t, root, "inside", []byte("shared"))
	writeFile(t, root, "pair/one", []byte("three names"))
	for _, link := range []string{"root/pair/two", "elsewhere"} {
		if err := os.Link(filepath.Join(root, "pair/one"), filepath.Join(base, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(root, "inside"), filepath.Join(base, "outside")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, base)
	r, logs := newRebalancer(t, Config{Root: root, ProcessHardlinks: true})

	p, s := scanAndExecute(t, r)
	if len(p.Items) != 0 || s.Skipped[SkipHardlinksOutside] != 3 || s.Rebalanced != 0 {
		t.Errorf("plan %+v, summary %+v", p, s)
	}
	// Every name found is logged, so the user can tell which files were left alone.
	want := map[string]string{
		"inside":   "1 of its 2 hardlinked names is outside this folder, so it was left alone",
		"pair/one": "1 of its 3 hardlinked names is outside this folder, so it was left alone",
		"pair/two": "1 of its 3 hardlinked names is outside this folder, so it was left alone",
	}
	skipped := logs.op("skipped")
	if len(skipped) != len(want) {
		t.Errorf("skipped log lines = %+v", skipped)
	}
	for _, e := range skipped {
		if rel, _ := e.data["path"].(string); e.level != logrus.InfoLevel || e.data["reason"] != want[rel] {
			t.Errorf("skipped log line = %+v", e)
		}
	}
	checkUntouched(t, before, snapshot(t, base), "root/pair/one", "root/pair/two", "elsewhere")
	checkUntouched(t, before, snapshot(t, base), "root/inside", "outside")
	if n := lstatInfo(t, filepath.Join(root, "inside")).Nlink; n != 2 {
		t.Errorf("link count = %d, want 2", n)
	}
}

func TestExcludedPathsLeftAlone(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "keep/state.db", []byte("db"))
	writeFile(t, root, "skipdir/inner", []byte("x"))
	writeFile(t, root, "keep/state.db-wal", []byte("wal"))
	writeFile(t, root, "other", []byte("y"))
	before := snapshot(t, root)
	elsewhere := tempRoot(t)
	// One file is named through a symlinked folder, as an unresolved path might be; the other
	// through a symlink to the file itself, which only its identity can match.
	alias := filepath.Join(elsewhere, "alias")
	if err := os.Symlink(filepath.Join(root, "keep"), alias); err != nil {
		t.Fatal(err)
	}
	walLink := filepath.Join(elsewhere, "wal-link")
	if err := os.Symlink(filepath.Join(root, "keep/state.db-wal"), walLink); err != nil {
		t.Fatal(err)
	}
	r, _ := newRebalancer(t, Config{
		Root:    root,
		Exclude: []string{filepath.Join(alias, "state.db"), walLink, filepath.Join(root, "skipdir")},
	})

	p, s := scanAndExecute(t, r)
	if !slices.Equal(itemNames(p), []string{"other"}) || s.Rebalanced != 1 {
		t.Errorf("planned %q, summary %+v", itemNames(p), s)
	}
	checkUntouched(t, before, snapshot(t, root), "keep/state.db", "keep/state.db-wal", "skipdir/inner")
}

// A temporary file whose lock is held belongs to a run that is still going, so the cleanup leaves
// it alone; once the lock is gone it is a leftover like any other.
func TestStaleTempInUseLeftAlone(t *testing.T) {
	root := tempRoot(t)
	const inUse = "dir/.zfs-rebalance.ba9876543210.tmp"
	writeFile(t, root, "f", []byte("data"))
	writeFile(t, root, staleTemp, []byte("half a copy"))
	writeFile(t, root, inUse, []byte("another run's copy"))
	held, err := os.Open(filepath.Join(root, inUse))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil { // as the other run would
		t.Fatal(err)
	}
	r, logs := newRebalancer(t, Config{Root: root, Cleanup: true})

	p, s := scanAndExecute(t, r)
	if want := []string{inUse, staleTemp}; !slices.Equal(p.StaleTemps, want) || s.Rebalanced != 1 {
		t.Errorf("stale temps %q, summary %+v", p.StaleTemps, s)
	}
	after := snapshot(t, root)
	if _, ok := after[staleTemp]; ok {
		t.Error("the leftover temporary file wasn't removed")
	}
	if _, ok := after[inUse]; !ok {
		t.Fatal("the temporary file another run is using was removed")
	}
	if removed := logs.op("removed-temp"); len(removed) != 1 || removed[0].data["path"] != staleTemp {
		t.Errorf("removed-temp log lines = %+v", removed)
	}
	if l := logs.about(inUse); len(l) != 1 || l[0].level != logrus.InfoLevel || !strings.Contains(l[0].msg, "being used by another run") {
		t.Errorf("log lines about the temporary file in use = %+v", l)
	}
	if w := logs.op("warning"); len(w) != 0 {
		t.Errorf("warnings = %+v", w)
	}
	logs.checkNoPathsInMessages(t)

	if err := held.Close(); err != nil { // the other run has ended
		t.Fatal(err)
	}
	r2, logs2 := newRebalancer(t, Config{Root: root, Cleanup: true})
	scanAndExecute(t, r2)
	if removed := logs2.op("removed-temp"); len(removed) != 1 || removed[0].data["path"] != inUse {
		t.Errorf("second run's removed-temp log lines = %+v", removed)
	}
	checkNoTemps(t, root)
}

// ExcludeIDs leaves files alone by identity, whatever they are called, such as the file the
// command's own output is going to.
func TestExcludeIDsLeftAlone(t *testing.T) {
	root := tempRoot(t)
	for _, rel := range []string{"run.log", "keep/inner", "other"} {
		writeFile(t, root, rel, []byte(rel))
	}
	if err := os.Link(filepath.Join(root, "run.log"), filepath.Join(root, "run.log.link")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	ids := []fileutil.FileID{lstatInfo(t, filepath.Join(root, "run.log")).ID, lstatInfo(t, filepath.Join(root, "keep")).ID}
	r, _ := newRebalancer(t, Config{Root: root, ExcludeIDs: ids, ProcessHardlinks: true})

	p, s := scanAndExecute(t, r)
	if !slices.Equal(itemNames(p), []string{"other"}) || s.Rebalanced != 1 || len(s.Skipped) != 0 {
		t.Errorf("planned %q, summary %+v", itemNames(p), s)
	}
	checkUntouched(t, before, snapshot(t, root), "run.log", "run.log.link", "keep/inner")
}

// The plan says how much disk space its largest item takes up, which for a sparse or compressed
// file is less than its size.
func TestLargestAllocated(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "small", randomBytes(10<<10))
	writeFile(t, root, "medium", randomBytes(300<<10))
	sparse := filepath.Join(root, "sparse")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(randomBytes(4 << 10)); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := newRebalancer(t, Config{Root: root})

	p := scan(t, r)
	var want int64
	for _, it := range p.Items {
		info := lstatInfo(t, filepath.Join(root, it.Names[0]))
		if it.Allocated != info.Blocks {
			t.Errorf("%s: allocated %d, want %d", it.Names[0], it.Allocated, info.Blocks)
		}
		want = max(want, info.Blocks)
	}
	if p.LargestAllocated != want || p.LargestFile != 64<<20 {
		t.Errorf("largest allocated %d (want %d), largest file %d", p.LargestAllocated, want, p.LargestFile)
	}
	if lstatInfo(t, sparse).Blocks < 64<<20 && p.LargestAllocated >= p.LargestFile {
		t.Errorf("largest allocated %d isn't below the sparse file's size", p.LargestAllocated)
	}
}

func TestScanStopsWhenCancelled(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "f", []byte("x"))
	r, _ := newRebalancer(t, Config{Root: root})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan with a cancelled context returned %v", err)
	}
}

func TestUnreadableFolderIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any folder")
	}
	root := tempRoot(t)
	writeFile(t, root, "locked/f", []byte("x"))
	writeFile(t, root, "open", []byte("y"))
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	r, logs := newRebalancer(t, Config{Root: root})

	p := scan(t, r)
	if !slices.Equal(itemNames(p), []string{"open"}) || p.Skipped[SkipUnreadable] != 1 {
		t.Errorf("planned %q, skipped %v", itemNames(p), p.Skipped)
	}
	if w := logs.op("warning"); len(w) != 1 || w[0].data["path"] != "locked" || w[0].data["reason"] == "" {
		t.Errorf("warnings = %+v", w)
	}
	logs.checkNoPathsInMessages(t)
}
