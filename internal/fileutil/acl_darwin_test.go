//go:build darwin

package fileutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// noexec reports whether dir is on a filesystem mounted noexec.
func noexec(t *testing.T, dir string) bool {
	t.Helper()
	var st unix.Statfs_t
	require.NoError(t, unix.Statfs(dir, &st))
	return st.Flags&unix.MNT_NOEXEC != 0
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
	// Without root, a "deny" entry could block the steps after the copy, so the ACL comes last.
	early := want
	if os.Geteuid() != 0 {
		early = nil
	}
	setPrepareHook(t, func(tmpRel string) { require.Equal(t, early, aclOf(t, filepath.Join(dir, tmpRel))) })

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
// had it, so it must be removed again, and before any data is written: otherwise everyone the
// folder lets in could read (or change) a private file's data while it is being copied.
func TestReplaceInPlaceDropsInheritedMacACL(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(name, []byte("private"), 0o600))
	require.Nil(t, aclOf(t, name))
	addACL(t, dir, "everyone allow read,write,file_inherit")

	prepared := false
	setPrepareHook(t, func(tmpRel string) {
		prepared = true
		tmp := filepath.Join(dir, tmpRel)
		require.Nil(t, aclOf(t, tmp), "the copy still has the folder's ACL while data is written: %s", listACL(t, tmp))
	})
	_, err := ReplaceInPlace(context.Background(), root, "secret", Options{})
	require.NoError(t, err)
	require.True(t, prepared)

	require.Nil(t, aclOf(t, name), "the inherited ACL must not stick: %s", listACL(t, name))
	requireContent(t, name, []byte("private"))
}

// chflags uchg (or uappnd) stops even root from renaming over a file, so it must be refused before
// anything is read or copied.
func TestReplaceInPlaceRefusesImmutable(t *testing.T) {
	for _, flag := range []int{unix.UF_IMMUTABLE, unix.UF_APPEND} {
		t.Run(fmt.Sprintf("%#x", flag), func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, randomBytes(t, 64<<10), 0o644))
			setOldAtime(t, name)
			require.NoError(t, unix.Chflags(name, flag))
			t.Cleanup(func() { _ = unix.Chflags(name, 0) })
			before := mustLstat(t, root, "f")

			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.ErrorIs(t, err, ErrImmutable)
			require.Equal(t, "it's marked immutable or append-only (chflags uchg / uappnd), so it was left alone — nothing was changed", err.Error())

			after := mustLstat(t, root, "f")
			require.True(t, before.Atime.Equal(after.Atime), "the file was read before it was refused")
			requireUntouched(t, root, "f", before)
			requireNoTemps(t, dir)
		})
	}
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
