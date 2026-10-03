//go:build linux || darwin

package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/database"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// fakeZFS answers zfs and zpool commands from a table. A command it doesn't know fails as if the
// tools weren't installed, and is remembered so a test can check none were unexpected.
type fakeZFS struct {
	mu         sync.Mutex
	answers    map[string][]string // command line -> outputs for successive calls; the last repeats
	unexpected []string
	// respond, if set, is asked first; it answers a command by returning handled = true.
	respond func(cmdline string) (out string, handled bool, err error)
}

func (f *fakeZFS) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmdline := strings.Join(append([]string{name}, args...), " ")
	if f.respond != nil {
		if out, ok, err := f.respond(cmdline); ok {
			return []byte(out), err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	outs, ok := f.answers[cmdline]
	if !ok {
		f.unexpected = append(f.unexpected, cmdline)
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	if len(outs) > 1 {
		f.answers[cmdline] = outs[1:]
	}
	return []byte(outs[0]), nil
}

func (f *fakeZFS) unexpectedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unexpected...)
}

// useZFS makes run use f for the rest of the test.
func useZFS(t *testing.T, f *fakeZFS) {
	t.Helper()
	old := newZFSClient
	newZFSClient = func() *zfs.Client { return &zfs.Client{Runner: f} }
	t.Cleanup(func() { newZFSClient = old })
}

// zpoolMirrors is `zpool list -v -H -p -o name,size,allocated,free tank` for two mirrors.
const zpoolMirrors = "tank\t11974368821248\t4187077713920\t7787291107328\n" +
	"\tmirror-0\t3985729650688\t3228441014272\t757288636416\t-\t-\t14\t80\t-\tONLINE\n" +
	"\tsda\t4000776716288\t-\t-\t-\t-\t-\t-\t-\tONLINE\n" +
	"\tsdb\t4000776716288\t-\t-\t-\t-\t-\t-\t-\tONLINE\n" +
	"\tmirror-1\t7988639170560\t958636699648\t7030002470912\t-\t-\t2\t11\t-\tONLINE\n" +
	"\tsdc\t8001561821184\t-\t-\t-\t-\t-\t-\t-\tONLINE\n" +
	"\tsdd\t8001561821184\t-\t-\t-\t-\t-\t-\t-\tONLINE\n"

// runCLI runs the command with args and returns its exit code and output.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runWith(t, &syncBuffer{}, args...)
}

// runWith is runCLI with a given stderr, so a test can react to log lines as they are written.
func runWith(t *testing.T, stderr *syncBuffer, args ...string) (code int, stdout, errOut string) {
	t.Helper()
	var out syncBuffer
	code = run(args, &out, stderr)
	t.Logf("rebalance %s -> exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out.String(), stderr.String())
	return code, out.String(), stderr.String()
}

// isolateState points the default state folder at a new temporary folder and returns the
// folder the run will use inside it.
func isolateState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "go-zfs-rebalance")
}

// makeTree creates a folder holding the named files (slash-separated, relative to it).
func makeTree(t *testing.T, names ...string) string {
	t.Helper()
	root := tempDir(t)
	for _, name := range names {
		writeFile(t, filepath.Join(root, name), contentOf(name))
	}
	return root
}

func contentOf(name string) string { return "the contents of " + name }

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := fileutil.InfoOf(fi)
	if err != nil {
		t.Fatal(err)
	}
	return info.ID.Ino
}

func inodes(t *testing.T, root string, names []string) map[string]uint64 {
	t.Helper()
	m := make(map[string]uint64, len(names))
	for _, name := range names {
		m[name] = inodeOf(t, filepath.Join(root, name))
	}
	return m
}

