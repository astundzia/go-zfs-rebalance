//go:build linux || darwin

package rebalance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

func TestCantRewrite(t *testing.T) {
	me := account{uid: 1000, groups: map[uint32]bool{1000: true, 27: true}}
	plainDir := fileutil.Info{Mode: os.ModeDir | 0o755, GID: 50}
	setgidDir := fileutil.Info{Mode: os.ModeDir | os.ModeSetgid | 0o2775, GID: 50}
	// macOS gives every new file its folder's group; Linux only does so for setgid folders.
	var passesGroup SkipReason
	if runtime.GOOS != "darwin" {
		passesGroup = SkipGroup
	}
	texts := map[SkipReason]string{"": "", SkipOwner: reasonOtherOwner, SkipGroup: reasonOtherGroup}

	tests := []struct {
		name string
		as   account
		file fileutil.Info
		dir  fileutil.Info
		want SkipReason
	}{
		{"root rewrites anything", account{root: true}, fileutil.Info{UID: 7, GID: 7}, plainDir, ""},
		{"my file, my group", me, fileutil.Info{UID: 1000, GID: 1000}, plainDir, ""},
		{"my file, another of my groups", me, fileutil.Info{UID: 1000, GID: 27}, plainDir, ""},
		{"someone else's file", me, fileutil.Info{UID: 1001, GID: 1000}, plainDir, SkipOwner},
		{"someone else's file in a setgid folder", me, fileutil.Info{UID: 1001, GID: 50}, setgidDir, SkipOwner},
		{"my file, a group I'm not in", me, fileutil.Info{UID: 1000, GID: 60}, plainDir, SkipGroup},
		{"my file, its setgid folder's group", me, fileutil.Info{UID: 1000, GID: 50}, setgidDir, ""},
		{"my file, its plain folder's group", me, fileutil.Info{UID: 1000, GID: 50}, plainDir, passesGroup},
		{"folder unknown", me, fileutil.Info{UID: 1000, GID: 50}, fileutil.Info{}, SkipGroup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why, text := tt.as.cantRewrite(tt.file, tt.dir); why != tt.want || text != texts[tt.want] {
				t.Errorf("cantRewrite = %q, %q; want %q, %q", why, text, tt.want, texts[tt.want])
			}
		})
	}
}

func TestCurrentAccount(t *testing.T) {
	a := currentAccount()
	if a.root != (os.Geteuid() == 0) {
		t.Fatalf("root = %v with euid %d", a.root, os.Geteuid())
	}
	if !a.root && (a.uid != uint32(os.Geteuid()) || !a.groups[uint32(os.Getegid())]) {
		t.Errorf("account %+v doesn't match euid %d, egid %d", a, os.Geteuid(), os.Getegid())
	}
}

// Without root, files owned by someone else are skipped by Scan: never opened, never copied, and
// (because they are never read) their access times stay as they were.
func TestOtherUsersFilesSkippedWithoutReading(t *testing.T) {
	root := tempRoot(t)
	names := []string{"a", "sub/b", "sub/c"}
	for _, n := range names {
		writeFile(t, root, n, randomBytes(64<<10))
	}
	if err := os.Link(filepath.Join(root, "a"), filepath.Join(root, "a-link")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root) // reads the files, so the access times are set afterwards
	for _, n := range names {
		setOldAtime(t, filepath.Join(root, n))
	}
	forbidReplace(t)
	r, logs := newRebalancer(t, Config{Root: root, ProcessHardlinks: true})
	someoneElse := ownerOf(t, filepath.Join(root, "a"))
	someoneElse.uid++
	r.as = someoneElse

	p, s := scanAndExecute(t, r)
	if len(p.Items) != 0 || p.Skipped[SkipOwner] != 4 || s.Skipped[SkipOwner] != 4 || s.Rebalanced != 0 || s.Failed != 0 {
		t.Errorf("plan %+v, summary %+v", p, s)
	}
	for _, n := range names {
		if got := lstatInfo(t, filepath.Join(root, n)).Atime; !got.Equal(oldAtime) {
			t.Errorf("%s was read: access time %v, want %v", n, got, oldAtime)
		}
	}
	checkUntouched(t, before, snapshot(t, root), append(names, "a-link")...)
	skipped := logs.op("skipped")
	if len(skipped) != 4 {
		t.Fatalf("skipped log lines = %+v", skipped)
	}
	for _, e := range skipped {
		if e.level != logrus.DebugLevel || e.data["reason"] != reasonOtherOwner {
			t.Errorf("skipped log line = %+v", e)
		}
	}
}

