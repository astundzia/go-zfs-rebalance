//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/rebalance"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// onZFSByKernel makes the run believe the kernel says every folder is (or isn't) on ZFS.
func onZFSByKernel(t *testing.T, on bool) {
	t.Helper()
	old := zfsOnPath
	zfsOnPath = func(string) (bool, error) { return on, nil }
	t.Cleanup(func() { zfsOnPath = old })
}

// refusedStart is how starting zfs or zpool fails on TrueNAS once the sudo session that started the
// run has ended.
func refusedStart(cmdline string) error {
	name, _, _ := strings.Cut(cmdline, " ")
	return &zfs.CommandError{Command: cmdline, Err: &fs.PathError{Op: "fork/exec", Path: "/sbin/" + name, Err: syscall.ENOSYS}}
}

// poolAnswers are the answers for a normal --vdev-report run on root, held by tank/media.
func poolAnswers(root string) map[string][]string {
	return map[string][]string{
		"zfs list -H -o name -- " + root:                        {"tank/media\n"},
		"zfs list -H -t snapshot -r -o name -s name tank/media": {""},
		dedupCmd: {"tank/media\tdedup\toff\ntank/media\tmountpoint\t" + root + "\ntank/media\tmounted\tyes\n"},
		"zfs get -Hp -o value available tank/media":            {"1000000000000\n"},
		"zpool list -v -H -p -o name,size,allocated,free tank": {zpoolMirrors},
		"zpool get -Hp -o value bcloneused tank":               {"0\n"},
		"zpool sync tank":                                      {""},
		"zpool get -Hp -o value freeing tank":                  {"0\n"},
		"zfs get -Hp -r -o value usedbysnapshots tank/media":   {"0\n"},
	}
}

func TestWhyNoDataset(t *testing.T) {
	notZFS := fmt.Errorf("%w: zfs said: not a ZFS filesystem", zfs.ErrNotZFS)
	missing := fmt.Errorf("%w: %w", zfs.ErrToolsMissing, exec.ErrNotFound)
	blocked := fmt.Errorf("%w: %w", zfs.ErrExecBlocked, syscall.ENOSYS)
	other := errors.New("zfs said: out of memory")
	unknown := errors.New("statfs failed")
	tests := []struct {
		err     error
		on      bool
		statErr error
		want    noDataset
	}{
		{notZFS, false, nil, notOnZFS},
		{missing, false, nil, notOnZFS},
		{missing, true, nil, toolsMissing},
		{missing, false, unknown, toolsMissing},
		{blocked, true, nil, execBlocked},
		{blocked, false, unknown, execBlocked},
		{notZFS, true, nil, cantReachZFS},
		{notZFS, false, unknown, notOnZFS},
		{other, true, nil, lookupFailed},
		{other, false, unknown, lookupFailed},
	}
	old := zfsOnPath
	t.Cleanup(func() { zfsOnPath = old })
	for _, tt := range tests {
		zfsOnPath = func(string) (bool, error) { return tt.on, tt.statErr }
		if got, _ := whyNoDataset("/mnt/tank", tt.err); got != tt.want {
			t.Errorf("whyNoDataset(%v) with the kernel saying on=%v, err=%v: got %d, want %d", tt.err, tt.on, tt.statErr, got, tt.want)
		}
	}
}

// TestOnZFSWithoutTheTools checks that a ZFS folder isn't called "not on ZFS" just because the zfs
// commands are missing.
func TestOnZFSWithoutTheTools(t *testing.T) {
	isolateState(t)
	onZFSByKernel(t, true)
	root := makeTree(t, "a")
	code, out, errOut := runCLI(t, "--vdev-report", root)
	if code != exitOK || out != "" {
		t.Fatalf("exit %d, stdout %q; want 0 and no table", code, out)
	}
	mustContain(t, errOut,
		"! This folder is on ZFS, but the zfs and zpool commands weren't found, so the ZFS safety checks (snapshots, deduplication and free space) were skipped.",
		"! Skipping the vdev report, because the zfs and zpool commands weren't found.",
		"rebalanced 1 file")
	mustNotContain(t, errOut, "isn't on ZFS", "executable file not found")

	code, out, errOut = runCLI(t, "--report", root)
	if code != exitUsage || out != "" {
		t.Errorf("--report: exit %d, stdout %q; want 2 and no table", code, out)
	}
	mustContain(t, errOut, "rebalance: the zfs and zpool commands weren't found, so there's no pool to report on.")
	mustNotContain(t, errOut, "isn't on ZFS")
}

