//go:build linux || darwin

package fileutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Before any data is written, the copy must already have the original's owner, group and
// permission bits (so nobody the original keeps out can read it), but not yet setuid, setgid or the
// other extended attributes, which come after the data.
func TestReplaceInPlacePreparesEmptyCopy(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	content := randomBytes(t, 200<<10)
	require.NoError(t, os.WriteFile(name, content, 0o600))
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if uint32(g) != mustLstat(t, root, "f").GID && os.Chown(name, -1, g) == nil {
				break
			}
		}
	}
	require.NoError(t, os.Chmod(name, 0o750|os.ModeSetgid)) // chgrp may clear setgid, so set it afterwards
	xattr := unix.Setxattr(name, testXattrName(), []byte("later"), 0) == nil
	before := mustLstat(t, root, "f")

	prepared := false
	setPrepareHook(t, func(tmpRel string) {
		prepared = true
		tmp := mustLstat(t, root, tmpRel)
		require.Zero(t, tmp.Size, "no data may be written before the copy is protected")
		require.Equal(t, before.UID, tmp.UID)
		require.Equal(t, before.GID, tmp.GID)
		require.Equal(t, os.FileMode(0o750), tmp.Mode.Perm())
		require.Zero(t, tmp.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky), "setgid must wait until the data is written")
		if xattr {
			_, err := unix.Getxattr(filepath.Join(dir, tmpRel), testXattrName(), nil)
			require.Error(t, err, "extended attributes other than ACLs come after the data")
		}
	})

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)
	require.True(t, prepared)

	after := mustLstat(t, root, "f")
	require.NotEqual(t, before.ID, after.ID)
	require.Equal(t, before.Mode, after.Mode)
	require.Equal(t, before.GID, after.GID)
	requireContent(t, name, content)
}

// Writing an extended attribute needs write permission, so a read-only file with attributes can
// only be rewritten if the copy stays writable for its owner until they are in place.
func TestReplaceInPlaceKeepsXattrsOnReadOnlyFile(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("read only"), 0o644))
	if err := unix.Setxattr(name, testXattrName(), []byte("kept"), 0); err != nil {
		t.Skipf("this filesystem doesn't support %s: %v", testXattrName(), err)
	}
	require.NoError(t, os.Chmod(name, 0o444))
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	after := mustLstat(t, root, "f")
	require.NotEqual(t, before.ID, after.ID)
	require.Equal(t, os.FileMode(0o444), after.Mode)
	buf := make([]byte, 16)
	n, err := unix.Getxattr(name, testXattrName(), buf)
	require.NoError(t, err)
	require.Equal(t, "kept", string(buf[:n]))
}

// TestHelperReplace is not a test of its own: TestReplaceInPlaceRootRefusesOtherOwnerBeforeCopying
// runs the test binary again as an unprivileged user with only this test selected.
func TestHelperReplace(t *testing.T) {
	dir := os.Getenv("FILEUTIL_HELPER_DIR")
	if dir == "" {
		t.Skip("only runs as a helper process")
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	_, err = ReplaceInPlace(context.Background(), root, "f", Options{})
	fmt.Printf("helper result: ownership=%t err=%v\n", errors.Is(err, ErrOwnership), err)
}

// Without root, a file owned by someone else must be refused before a single byte is read: no
// wasted copy, and (because it is never read) its access time stays as it was.
func TestReplaceInPlaceRootRefusesOtherOwnerBeforeCopying(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to run a helper as another user")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no nobody user: %v", err)
	}
	uid, err := strconv.ParseInt(nobody.Uid, 10, 64)
	require.NoError(t, err)
	gid, err := strconv.ParseInt(nobody.Gid, 10, 64)
	require.NoError(t, err)

	// Everything the helper needs must be reachable by nobody, so it lives outside t.TempDir, and
	// it must be somewhere programs can run (TrueNAS mounts /tmp noexec).
	work, err := os.MkdirTemp(os.Getenv(testDirEnv), "fileutil-helper-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	require.NoError(t, os.Chmod(work, 0o755))
	if noexec(t, work) {
		t.Skipf("%s can't run programs; set %s to a folder that can", work, testDirEnv)
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	bin, err := os.ReadFile(exe)
	require.NoError(t, err)
	helper := filepath.Join(work, "fileutil.test")
	require.NoError(t, os.WriteFile(helper, bin, 0o755))
	data := filepath.Join(work, "data")
	require.NoError(t, os.Mkdir(data, 0o777))
	require.NoError(t, os.Chmod(data, 0o777)) // writable by nobody, so only the owner check can stop it
	name := filepath.Join(data, "f")
	require.NoError(t, os.WriteFile(name, randomBytes(t, 1<<20), 0o644))
	setOldAtime(t, name)
	root, err := os.OpenRoot(data)
	require.NoError(t, err)
	defer root.Close()
	before := mustLstat(t, root, "f")

	cmd := exec.Command(helper, "-test.run=^TestHelperReplace$", "-test.v")
	cmd.Env = append(os.Environ(), "FILEUTIL_HELPER_DIR="+data)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "helper result: ownership=true", "%s", out)

	after := mustLstat(t, root, "f")
	require.True(t, before.Atime.Equal(after.Atime), "the file was read: atime %v -> %v", before.Atime, after.Atime)
	requireUntouched(t, root, "f", before)
	requireNoTemps(t, data)
}