// checkTree fails unless every named file still holds its contents and no temporary copies are
// left in root.
func checkTree(t *testing.T, root string, names []string) {
	t.Helper()
	for _, name := range names {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != contentOf(name) {
			t.Errorf("%s: contents %q, %v", name, got, err)
		}
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && fileutil.IsTempName(d.Name()) {
			t.Errorf("temporary file left behind: %s", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	code, out, errOut := runCLI(t, "--version")
	if code != exitOK || out != "go-zfs-rebalance dev\n" || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"/no/such/folder", "--help"}} {
		code, out, errOut := runCLI(t, args...)
		if code != exitOK || out != helpText || errOut != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	mustContain(t, helpText,
		"Usage: rebalance [options] <folder>",
		"rebalance --report /mnt/tank/media",
		"rebalance --vdev-report /mnt/tank/media",
		"rebalance --resume /mnt/tank/media",
		"More help: https://github.com/astundzia/go-zfs-rebalance#readme")
	for line := range strings.Lines(helpText) {
		if len([]rune(strings.TrimRight(line, "\n"))) > 80 {
			t.Errorf("help line is too long: %q", line)
		}
	}
}

func TestNoArgumentsShowsHelp(t *testing.T) {
	code, out, errOut := runCLI(t)
	if code != exitUsage || out != "" || errOut != helpText {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestBadArgumentsExitTwo(t *testing.T) {
	stateDir := isolateState(t)
	dir := makeTree(t, "file")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"two folders", []string{dir, dir}, "please give just one folder"},
		{"passes 0", []string{dir, "--passes", "0"}, "A normal run already rewrites every file once"},
		{"unknown checksum", []string{"--checksum", "foo", dir}, `--checksum needs sha256 (the default) or md5, not "foo"`},
		{"concurrency not a number", []string{dir, "--concurrency", "abc"}, `--concurrency needs a whole number, like 4, not "abc"`},
		{"negative size threshold", []string{dir, "--size-threshold", "-5"}, "--size-threshold can't be negative"},
		{"unknown option", []string{dir, "--nope"}, "there's no option called --nope"},
		{"folder missing", []string{filepath.Join(dir, "missing")}, "doesn't exist"},
		{"file instead of folder", []string{filepath.Join(dir, "file")}, "is a file, not a folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, tt.args...)
			if code != exitUsage || out != "" {
				t.Errorf("exit %d, stdout %q; want exit 2 and no output", code, out)
			}
			mustContain(t, errOut, "rebalance: ", tt.want)
		})
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("bad arguments created the state folder (%v)", err)
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	old := goos
	goos = "windows"
	t.Cleanup(func() { goos = old })
	code, _, errOut := runCLI(t, tempDir(t))
	if code != exitUsage {
		t.Errorf("exit %d, want 2", code)
	}
	mustContain(t, errOut, "only works on Linux and macOS, not windows, so nothing was changed")
	if code, _, _ := runCLI(t, "--version"); code != exitOK {
		t.Errorf("--version exit %d, want 0 everywhere", code)
	}
}

func TestRunRewritesEveryFileThenResumes(t *testing.T) {
	isolateState(t)
	names := []string{"a.txt", "sub/b.txt", "sub/c d.txt", "evil\x1b[31m.txt"}
	root := makeTree(t, names...)
	db := filepath.Join(t.TempDir(), "progress.db")
	orig := inodes(t, root, names)

	// Options after the folder are honoured.
	code, _, errOut := runCLI(t, root, "--db", db, "--no-random", "--concurrency", "2")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	first := inodes(t, root, names)
	for _, name := range names {
		if first[name] == orig[name] {
			t.Errorf("%q wasn't rewritten (same inode)", name)
		}
	}
	checkTree(t, root, names)
	mustContain(t, errOut,
		"Rebalancing "+root,
		"Settings: 2 files at a time, in folder order, each copy checked with sha256.",
		"Found 4 files to rebalance",
		"✓ rebalanced  sub/c d.txt",
		`✓ rebalanced  evil\x1b[31m.txt`,
		"Finished in ",
		"rebalanced 4 files")
	mustNotContain(t, errOut, "\x1b", "discarded", "Resuming")
	if os.Geteuid() != 0 {
		mustContain(t, errOut, "! Not running as root, so only your own files can be rewritten. Other users' files, and files in folders you can't change, are skipped and left exactly as they are.")
	}

	// --resume: everything is already done.
	code, _, errOut = runCLI(t, "--resume", "--db", db, root)
	if code != exitOK {
		t.Fatalf("resumed run: exit %d, want 0", code)
	}
	if again := inodes(t, root, names); fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("the resumed run rewrote files: %v -> %v", first, again)
	}
	mustContain(t, errOut, "Resuming where the last run stopped (it had rewritten 4 files).",
		"Finished: there was nothing to rebalance.", "Skipped 4 files: 4 already done.")

	// Without --resume, the run starts over and says so.
	code, _, errOut = runCLI(t, root, "--db", db)
	if code != exitOK {
		t.Fatalf("fresh run: exit %d, want 0", code)
	}
	for name, ino := range inodes(t, root, names) {
		if ino == first[name] {
			t.Errorf("%q wasn't rewritten by the fresh run", name)
		}
	}
	checkTree(t, root, names)
	mustContain(t, errOut, "Starting fresh, so the saved progress of an earlier run (4 files rewritten) was discarded. Next time, add --resume",
		"rebalanced 4 files")
}

func TestRunKeepsProgressInTheStateFolder(t *testing.T) {
	stateDir := isolateState(t)
	root := makeTree(t, "a")
	if code, _, _ := runCLI(t, root); code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	dbPath, err := database.DefaultPath(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dbPath, filepath.Join(stateDir, lockFileName)} {
		if _, err := os.Stat(p); err != nil || filepath.Dir(p) != stateDir {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestRunLeavesItsOwnStateFilesAlone(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a", "b/c")
	db := filepath.Join(root, ".state", "progress.db")
	code, _, errOut := runCLI(t, "--db", db, root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "rebalanced 2 files")

	state := []string{db, filepath.Join(root, ".state", lockFileName)}
	before := make([]uint64, len(state))
	for i, p := range state {
		before[i] = inodeOf(t, p)
	}
	code, _, errOut = runCLI(t, "--db", db, "--resume", root)
	if code != exitOK {
		t.Fatalf("resumed run: exit %d, want 0", code)
	}
	mustContain(t, errOut, "Skipped 2 files: 2 already done.")
	mustNotContain(t, errOut, "✓ rebalanced")
	for i, p := range state {
		if inodeOf(t, p) != before[i] {
			t.Errorf("%s was rewritten", p)
		}
	}
}

func TestRunRefusesWhileAnotherRunIsGoing(t *testing.T) {
	stateDir := isolateState(t)
	release, err := database.AcquireLock(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	root := makeTree(t, "a")
	before := inodeOf(t, filepath.Join(root, "a"))

	code, _, errOut := runCLI(t, root)
	if code != exitUsage {
		t.Errorf("exit %d, want 2", code)
	}
	mustContain(t, errOut, "✗ Can't start: another rebalance is already running; wait for it to finish, then try again")
	if inodeOf(t, filepath.Join(root, "a")) != before {
		t.Error("a file was rewritten while another run held the lock")
	}
}

func TestInterruptedRunCanBeResumed(t *testing.T) {
	sigs := captureSignals(t)
	isolateState(t)
	var names []string
	for i := range 20 {
		names = append(names, fmt.Sprintf("f%02d", i))
	}
	root := makeTree(t, names...)
	db := filepath.Join(t.TempDir(), "progress.db")
	orig := inodes(t, root, names)

	// Press Ctrl+C as the first file is finished. The only worker waits inside the log write
	// while the stop reaches the engine.
	var once sync.Once
	stderr := &syncBuffer{hook: func(line string) {
		if strings.Contains(line, "✓ rebalanced") {
			once.Do(func() {
				(<-sigs) <- os.Interrupt
				time.Sleep(200 * time.Millisecond)
			})
		}
	}}
	code, _, errOut := runWith(t, stderr, "--db", db, "--concurrency", "1", "--no-random", root)
	if code != exitInterrupted {
		t.Fatalf("exit %d, want 130", code)
	}
	mustContain(t, errOut, "! Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)",
		"! Stopped when asked, after ", "To finish the rest, run the same command again with --resume added.")
	checkTree(t, root, names)
	mid := inodes(t, root, names)
	done := 0
	for _, name := range names {
		if mid[name] != orig[name] {
			done++
		}
	}
	if done == 0 || done == len(names) {
		t.Fatalf("%d of %d files were rewritten before the stop; want some but not all", done, len(names))
	}

	code, _, errOut = runCLI(t, "--db", db, "--resume", root)
	if code != exitOK {
		t.Fatalf("resumed run: exit %d, want 0", code)
	}
	mustContain(t, errOut, fmt.Sprintf("rebalanced %d files", len(names)-done), fmt.Sprintf("%d already done", done))
	checkTree(t, root, names)
	for name, ino := range inodes(t, root, names) {
		switch {
		case mid[name] != orig[name] && ino != mid[name]:
			t.Errorf("%s was rewritten twice", name)
		case mid[name] == orig[name] && ino == orig[name]:
			t.Errorf("%s was never rewritten", name)
		}
	}
}

func TestStopWhileLookingThroughTheFolder(t *testing.T) {
	sigs := captureSignals(t)
	isolateState(t)
	names := []string{"a", "b"}
	root := makeTree(t, names...)
	orig := inodes(t, root, names)
	stderr := &syncBuffer{hook: func(line string) {
		if strings.Contains(line, "Looking through the folder") {
			(<-sigs) <- os.Interrupt
			time.Sleep(100 * time.Millisecond)
		}
	}}
	code, _, errOut := runWith(t, stderr, root)
	if code != exitInterrupted {
		t.Fatalf("exit %d, want 130", code)
	}
	mustContain(t, errOut, "! Stopped before any files were changed.")
	mustNotContain(t, errOut, "Found 2 files", "Finished")
	if got := inodes(t, root, names); fmt.Sprint(got) != fmt.Sprint(orig) {
		t.Error("files were rewritten after the stop")
	}
}

func TestHaltOnMissingFile(t *testing.T) {
	isolateState(t)
	names := []string{"a", "b", "c", "d"}
	root := makeTree(t, names...)
	// Delete a file after the scan has listed it, before it is rewritten.
	stderr := &syncBuffer{hook: func(line string) {
		if strings.Contains(line, "Found 4 files to rebalance") {
			if err := os.Remove(filepath.Join(root, "b")); err != nil {
				t.Error(err)
			}
		}
	}}
	code, _, errOut := runWith(t, stderr, "--db", filepath.Join(t.TempDir(), "p.db"), "--halt-on-missing", "--no-random", "--concurrency", "1", root)
	if code != exitHalted {
		t.Fatalf("exit %d, want 3", code)
	}
	mustContain(t, errOut, "! This file went missing, so the run is stopping, as you asked  b",
		"because a file went missing (--halt-on-missing)", "To finish the rest, run the same command again with --resume added.")
	checkTree(t, root, []string{"a", "c", "d"})
}

// TestUnwritableFolderIsSkippedWithoutRoot checks that, without root, a file in a folder the user
// can't change is skipped, with a hint to use sudo rather than --resume, and the run exits 0.
func TestUnwritableFolderIsSkippedWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write in a read-only folder")
	}
	isolateState(t)
	root := makeTree(t, "ok.txt", "ro/locked.txt")
	ro := filepath.Join(root, "ro")
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	before := inodeOf(t, filepath.Join(ro, "locked.txt"))

	code, _, errOut := runCLI(t, root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "! skipped  ro/locked.txt  no permission to replace it (run with sudo to include it)",
		"rebalanced 1 of 2 files", "Skipped 1 file: 1 file you aren't allowed to replace.",
		"To include the 1 file you don't have permission to change, run it again with sudo (that run starts from the beginning).")
	mustNotContain(t, errOut, "--resume", "couldn't be rebalanced")
	if inodeOf(t, filepath.Join(ro, "locked.txt")) != before {
		t.Error("the skipped file was changed")
	}
	checkTree(t, root, []string{"ok.txt", "ro/locked.txt"})

	// Resuming tries the skipped file again, and says plainly that there was nothing new to do.
	code, _, errOut = runCLI(t, "--resume", root)
	if code != exitOK {
		t.Fatalf("resumed run: exit %d, want 0", code)
	}
	mustContain(t, errOut, "! skipped  ro/locked.txt  no permission to replace it",
		"Nothing new to rebalance: the remaining file was skipped again (1 file you aren't allowed to replace).",
		"Skipped 2 files: 1 already done, 1 file you aren't allowed to replace.",
		"To include the 1 file you don't have permission to change, run it again with sudo")
	mustNotContain(t, errOut, "Finished")
	checkTree(t, root, []string{"ok.txt", "ro/locked.txt"})
}

func TestLeftoversAreReported(t *testing.T) {
	isolateState(t)
	names := []string{"keep", "keep.balance"}
	for i := 1; i <= 7; i++ {
		names = append(names, fmt.Sprintf("o%d.balance", i))
	}
	root := makeTree(t, names...)
	temp := filepath.Join(root, fileutil.TempPrefix+"0123456789ab"+fileutil.TempSuffix)
	writeFile(t, temp, "half a copy")
	orig := inodes(t, root, names)

	code, _, errOut := runCLI(t, root, "--no-cleanup-balance")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut,
		"--no-cleanup-balance is now called --no-cleanup. The old name still works for now.",
		"! Found 7 files ending in .balance with no original next to them.",
		"may be the only copies of some files — please check them. They won't be touched:",
		"      o1.balance\n", "      o5.balance\n", "! …and 2 more",
		"Found a file ending in .balance next to one with the same name without it.",
		"! Found a temporary file left behind by an interrupted run. It was kept because of --no-cleanup",
		"rebalanced 2 files", "7 leftover .balance files.")
	mustNotContain(t, errOut, "o6.balance")
	after := inodes(t, root, names)
	for _, name := range names {
		rewritten := after[name] != orig[name]
		if want := name == "keep" || name == "keep.balance"; rewritten != want {
			t.Errorf("%s: rewritten = %v, want %v", name, rewritten, want)
		}
	}
	if _, err := os.Stat(temp); err != nil {
		t.Errorf("the temporary file wasn't kept: %v", err)
	}

	// Without the option, the temporary file is cleaned up.
	code, _, errOut = runCLI(t, root, "--filename-only")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "- removed a leftover temporary file  "+filepath.Base(temp))
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("the temporary file is still there (%v)", err)
	}
}

