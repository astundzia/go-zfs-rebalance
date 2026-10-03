//go:build darwin

package fileutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// addACL adds a macOS ACL entry with chmod(1), skipping the test if the filesystem has no ACLs.
// The ACL is removed again when the test ends so that its files can be deleted.
func addACL(t *testing.T, name, entry string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", entry, name).CombinedOutput(); err != nil {
		t.Skipf("can't add an ACL here: %v: %s", err, out)
	}
	dir := name
	if fi, err := os.Stat(name); err == nil && !fi.IsDir() {
		dir = filepath.Dir(name)
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-R", "-N", dir).Run() })
}

func aclOf(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()
	acl, err := readACL(f)
	require.NoError(t, err)
	return acl
}

func listACL(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("/bin/ls", "-le", name).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

func TestReplaceInPlaceKeepsMacACL(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("protected"), 0o644))
	addACL(t, name, "everyone deny write")
	addACL(t, name, "group:staff allow read,append")
	want := aclOf(t, name)
	require.NotNil(t, want)
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	after := mustLstat(t, root, "f")
	require.NotEqual(t, before.ID, after.ID)
	require.True(t, before.Mtime.Equal(after.Mtime))
	require.Equal(t, want, aclOf(t, name))
	listing := listACL(t, name)
	require.Contains(t, listing, "group:everyone deny write")
	require.Contains(t, listing, "group:staff allow read,append")
}

// A "deny delete" entry stops anyone but root from renaming over the file. The original must stay
// as it was and the temporary copy, which carries the same entry, must still be cleaned up.
func TestReplaceInPlaceMacACLDenyDelete(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("undeletable"), 0o644))
	addACL(t, name, "everyone deny delete")
	want := aclOf(t, name)
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	if os.Geteuid() == 0 {
		require.NoError(t, err)
		require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
	} else {
		require.ErrorIs(t, err, os.ErrPermission)
		require.Equal(t, "couldn't swap in the new copy (permission denied) — nothing was changed", err.Error())
		requireUntouched(t, root, "f", before)
	}
	require.Equal(t, want, aclOf(t, name))
	requireNoTemps(t, dir)
}

// A directory's inheritable ACL is applied to the new copy when it is created; the original never
// had it, so it must be removed again.
func TestReplaceInPlaceDropsInheritedMacACL(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("plain"), 0o644))
	require.Nil(t, aclOf(t, name))
	addACL(t, dir, "everyone allow read,file_inherit")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	require.Nil(t, aclOf(t, name), "the inherited ACL must not stick: %s", listACL(t, name))
}

func TestMacACLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	require.NoError(t, os.WriteFile(src, nil, 0o644))
	addACL(t, src, "everyone deny delete")
	addACL(t, src, "user:root allow read")
	dst, err := os.Create(filepath.Join(dir, "dst"))
	require.NoError(t, err)
	defer dst.Close()

	acl := aclOf(t, src)
	require.NoError(t, writeACL(dst, acl))
	got, err := readACL(dst)
	require.NoError(t, err)
	require.Equal(t, acl, got)

	require.NoError(t, writeACL(dst, nil))
	got, err = readACL(dst)
	require.NoError(t, err)
	require.Nil(t, got)
}