// A temporary copy is locked from the moment it exists until it is renamed into place, including
// after it has been closed, so another run never mistakes it for a leftover.
func TestReplaceInPlaceLocksTemp(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), randomBytes(t, 100<<10), 0o644))

	checks := 0
	inUse := func() {
		checks++
		rel := findTemp(t, dir)
		removed, err := RemoveStaleTemp(root, rel)
		require.NoError(t, err)
		require.False(t, removed, "a temporary copy in use was removed")
		_, err = os.Lstat(filepath.Join(dir, rel))
		require.NoError(t, err)
	}
	setPrepareHook(t, func(string) { inUse() })
	setSwapHook(t, inUse)

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)
	require.Equal(t, 2, checks)
	requireNoTemps(t, dir)
}

func TestReplaceGroupLocksLinkedTemps(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("shared"), 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "sub", "b")))

	var seen []string
	setNameSwapHook(t, func(tempRel, name string) {
		seen = append(seen, name)
		removed, err := RemoveStaleTemp(root, tempRel)
		require.NoError(t, err)
		require.False(t, removed, "the temporary name for %s was removed while in use", name)
	})

	_, err := ReplaceGroup(context.Background(), root, []string{"a", "sub/b"}, Options{})
	require.NoError(t, err)
	require.Equal(t, []string{"sub/b", "a"}, seen)
	requireNoTemps(t, dir)
}

// lockFile takes a lock on name the way another program (or another run) would, and returns a
// function that lets it go.
func lockFile(t *testing.T, name string, how int) (unlock func()) {
	t.Helper()
	f, err := os.Open(name)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(f.Fd()), how|unix.LOCK_NB))
	return func() { require.NoError(t, f.Close()) }
}

// tryLock reports whether name can be locked right now, letting the lock go straight away.
func tryLock(t *testing.T, name string) bool {
	t.Helper()
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false
	}
	require.NoError(t, err)
	return true
}

// A file that another program or run has locked is being worked on, so it is left alone before
// anything is read or copied. Once the lock is gone it is rewritten as usual.
func TestReplaceInPlaceLeavesLockedFileAlone(t *testing.T) {
	for _, tt := range []struct {
		name  string
		how   int
		names []string
	}{
		{"exclusive lock", unix.LOCK_EX, []string{"f"}},
		{"shared lock", unix.LOCK_SH, []string{"f"}},
		{"lock through another hardlink", unix.LOCK_EX, []string{"f", "g"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, randomBytes(t, 64<<10), 0o644))
			for _, other := range tt.names[1:] {
				require.NoError(t, os.Link(name, filepath.Join(dir, other)))
			}
			setOldAtime(t, name)
			before := mustLstat(t, root, "f")
			unlock := lockFile(t, filepath.Join(dir, tt.names[len(tt.names)-1]), tt.how)

			_, err := ReplaceGroup(context.Background(), root, tt.names, Options{})
			require.ErrorIs(t, err, ErrBusy)
			require.Equal(t, "another program or another rebalance run is working on it, so it was left alone — nothing was changed", err.Error())
			require.True(t, before.Atime.Equal(mustLstat(t, root, "f").Atime), "the file was read")
			requireUntouched(t, root, "f", before)
			requireNoTemps(t, dir)

			unlock()
			_, err = ReplaceGroup(context.Background(), root, tt.names, Options{})
			require.NoError(t, err)
			require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
		})
	}
}