func TestZFSToolsCantReachZFS(t *testing.T) {
	isolateState(t)
	onZFSByKernel(t, true)
	root := makeTree(t, "a")
	useZFS(t, &fakeZFS{respond: func(cmdline string) (string, bool, error) {
		return "", true, errors.New(cmdline + ": The ZFS modules are not loaded. Try running 'modprobe zfs' as root to load them. (exit status 1)")
	}})
	code, _, errOut := runCLI(t, root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "! This folder is on ZFS, but the zfs tools can't reach ZFS from here (as inside some containers), so the ZFS safety checks")
	mustNotContain(t, errOut, "isn't on ZFS")
}

// TestZFSToolsRefusedAfterTheRun is TrueNAS once the sudo session that started the run has ended:
// starting zpool fails with ENOSYS. The run explains it once, in plain words, and skips the table.
// It doesn't say it is waiting for ZFS when it can't even ask.
func TestZFSToolsRefusedAfterTheRun(t *testing.T) {
	for _, tt := range []struct {
		firstRefused string
		waiting      bool // the wait for ZFS was announced before the refusal
	}{
		{"zpool get -Hp -o value freeing tank", false},
		{"zpool sync tank", true},
	} {
		t.Run(tt.firstRefused, func(t *testing.T) {
			isolateState(t)
			root := makeTree(t, "a", "b")
			var refused atomic.Bool
			f := &fakeZFS{answers: poolAnswers(root), respond: func(cmdline string) (string, bool, error) {
				if cmdline == tt.firstRefused {
					refused.Store(true)
				}
				if refused.Load() {
					return "", true, refusedStart(cmdline)
				}
				return "", false, nil
			}}
			useZFS(t, f)
			code, out, errOut := runCLI(t, "--vdev-report", "--db", filepath.Join(t.TempDir(), "p.db"), root)
			if code != exitOK {
				t.Fatalf("exit %d, want 0", code)
			}
			mustContain(t, out, "Before:\n")
			mustNotContain(t, out, "Before and after:")
			mustContain(t, errOut, "rebalanced 2 files",
				"! Couldn't start the zfs tools, so there's no before-and-after table. TrueNAS blocks starting other programs once the sudo session that started this run has ended. Next time, start tmux first without sudo, then run sudo rebalance inside it.")
			mustNotContain(t, errOut, "function not implemented", "fork/exec", "/sbin/zpool")
			if strings.Count(errOut, "TrueNAS") != 1 {
				t.Error("the explanation should be given once")
			}
			if got := strings.Contains(errOut, "Waiting for ZFS"); got != tt.waiting {
				t.Errorf("said it was waiting for ZFS: %v, want %v", got, tt.waiting)
			}
		})
	}
}