// The owner is checked again just before each file is rewritten, since it may have changed since
// the scan.
func TestOwnerCheckedAgainBeforeCopying(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "a", []byte("a"))
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root})
	me := ownerOf(t, filepath.Join(root, "a"))
	r.as = me
	p := scan(t, r)
	if p.TotalFiles != 1 {
		t.Fatalf("plan = %+v", p)
	}

	forbidReplace(t)
	someoneElse := me
	someoneElse.uid++
	r.as = someoneElse // as if the file had been given to someone else after the scan
	s := execute(t, context.Background(), r, p)
	if s.Skipped[SkipOwner] != 1 || s.Rebalanced != 0 || s.Remaining != 0 {
		t.Errorf("summary = %+v", s)
	}
	checkUntouched(t, before, snapshot(t, root), "a")
	if sk := logs.op("skipped"); len(sk) != 1 || sk[0].level != logrus.InfoLevel || sk[0].data["reason"] != reasonOtherOwner {
		t.Errorf("skipped log lines = %+v", sk)
	}
}

func TestGroupNotYoursSkippedUnlessTheFolderGivesIt(t *testing.T) {
	t.Run("a group the folder doesn't give", func(t *testing.T) {
		root := tempRoot(t)
		writeFile(t, root, "mine", []byte("x"))
		writeFile(t, root, "other-group", []byte("x"))
		dir := lstatInfo(t, root)
		// The file needs a group that is neither the account's nor (on macOS) the folder's.
		other, ok := otherGroup(dir.GID)
		if !ok {
			t.Skip("the user running the tests is in only one group")
		}
		if err := os.Lchown(filepath.Join(root, "other-group"), -1, int(other)); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, root)
		r, _ := newRebalancer(t, Config{Root: root})
		mine := lstatInfo(t, filepath.Join(root, "mine"))
		r.as = account{uid: mine.UID, groups: map[uint32]bool{mine.GID: true}}

		p, s := scanAndExecute(t, r)
		if p.Skipped[SkipGroup] != 1 || len(p.Skipped) != 1 || s.Rebalanced != 1 {
			t.Errorf("plan skipped %v, summary %+v", p.Skipped, s)
		}
		if got := SkipGroup.Phrase(1); got != "1 file in a group you're not in" {
			t.Errorf("summary label %q", got)
		}
		after := snapshot(t, root)
		checkUntouched(t, before, after, "other-group")
		checkRewritten(t, before, after, "mine")
	})

	t.Run("the folder's group", func(t *testing.T) {
		root := tempRoot(t)
		sub := filepath.Join(root, "shared")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "darwin" {
			// Linux gives new files the group of setgid folders. (os.Chmod ignores a raw 0o2000.)
			if err := os.Chmod(sub, 0o755|os.ModeSetgid); err != nil {
				t.Fatal(err)
			}
			if lstatInfo(t, sub).Mode&os.ModeSetgid == 0 {
				t.Fatal("the folder didn't get the setgid bit")
			}
		}
		writeFile(t, root, "shared/f", []byte("x"))
		f := lstatInfo(t, filepath.Join(sub, "f"))
		if f.GID != lstatInfo(t, sub).GID {
			t.Skipf("this filesystem didn't give the new file its folder's group")
		}
		before := snapshot(t, root)
		r, _ := newRebalancer(t, Config{Root: root})
		r.as = account{uid: f.UID, groups: map[uint32]bool{}} // in none of the groups

		_, s := scanAndExecute(t, r)
		if s.Rebalanced != 1 || len(s.Skipped) != 0 {
			t.Errorf("summary = %+v", s)
		}
		checkRewritten(t, before, snapshot(t, root), "shared/f")
	})
}