// The original stays locked while it is copied, so another run leaves it alone, and the lock goes
// as soon as the rewrite ends, however it ends.
func TestReplaceInPlaceLocksOriginalWhileCopying(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("fails %v", fails), func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, randomBytes(t, 64<<10), 0o644))
			before := mustLstat(t, root, "f")
			checked := false
			setSwapHook(t, func() {
				checked = true
				require.False(t, tryLock(t, name), "the original isn't locked while it is copied")
				_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
				require.ErrorIs(t, err, ErrBusy, "a second rewrite of the same file must leave it alone")
				if fails {
					require.NoError(t, os.Chmod(name, 0o600))
				}
			})

			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.True(t, checked)
			if fails {
				require.ErrorIs(t, err, ErrModified)
				require.Equal(t, before.ID, mustLstat(t, root, "f").ID)
			} else {
				require.NoError(t, err)
				require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
			}
			require.True(t, tryLock(t, name), "the lock outlived the rewrite")
			requireNoTemps(t, dir)
		})
	}
}

func TestRemoveStaleTemp(t *testing.T) {
	const temp = ".zfs-rebalance.0123456789ab.tmp"
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) // creates rel inside dir
		rel     string
		removed bool
		is      error
		keep    bool // rel must still exist afterwards
	}{
		{"left by a stopped run", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, temp), []byte("partial copy"), 0o600))
		}, temp, true, nil, false},
		{"unreadable leftover", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, temp), nil, 0o200))
			if os.Geteuid() != 0 {
				t.Skip("only root can open a file it may not read")
			}
		}, temp, true, nil, false},
		{"in a subfolder", func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", temp), nil, 0o600))
		}, "sub/" + temp, true, nil, false},
		{"in use by another run", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, temp), nil, 0o600))
			f, err := os.Open(filepath.Join(dir, temp))
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB))
		}, temp, false, nil, true},
		{"symlink", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "victim"), []byte("keep"), 0o644))
			require.NoError(t, os.Symlink("victim", filepath.Join(dir, temp)))
		}, temp, false, ErrNotRegular, true},
		{"pipe", func(t *testing.T, dir string) {
			require.NoError(t, unix.Mkfifo(filepath.Join(dir, temp), 0o600))
		}, temp, false, ErrNotRegular, true},
		{"folder", func(t *testing.T, dir string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, temp), 0o755))
		}, temp, false, ErrNotRegular, true},
		{"already gone", func(*testing.T, string) {}, temp, false, fs.ErrNotExist, false},
		{"not one of ours", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("keep"), 0o644))
		}, "photo.jpg", false, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			tt.setup(t, dir)

			removed, err := RemoveStaleTemp(root, tt.rel)
			require.Equal(t, tt.removed, removed)
			switch {
			case tt.is != nil:
				require.ErrorIs(t, err, tt.is)
			case tt.rel == "photo.jpg":
				require.ErrorContains(t, err, "isn't the name of a temporary file")
			default:
				require.NoError(t, err)
			}
			_, statErr := os.Lstat(filepath.Join(dir, filepath.FromSlash(tt.rel)))
			if tt.keep {
				require.NoError(t, statErr, "%s must be left alone", tt.rel)
			} else {
				require.ErrorIs(t, statErr, fs.ErrNotExist)
			}
			if strings.HasPrefix(tt.name, "symlink") {
				requireContent(t, filepath.Join(dir, "victim"), []byte("keep"))
			}
		})
	}
}

// If one of a hardlink group's names disappears while the names are being switched over, the error
// must say so, and how many names already use the new copy.
func TestReplaceGroupNameRemovedWhileSwitching(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	content := []byte("shared content")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), content, 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "sub", "b")))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "c")))
	before := mustLstat(t, root, "a")
	setNameSwapHook(t, func(_, name string) {
		if name == "c" {
			require.NoError(t, os.Remove(filepath.Join(dir, "c")))
		}
	})

	_, err := ReplaceGroup(context.Background(), root, []string{"a", "sub/b", "c"}, Options{})
	require.ErrorIs(t, err, ErrLinkMismatch)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Equal(t, "the file's hardlinks don't match what was expected (one of its names was removed or renamed by another program) — "+
		"1 of its 3 hardlinked names was switched to the new, identical copy; the others still use the original", err.Error())

	require.NotEqual(t, before.ID, mustLstat(t, root, "sub/b").ID)
	require.Equal(t, before.ID, mustLstat(t, root, "a").ID)
	requireContent(t, filepath.Join(dir, "a"), content)
	requireContent(t, filepath.Join(dir, "sub", "b"), content)
	requireNoTemps(t, dir)
}