// TestZFSToolsGoneAfterTheRun checks that when the zfs tools can't be found any more after the run,
// the run doesn't say it is waiting for ZFS, and says why there is no table.
func TestZFSToolsGoneAfterTheRun(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a")
	var gone atomic.Bool
	useZFS(t, &fakeZFS{answers: poolAnswers(root), respond: func(cmdline string) (string, bool, error) {
		if cmdline == "zpool get -Hp -o value freeing tank" {
			gone.Store(true)
		}
		if gone.Load() {
			return "", true, &exec.Error{Name: "zpool", Err: exec.ErrNotFound}
		}
		return "", false, nil
	}})
	code, out, errOut := runCLI(t, "--vdev-report", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustNotContain(t, out, "Before and after:")
	mustContain(t, errOut, "rebalanced 1 file",
		"! Couldn't read how full each vdev is after the run, so there's no before-and-after table  the zfs and zpool commands weren't found")
	mustNotContain(t, errOut, "Waiting for ZFS", "executable file not found")
}

// TestZFSToolsRefusedDuringTheRun is TrueNAS when the sudo session ends while files are still being
// rewritten. The block-cloning watch finds out first: it explains why (once), stops asking, and the
// end of the run skips the wait and the table without trying the zfs tools again.
func TestZFSToolsRefusedDuringTheRun(t *testing.T) {
	isolateState(t)
	old := cloneWatch
	cloneWatch = time.Millisecond
	t.Cleanup(func() { cloneWatch = old })
	root := makeTree(t, "a", "b", "c")
	var refusing atomic.Bool
	var refused atomic.Int32
	useZFS(t, &fakeZFS{answers: poolAnswers(root), respond: func(cmdline string) (string, bool, error) {
		if !refusing.Load() {
			return "", false, nil
		}
		refused.Add(1)
		return "", true, refusedStart(cmdline)
	}})
	var once sync.Once
	stderr := &syncBuffer{hook: func(line string) {
		if strings.Contains(line, "✓ rebalanced") {
			// Hold the run here until the watch has been refused.
			once.Do(func() {
				refusing.Store(true)
				for deadline := time.Now().Add(5 * time.Second); refused.Load() == 0 && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
			})
		}
	}}
	code, out, errOut := runWith(t, stderr, "--vdev-report", "--concurrency", "1", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, out, "Before:\n")
	mustNotContain(t, out, "Before and after:")
	mustContain(t, errOut, "rebalanced 3 files",
		"! Couldn't start the zfs tools, so block cloning is no longer being checked. TrueNAS blocks starting other programs",
		"! Skipping the before-and-after table, because the zfs tools couldn't be started.")
	mustNotContain(t, errOut, "Waiting for ZFS", "function not implemented")
	if strings.Count(errOut, "TrueNAS") != 1 {
		t.Error("the explanation should be given once")
	}
	if n := refused.Load(); n != 1 {
		t.Errorf("the zfs tools were tried %d times after being refused, want just the once", n)
	}
}

func TestZFSToolsRefusedAtTheStart(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a")
	useZFS(t, &fakeZFS{respond: func(cmdline string) (string, bool, error) { return "", true, refusedStart(cmdline) }})
	code, out, errOut := runCLI(t, "--vdev-report", root)
	if code != exitOK || out != "" {
		t.Fatalf("exit %d, stdout %q; want 0 and no table", code, out)
	}
	mustContain(t, errOut, "! Couldn't start the zfs tools, so the ZFS safety checks (snapshots, deduplication and free space) were skipped. TrueNAS blocks",
		"! Skipping the vdev report, because the zfs tools couldn't be started.")
	mustNotContain(t, errOut, "function not implemented", "isn't on ZFS")

	code, _, errOut = runCLI(t, "--report", root)
	if code != exitUsage {
		t.Errorf("--report: exit %d, want 2", code)
	}
	mustContain(t, errOut, "rebalance: couldn't start the zfs tools, so there's no report. TrueNAS blocks")
	mustNotContain(t, errOut, "function not implemented")
}

func TestDedupInside(t *testing.T) {
	all := []zfs.DatasetDedup{
		{Name: "tank/media", Dedup: "off", Mountpoint: "/mnt/tank/media", Mounted: true},
		{Name: "tank/media/kid", Dedup: "on", Mountpoint: "/mnt/tank/media/sub/kid", Mounted: true},
		{Name: "tank/media/elsewhere", Dedup: "on", Mountpoint: "/srv/elsewhere", Mounted: true},
		{Name: "tank/media/sibling", Dedup: "verify", Mountpoint: "/mnt/tank/media/subway", Mounted: true},
		{Name: "tank/media/unmounted", Dedup: "on", Mountpoint: "/mnt/tank/media/sub/off", Mounted: false},
		{Name: "tank/media/legacy", Dedup: "sha256", Mountpoint: "legacy", Mounted: true},
		{Name: "tank/media/none", Dedup: "on", Mountpoint: "none"},
		{Name: "tank/media/sub/plain", Dedup: "off", Mountpoint: "/mnt/tank/media/sub/plain", Mounted: true},
	}
	got := dedupedInside(all, "tank/media", "/mnt/tank/media/sub")
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	if want := "tank/media/kid tank/media/legacy"; strings.Join(names, " ") != want {
		t.Errorf("deduped inside /mnt/tank/media/sub: %v, want %s", names, want)
	}
	if got := dedupWarning(got, "tank/media"); got != "Deduplication is on for 2 datasets in this folder: tank/media/kid (dedup=on), tank/media/legacy (dedup=sha256). "+
		"Rewriting will be slow, and the new copies may just point back at the old blocks, so the data may not move." {
		t.Errorf("warning: %q", got)
	}
	if got := dedupWarning(got[:1], "tank/media"); !strings.HasPrefix(got, "Deduplication is on for tank/media/kid (dedup=on), which is inside this folder. Rewriting") {
		t.Errorf("warning for one child: %q", got)
	}
	if got := dedupWarning(nil, "tank/media"); got != "" {
		t.Errorf("warning with no dedup: %q", got)
	}
}

// TestDedupOnAChildDataset checks the warning and the note under the table for a dataset mounted
// inside the folder, when the folder's own dataset has deduplication off.
func TestDedupOnAChildDataset(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a", "kid/b")
	answers := poolAnswers(root)
	answers[dedupCmd] = []string{"tank/media\tdedup\toff\ntank/media\tmountpoint\t" + root + "\ntank/media\tmounted\tyes\n" +
		"tank/media/kid\tdedup\ton\ntank/media/kid\tmountpoint\t" + filepath.Join(root, "kid") + "\ntank/media/kid\tmounted\tyes\n" +
		"tank/media/away\tdedup\ton\ntank/media/away\tmountpoint\t/srv/away\ntank/media/away\tmounted\tyes\n"}
	useZFS(t, &fakeZFS{answers: answers})
	code, out, errOut := runCLI(t, "--vdev-report", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "! Deduplication is on for tank/media/kid (dedup=on), which is inside this folder.")
	mustContain(t, out, "note: deduplication is on for tank/media/kid, so the files rewritten there may not have moved\n")
	mustNotContain(t, errOut+out, "tank/media/away")
}

// TestBlockCloningSeenDuringTheRun checks that block cloning that rises during the run and falls
// back by the end (as it does when a cloned copy replaces its original) still gets its note.
func TestBlockCloningSeenDuringTheRun(t *testing.T) {
	isolateState(t)
	old := cloneWatch
	cloneWatch = time.Millisecond
	t.Cleanup(func() { cloneWatch = old })
	root := makeTree(t, "a", "b", "c")
	var during atomic.Bool
	var samples atomic.Int32
	useZFS(t, &fakeZFS{answers: poolAnswers(root), respond: func(cmdline string) (string, bool, error) {
		if cmdline != "zpool get -Hp -o value bcloneused tank" {
			return "", false, nil
		}
		if during.Load() {
			samples.Add(1)
			return "524288\n", true, nil
		}
		return "0\n", true, nil
	}})
	var once sync.Once
	stderr := &syncBuffer{hook: func(line string) {
		switch {
		case strings.Contains(line, "✓ rebalanced"):
			// Hold the run here until the pool has been sampled while the counter is up.
			once.Do(func() {
				during.Store(true)
				for deadline := time.Now().Add(5 * time.Second); samples.Load() == 0 && time.Now().Before(deadline); {
					time.Sleep(time.Millisecond)
				}
			})
		case strings.Contains(line, "Finished in"):
			during.Store(false)
		}
	}}
	code, out, _ := runWith(t, stderr, "--vdev-report", "--concurrency", "1", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if samples.Load() == 0 {
		t.Fatal("the block-cloning counter wasn't read during the run")
	}
	mustContain(t, out, "note: block cloning grew by 512.0 KiB during the run")
}

// TestStopWhileWaitingForZFS checks what Ctrl+C says while waiting for ZFS to free space: the wait
// is skipped, and nothing is said about files being copied.
func TestStopWhileWaitingForZFS(t *testing.T) {
	for _, alreadyStopping := range []bool{false, true} {
		t.Run(fmt.Sprintf("already stopping=%v", alreadyStopping), func(t *testing.T) {
			sigs := captureSignals(t)
			advance := testClock(t)
			isolateState(t)
			root := makeTree(t, "a", "b", "c")
			answers := poolAnswers(root)
			answers["zpool get -Hp -o value freeing tank"] = []string{"4096\n"} // never done
			useZFS(t, &fakeZFS{answers: answers})
			var c chan<- os.Signal
			var once sync.Once
			stderr := &syncBuffer{hook: func(line string) {
				once.Do(func() { c = <-sigs })
				switch {
				case alreadyStopping && strings.Contains(line, "✓ rebalanced"):
					c <- os.Interrupt
					time.Sleep(100 * time.Millisecond)
				case strings.Contains(line, "Waiting for ZFS to finish freeing"):
					advance(2 * time.Second)
					c <- os.Interrupt
				}
			}}
			start := time.Now()
			code, out, errOut := runWith(t, stderr, "--vdev-report", "--concurrency", "1", "--no-random", "--db", filepath.Join(t.TempDir(), "p.db"), root)
			if waited := time.Since(start); waited > time.Minute {
				t.Errorf("the run waited %v", waited)
			}
			want := exitOK
			if alreadyStopping {
				want = exitInterrupted
			}
			if code != want {
				t.Errorf("exit %d, want %d", code, want)
			}
			mustContain(t, errOut, "! Skipping the wait for ZFS to free space…")
			mustNotContain(t, errOut, "abandoned", "Quitting")
			mustContain(t, out, "Before and after:", "note: ZFS may not have finished freeing the old copies' space")
		})
	}
}

func TestCheckSpaceUsesSpaceOnDisk(t *testing.T) {
	tests := []struct {
		allocated int64
		warn      bool
	}{
		{4096, false},   // a 1 TiB sparse file taking up 4 KiB
		{1 << 30, true}, // the same file, filled in
		{0, false},      // nothing taking up space
	}
	for _, tt := range tests {
		f := &fakeZFS{answers: map[string][]string{"zfs get -Hp -o value available tank/media": {"1048576\n"}}}
		log, buf := testLogger(logrus.InfoLevel)
		z := &zfsView{client: &zfs.Client{Runner: f}, log: log, dataset: "tank/media"}
		plan := &rebalance.Plan{Items: make([]rebalance.Item, 4), LargestFile: 1 << 40, LargestAllocated: tt.allocated}
		z.checkSpace(context.Background(), plan, 2)
		if got := strings.Contains(buf.String(), "Free space is tight"); got != tt.warn {
			t.Errorf("allocated %d: warned = %v, want %v (%q)", tt.allocated, got, tt.warn, buf.String())
		}
		if tt.warn {
			mustContain(t, buf.String(), "1.0 MiB is free, and working on 2 files at a time may need up to 4.0 GiB")
		}
	}
}

func TestZFSReason(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %w", zfs.ErrToolsMissing, exec.ErrNotFound), "the zfs and zpool commands weren't found"},
		{fmt.Errorf("%w: %w", zfs.ErrExecBlocked, refusedStart("zpool sync tank")), "the system wouldn't start the zfs tools"},
		{context.DeadlineExceeded, "the zfs tools took too long to answer"},
		{&zfs.CommandError{Command: "zpool list -v tank", Stderr: "cannot open 'tank': no such pool", Err: errors.New("exit status 1")},
			"zpool said: cannot open 'tank': no such pool"},
		{errors.New("something else"), "something else"},
	}
	for _, tt := range tests {
		if got := zfsReason(tt.err); got != tt.want {
			t.Errorf("zfsReason(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// TestOwnLogFileIsLeftAlone runs with the log written to a file inside the folder, as with
// `rebalance /mnt/tank > /mnt/tank/rebalance.log 2>&1`. Rewriting that file would send the rest of
// the log to the old, deleted copy.
func TestOwnLogFileIsLeftAlone(t *testing.T) {
	isolateState(t)
	names := []string{"a", "b"}
	root := makeTree(t, names...)
	logPath := filepath.Join(root, "rebalance.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	before, orig := inodeOf(t, logPath), inodes(t, root, names)

	code := run([]string{"--no-random", "--db", filepath.Join(t.TempDir(), "p.db"), root}, logFile, logFile)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	if inodeOf(t, logPath) != before {
		t.Error("the log file was rewritten")
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(got), "Found 2 files to rebalance", "Finished in ", "rebalanced 2 files")
	for name, ino := range inodes(t, root, names) {
		if ino == orig[name] {
			t.Errorf("%s wasn't rewritten", name)
		}
	}
}

// TestProgressFileOfAnotherFolder checks that reusing --db for a different folder says whose
// progress was discarded, without suggesting --resume (which would refuse that file).
func TestProgressFileOfAnotherFolder(t *testing.T) {
	isolateState(t)
	db := filepath.Join(t.TempDir(), "p.db")
	first, second := makeTree(t, "a"), makeTree(t, "b")
	if code, _, _ := runCLI(t, "--db", db, first); code != exitOK {
		t.Fatalf("first run: exit %d", code)
	}
	code, _, errOut := runCLI(t, "--db", db, second)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, fmt.Sprintf("! Starting fresh. The progress file %s held the saved progress of a different folder, %s (1 file rewritten), and that was discarded.", db, first))
	mustNotContain(t, errOut, "--resume")
}

func TestProgressFileThatIsntOurs(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a")
	notes := filepath.Join(t.TempDir(), "notes.txt")
	writeFile(t, notes, "my important notes\n")
	before := inodeOf(t, filepath.Join(root, "a"))
	code, _, errOut := runCLI(t, "--db", notes, root)
	if code != exitUsage {
		t.Errorf("exit %d, want 2", code)
	}
	mustContain(t, errOut, fmt.Sprintf("✗ Can't start: %q isn't a rebalance progress file, so it was left alone. Choose a different --db file", notes))
	if got, _ := os.ReadFile(notes); string(got) != "my important notes\n" {
		t.Errorf("the file was changed: %q", got)
	}
	if inodeOf(t, filepath.Join(root, "a")) != before {
		t.Error("a file was rewritten")
	}
}