// TestHardlinkedNamesAreShown checks that without --process-hardlinks a hardlinked file is skipped
// and named, with a hint; that a rewritten hardlinked file's line shows its other names without
// --debug; and that once rewritten, a resumed run without the option counts it as already done.
func TestHardlinkedNamesAreShown(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a", "links/b")
	for _, name := range []string{"links/c", "links/d", "links/e", "z"} {
		if err := os.Link(filepath.Join(root, "links/b"), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errOut := runCLI(t, root, "--no-random")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "rebalanced 1 file", "Skipped 5 files: 5 hardlinked.",
		"These files have hardlinks, so they were left alone:\n",
		"      links/b\n", "      links/c\n", "      links/d\n", "      links/e\n", "      z\n",
		"To include the 5 files with hardlinks, add --process-hardlinks.")
	mustNotContain(t, errOut, "more")

	code, _, errOut = runCLI(t, root, "--no-random", "--process-hardlinks")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "✓ rebalanced  links/b (also named links/c, links/d, links/e, +1 more)  ", "rebalanced 6 files")
	mustNotContain(t, errOut, "      also z")

	code, _, errOut = runCLI(t, root, "--resume")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "Skipped 6 files: 6 already done.")
	mustNotContain(t, errOut, "hardlinked", "--process-hardlinks")
}

