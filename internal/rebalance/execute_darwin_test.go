package rebalance

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// A folder whose times the run changed but can't put back is warned about once and counted. On
// macOS an ACL can deny changing a folder's attributes while still letting its files be replaced.
func TestFolderTimesNotRestoredAreCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ACLs don't apply to root")
	}
	root := tempRoot(t)
	for _, rel := range []string{"stuck/a", "stuck/b", "fine/c"} {
		writeFile(t, root, rel, []byte(rel))
	}
	old := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, dir := range []string{"stuck", "fine"} {
		if err := os.Chtimes(filepath.Join(root, dir), old, old); err != nil {
			t.Fatal(err)
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(root, "stuck")
	acl := me.Username + " deny writeattr"
	if out, err := exec.Command("/bin/chmod", "+a", acl, stuck).CombinedOutput(); err != nil {
		t.Skipf("couldn't add an ACL: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-a", acl, stuck).Run() })
	before := snapshot(t, root)
	r, logs := newRebalancer(t, Config{Root: root})

	_, s := scanAndExecute(t, r)
	if s.Rebalanced != 3 || s.Failed != 0 || s.FolderTimesNotRestored != 1 {
		t.Errorf("summary = %+v", s)
	}
	checkRewritten(t, before, snapshot(t, root), "stuck/a", "stuck/b", "fine/c")
	w := logs.op("warning")
	if len(w) != 1 || w[0].level != logrus.WarnLevel || w[0].data["path"] != "stuck" || w[0].data["reason"] == "" {
		t.Errorf("warnings = %+v", w)
	}
	if got := lstatInfo(t, filepath.Join(root, "fine")).Mtime; !got.Equal(old) {
		t.Errorf("fine's modified time is %v, want it put back to %v", got, old)
	}
	logs.checkNoPathsInMessages(t)
}