// otherGroup returns a group other than gid that the user running the tests can give a file.
func otherGroup(gid uint32) (uint32, bool) {
	if os.Geteuid() == 0 {
		return gid + 4242, true
	}
	gids, _ := os.Getgroups()
	for _, g := range append(gids, os.Getegid()) {
		if uint32(g) != gid {
			return uint32(g), true
		}
	}
	return 0, false
}

const helperDirEnv = "REBALANCE_HELPER_DIR"

// TestHelperRunAsOtherUser is not a test of its own: TestRootRunWithoutRootSkipsOthersFiles runs
// the test binary again as an unprivileged user with only this test selected.
func TestHelperRunAsOtherUser(t *testing.T) {
	dir := os.Getenv(helperDirEnv)
	if dir == "" {
		t.Skip("only runs as a helper process")
	}
	r, err := New(Config{Root: dir, Passes: 1, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	p, err := r.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Execute(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("helper result: rebalanced=%d failed=%d owner=%d no-permission=%d folder-times-not-restored=%d\n",
		s.Rebalanced, s.Failed, s.Skipped[SkipOwner], s.Skipped[SkipNoPermission], s.FolderTimesNotRestored)
}

// A run without root, for real: other people's files are skipped without being read (not reported
// as failures, even ones it can't open), the user's own file in a folder it can't write to is
// skipped for lack of permission, and a folder whose times it can't put back is counted.
func TestRootRunWithoutRootSkipsOthersFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a helper as another user")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no nobody user: %v", err)
	}
	uid, err := strconv.ParseInt(nobody.Uid, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseInt(nobody.Gid, 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	// Everything the helper needs must be reachable by nobody, so it lives outside t.TempDir, and
	// it must be somewhere programs can run (TrueNAS mounts /tmp noexec).
	work, err := os.MkdirTemp(os.Getenv(testDirEnv), "rebalance-helper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	if work, err = filepath.EvalSymlinks(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if noexec(t, work) {
		t.Skipf("%s can't run programs; set %s to a folder that can", work, testDirEnv)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(work, "rebalance.test")
	if err := os.WriteFile(helper, bin, 0o755); err != nil {
		t.Fatal(err)
	}

	// data is root's, but anyone may add files to it, so only the owner check can keep the run
	// away from root's files there.
	data := filepath.Join(work, "data")
	for _, dir := range []string{data, filepath.Join(data, "locked")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(data, 0o777); err != nil {
		t.Fatal(err)
	}
	files := map[string]os.FileMode{"root-public": 0o644, "root-private": 0o600, "mine": 0o644, "locked/mine": 0o644}
	for rel, mode := range files {
		p := filepath.Join(data, rel)
		if err := os.WriteFile(p, randomBytes(256<<10), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, "mine") {
			if err := os.Lchown(p, int(uid), int(gid)); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(data, old, old); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, data) // reads the files, so the access times are set afterwards
	for _, rel := range []string{"root-public", "root-private"} {
		setOldAtime(t, filepath.Join(data, rel))
	}

	cmd := exec.Command(helper, "-test.run=^TestHelperRunAsOtherUser$", "-test.v")
	cmd.Env = append(os.Environ(), helperDirEnv+"="+data)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	want := "helper result: rebalanced=1 failed=0 owner=2 no-permission=1 folder-times-not-restored=1"
	if !strings.Contains(string(out), want) {
		t.Fatalf("helper output doesn't contain %q:\n%s", want, out)
	}

	for _, rel := range []string{"root-public", "root-private"} {
		if got := lstatInfo(t, filepath.Join(data, rel)).Atime; !got.Equal(oldAtime) {
			t.Errorf("%s was read: access time %v, want %v", rel, got, oldAtime)
		}
	}
	after := snapshot(t, data)
	checkUntouched(t, before, after, "root-public", "root-private", "locked/mine")
	checkRewritten(t, before, after, "mine")
	if got := lstatInfo(t, filepath.Join(data, "mine")); got.UID != uint32(uid) {
		t.Errorf("the rewritten file's owner is %d, want %d", got.UID, uid)
	}
	checkNoTemps(t, data)
}