func TestFolderNameStartingWithADash(t *testing.T) {
	isolateState(t)
	parent := tempDir(t)
	writeFile(t, filepath.Join(parent, "-dir", "a"), contentOf("a"))
	t.Chdir(parent)
	code, _, errOut := runCLI(t, "--no-random", "--", "-dir")
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut, "Rebalancing "+filepath.Join(parent, "-dir"), "rebalanced 1 file")
}

// treatAsNotZFS makes the run see root as a folder that isn't on ZFS. The kernel is asked as usual,
// unless root really is on ZFS (as with TMPDIR on a TrueNAS pool); then it is made to say no.
func treatAsNotZFS(t *testing.T, root string) {
	t.Helper()
	if on, err := zfs.OnZFS(root); err == nil && !on {
		return
	}
	t.Logf("%s is on ZFS, so the run is told it isn't", root)
	onZFSByKernel(t, false)
}

func TestReportNotOnZFS(t *testing.T) {
	stateDir := isolateState(t)
	root := tempDir(t)
	treatAsNotZFS(t, root)
	code, out, errOut := runCLI(t, "--report", root)
	if code != exitUsage || out != "" {
		t.Errorf("exit %d, stdout %q; want exit 2 and no table", code, out)
	}
	mustContain(t, errOut, fmt.Sprintf("rebalance: %q isn't on ZFS, so there's no pool to report on", root))
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("--report created the state folder (%v)", err)
	}
}