func TestReplaceGroupNameRemovedBeforeSwitching(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("shared"), 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
	before := mustLstat(t, root, "a")
	setSwapHook(t, func() { require.NoError(t, os.Remove(filepath.Join(dir, "b"))) })

	_, err := ReplaceGroup(context.Background(), root, []string{"a", "b"}, Options{})
	require.ErrorIs(t, err, ErrLinkMismatch)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Equal(t, "the file's hardlinks don't match what was expected (one of its names was removed or renamed by another program) — nothing was changed", err.Error())
	require.Equal(t, before.ID, mustLstat(t, root, "a").ID)
	requireNoTemps(t, dir)
}

// A single file that disappears just before the swap is reported as missing, not as a hardlink
// problem.
func TestReplaceInPlaceFileRemovedBeforeSwap(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644))
	setSwapHook(t, func() { require.NoError(t, os.Remove(filepath.Join(dir, "f"))) })

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.NotErrorIs(t, err, ErrLinkMismatch)
	requireNoTemps(t, dir)
}

func TestInfoBlocks(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, randomBytes(t, 256<<10), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sparse"), []byte("head"), 0o644))
	require.NoError(t, os.Truncate(filepath.Join(dir, "sparse"), 64<<20))

	for _, rel := range []string{"f", "sparse"} {
		info := mustLstat(t, root, rel)
		require.Equal(t, blocksOf(t, filepath.Join(dir, rel))*512, info.Blocks, rel)
	}
	if sparse := mustLstat(t, root, "sparse"); sparse.Blocks >= sparse.Size {
		t.Skip("this filesystem doesn't create sparse files")
	}
	require.Less(t, mustLstat(t, root, "sparse").Blocks, int64(1<<20))
}

// A share whose ACL mode is "restricted" refuses to chmod a file with ACL entries of its own, so
// the setuid and setgid bits can't be put back on its copy. That case gets its own plain words;
// every other failure to set the mode keeps the general ones.
func TestChmodErr(t *testing.T) {
	const suid, sgid = fs.ModeSetuid, fs.ModeSetgid
	withACL := wantedMeta{xattrs: map[string][]byte{"system.nfs4_acl_xdr": {1}}, acl: []byte{1}}
	noACL := wantedMeta{xattrs: map[string][]byte{"user.note": {1}}}
	general := func(errno syscall.Errno) string {
		return "couldn't keep the file's permissions, attributes or timestamps exactly (permissions: " + errno.Error() + ") — nothing was changed"
	}
	specific := func(bits string) string {
		return "its " + bits + " can't be put back on a copy while the file has ACL entries of its own " +
			"(this share's ACL mode doesn't allow it), so it was left alone — nothing was changed"
	}
	tests := []struct {
		name      string
		errno     syscall.Errno
		orig, cur fs.FileMode
		want      wantedMeta
		message   string
	}{
		{"setuid", syscall.EPERM, 0o755 | suid, 0o755, withACL, specific("setuid bit")},
		{"setgid", syscall.EPERM, 0o750 | sgid, 0o750, withACL, specific("setgid bit")},
		{"both", syscall.EPERM, 0o755 | suid | sgid, 0o755, withACL, specific("setuid and setgid bits")},
		{"only one missing", syscall.EPERM, 0o755 | suid | sgid, 0o755 | sgid, withACL, specific("setuid bit")},
		{"no ACL", syscall.EPERM, 0o755 | suid, 0o755, noACL, general(syscall.EPERM)},
		{"other bits differ too", syscall.EPERM, 0o755 | suid, 0o700, withACL, general(syscall.EPERM)},
		{"not refused for the ACL", syscall.EACCES, 0o755 | suid, 0o755, withACL, general(syscall.EACCES)},
		{"no setuid or setgid", syscall.EPERM, 0o644, 0o600, withACL, general(syscall.EPERM)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause := &fs.PathError{Op: "fchmod", Path: "dir/.zfs-rebalance.0123456789ab.tmp", Err: tt.errno}
			err := chmodErr(cause, Info{Mode: tt.orig}, Info{Mode: tt.cur}, tt.want)
			require.ErrorIs(t, err, ErrMetadata)
			require.ErrorIs(t, err, tt.errno)
			require.Equal(t, tt.message, err.Error())
		})
	}
	// Running out of space while setting the mode is still running out of space.
	err := chmodErr(&fs.PathError{Op: "fchmod", Path: "x", Err: syscall.ENOSPC}, Info{Mode: 0o755 | suid}, Info{Mode: 0o755}, withACL)
	require.ErrorIs(t, err, ErrNoSpace)
	require.NotErrorIs(t, err, ErrMetadata)
}