func TestReportShowsThePool(t *testing.T) {
	stateDir := isolateState(t)
	root := tempDir(t)
	f := &fakeZFS{answers: map[string][]string{
		"zfs list -H -o name -- " + root:                       {"tank/media\n"},
		"zpool list -v -H -p -o name,size,allocated,free tank": {zpoolMirrors},
	}}
	useZFS(t, f)
	code, out, errOut := runCLI(t, root, "--report")
	if code != exitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := "Pool tank      SIZE       ALLOC   USED%\n" +
		"mirror-0    3.6 TiB     2.9 TiB   81.0%\n" +
		"mirror-1    7.3 TiB   892.8 GiB   12.0%\n" +
		"spread      69.0 pts\n"
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if u := f.unexpectedCalls(); len(u) > 0 {
		t.Errorf("unexpected commands: %q", u)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("--report created the state folder (%v)", err)
	}
}

func TestVdevReportAndSafetyChecks(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a", "b")
	after := strings.NewReplacer(
		"3228441014272\t757288636416", "2228441014272\t1757288636416",
		"958636699648\t7030002470912", "1958636699648\t6030002470912",
	).Replace(zpoolMirrors)
	f := &fakeZFS{answers: map[string][]string{
		"zfs list -H -o name -- " + root:                        {"tank/media\n"},
		"zfs list -H -t snapshot -r -o name -s name tank/media": {"tank/media@daily-1\n"},
		dedupCmd: {"tank/media\tdedup\tsha256,verify\ntank/media\tmountpoint\t" + root + "\ntank/media\tmounted\tyes\n"},
		"zfs get -Hp -o value available tank/media":            {"10\n"},
		"zpool list -v -H -p -o name,size,allocated,free tank": {zpoolMirrors, after},
		"zpool get -Hp -o value bcloneused tank":               {"0\n", "1048576\n"},
		"zpool sync tank":                                      {""},
		"zpool get -Hp -o value freeing tank":                  {"0\n"},
		"zfs get -Hp -r -o value usedbysnapshots tank/media":   {"4096\n-\n"},
	}}
	useZFS(t, f)

	code, out, errOut := runCLI(t, "--vdev-report", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	if code != exitOK {
		t.Fatalf("exit %d, want 0", code)
	}
	mustContain(t, errOut,
		"! This dataset has snapshots. Every rewritten file will be stored twice until those snapshots are removed — keep an eye on free space.",
		"! Deduplication is on for this dataset (dedup=sha256,verify).",
		"! Free space is tight: 10 B is free, and working on 2 files at a time may need up to ",
		"Waiting for ZFS to finish freeing the old copies' space",
		"rebalanced 2 files")
	mustNotContain(t, errOut, "isn't on ZFS")
	want := "Before:\n" +
		"Pool tank      SIZE       ALLOC   USED%\n" +
		"mirror-0    3.6 TiB     2.9 TiB   81.0%\n" +
		"mirror-1    7.3 TiB   892.8 GiB   12.0%\n" +
		"spread      69.0 pts\n" +
		"\n" +
		"Before and after:\n" +
		"Pool tank   USED%            ALLOC CHANGE\n" +
		"mirror-0    81.0% -> 55.9%     -931.3 GiB\n" +
		"mirror-1    12.0% -> 24.5%     +931.3 GiB\n" +
		"spread      69.0 pts -> 31.4 pts\n" +
		"note: snapshots hold 4.0 KiB of old data, so the old vdevs won't shrink until those snapshots are removed\n" +
		"note: block cloning grew by 1.0 MiB during the run; if nothing else was copying files, some files may have been cloned instead of rewritten\n" +
		"note: deduplication is on for tank/media, so the files rewritten there may not have moved\n"
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
	if u := f.unexpectedCalls(); len(u) > 0 {
		t.Errorf("unexpected commands: %q", u)
	}
}

// dedupCmd is the command that lists the dedup setting of tank/media and the datasets below it.
const dedupCmd = "zfs get -r -H -t filesystem -o name,property,value dedup,mountpoint,mounted tank/media"

func TestVdevReportWhenNotOnZFS(t *testing.T) {
	isolateState(t)
	root := makeTree(t, "a")
	treatAsNotZFS(t, root)
	code, out, errOut := runCLI(t, "--vdev-report", root)
	if code != exitOK || out != "" {
		t.Fatalf("exit %d, stdout %q; want 0 and no table", code, out)
	}
	mustContain(t, errOut, "! This folder isn't on ZFS, so rewriting files won't rebalance anything.",
		"! Skipping the vdev report, because this folder isn't on ZFS.", "rebalanced 1 file")
}

func TestVdevComparisonAfterCtrlC(t *testing.T) {
	f := &fakeZFS{answers: map[string][]string{
		"zpool list -v -H -p -o name,size,allocated,free tank": {zpoolMirrors},
		"zfs get -Hp -r -o value usedbysnapshots tank/media":   {"0\n"},
		"zpool get -Hp -o value bcloneused tank":               {"5\n"},
	}}
	log, logOut := testLogger(logrus.InfoLevel)
	z := &zfsView{client: &zfs.Client{Runner: f}, log: log, dataset: "tank/media"}
	before, err := zfs.ParseZpoolList([]byte(zpoolMirrors))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var out syncBuffer
	z.reportAfter(cancelled, cancelled, &out, &vdevBefore{dist: before, bclone: 5, bcloneOK: true})
	mustContain(t, out.String(), "Before and after:\nPool tank", "spread      69.0 pts -> 69.0 pts\n",
		"note: ZFS may not have finished freeing the old copies' space, so the old vdevs may look fuller than they really are\n")
	mustNotContain(t, out.String(), "snapshots hold", "block cloning")
	mustNotContain(t, logOut.String(), "Waiting for ZFS")
	if u := f.unexpectedCalls(); len(u) > 0 {
		t.Errorf("unexpected commands (the wait should be skipped): %q", u)
	}
}
