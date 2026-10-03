//go:build linux || darwin

package fileutil

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// newRoot returns a fresh directory and an os.Root opened on it.
func newRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := testDir(t)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func mustLstat(t *testing.T, root *os.Root, rel string) Info {
	t.Helper()
	info, err := Lstat(root, rel)
	require.NoError(t, err)
	return info
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// requireNoTemps fails if any of our temporary files is left anywhere under dir.
func requireNoTemps(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		require.False(t, IsTempName(d.Name()), "temporary file left behind: %s", p)
		return nil
	}))
}

// requireUntouched checks that rel is still the same, unmodified inode. Atime is ignored because
// reading the file may update it, and so is the allocated size, which ZFS only settles once its
// pending writes reach the disks.
func requireUntouched(t *testing.T, root *os.Root, rel string, before Info) {
	t.Helper()
	after := mustLstat(t, root, rel)
	after.Atime, after.Blocks = before.Atime, before.Blocks
	require.Equal(t, before, after, "%s was changed", rel)
}

// findTemp returns the name of the one temporary file in dir.
func findTemp(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if IsTempName(e.Name()) {
			return e.Name()
		}
	}
	t.Fatal("no temporary copy found")
	return ""
}

func setPrepareHook(t *testing.T, hook func(tmpRel string)) {
	t.Helper()
	afterPrepareHook = hook
	t.Cleanup(func() { afterPrepareHook = nil })
}

func setNameSwapHook(t *testing.T, hook func(tempRel, name string)) {
	t.Helper()
	beforeNameSwapHook = hook
	t.Cleanup(func() { beforeNameSwapHook = nil })
}

// setOldAtime gives name an access time far in the past, which Linux's default relatime would
// move to now as soon as the file is read.
func setOldAtime(t *testing.T, name string) {
	t.Helper()
	fi, err := os.Stat(name)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(name, time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC), fi.ModTime()))
}

func requireContent(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, len(want), len(got), "size")
	require.True(t, bytes.Equal(want, got), "content differs")
}

func TestReplaceInPlaceContent(t *testing.T) {
	zeros := make([]byte, 3*zeroChunk+17)
	mixed := bytes.Join([][]byte{
		make([]byte, zeroChunk), []byte("data"), make([]byte, 2*zeroChunk), []byte("end"), make([]byte, zeroChunk+1),
	}, nil)

	tests := []struct {
		name    string
		content []byte
		opts    Options
	}{
		{"zero length", nil, Options{}},
		{"small text", []byte("hello, rebalance\n"), Options{}},
		{"5 MiB random", randomBytes(t, 5<<20), Options{}},
		{"5 MiB random, md5, odd buffer", randomBytes(t, 5<<20+3), Options{Checksum: ChecksumMD5, BufferSize: 300_001}},
		{"all zero", zeros, Options{}},
		{"holes and data", mixed, Options{BufferSize: zeroChunk}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), tt.content, 0o644))
			before := mustLstat(t, root, "f")

			res, err := ReplaceInPlace(context.Background(), root, "f", tt.opts)
			require.NoError(t, err)

			after := mustLstat(t, root, "f")
			require.NotEqual(t, before.ID, after.ID, "the file should be a new inode")
			require.Equal(t, after.ID, res.NewID)
			require.Equal(t, before.ID, res.Before.ID)
			require.Equal(t, int64(len(tt.content)), res.Size)
			require.Equal(t, int64(len(tt.content)), after.Size)
			require.Equal(t, uint64(1), after.Nlink)
			require.Positive(t, res.Duration)
			requireContent(t, filepath.Join(dir, "f"), tt.content)
			requireNoTemps(t, dir)
		})
	}
}

func TestReplaceInPlaceKeepsHoles(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "sparse")
	const size = 8 << 20
	require.NoError(t, os.WriteFile(name, []byte("head"), 0o644))
	require.NoError(t, os.Truncate(name, size))
	if blocksOf(t, name)*512 >= size {
		t.Skip("this filesystem doesn't create sparse files")
	}

	_, err := ReplaceInPlace(context.Background(), root, "sparse", Options{})
	require.NoError(t, err)

	want := make([]byte, size)
	copy(want, "head")
	requireContent(t, name, want)
	if runtime.GOOS == "darwin" {
		t.Skip("APFS fills in gaps left by seeking past the end of a file, so only content and size are checked")
	}
	require.Less(t, blocksOf(t, name)*512, int64(size), "the holes were filled in")
}

func blocksOf(t *testing.T, name string) int64 {
	t.Helper()
	fi, err := os.Stat(name)
	require.NoError(t, err)
	return fi.Sys().(*syscall.Stat_t).Blocks
}

func TestReplaceInPlaceKeepsMode(t *testing.T) {
	modes := []os.FileMode{
		0o644, 0o600, 0o755, 0o400, 0o444, 0o070,
		0o755 | os.ModeSetuid,
		0o755 | os.ModeSetgid,
		0o644 | os.ModeSticky,
		0o755 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"), 0o600))
			// A new file may take its folder's group (always on macOS), and only a member of the
			// file's group may set setgid on it.
			require.NoError(t, os.Chown(name, -1, os.Getegid()))
			require.NoError(t, os.Chmod(name, mode))
			before := mustLstat(t, root, "f")
			require.Equal(t, mode, before.Mode.Perm()|before.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky),
				"setting up the original")
			if mode&0o400 == 0 && os.Geteuid() != 0 {
				t.Skip("an unreadable file can only be rewritten by root")
			}

			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.NoError(t, err)

			after := mustLstat(t, root, "f")
			require.NotEqual(t, before.ID, after.ID)
			require.Equal(t, before.Mode, after.Mode)
			require.Equal(t, before.UID, after.UID)
			require.Equal(t, before.GID, after.GID)
		})
	}
}

func TestReplaceInPlaceKeepsGroup(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("x"), 0o640))
	before := mustLstat(t, root, "f")

	groups, err := os.Getgroups()
	require.NoError(t, err)
	other := -1
	for _, g := range groups {
		if uint32(g) != before.GID && os.Chown(name, -1, g) == nil {
			other = g
			break
		}
	}
	if other < 0 {
		t.Skip("no other group to move the file to")
	}
	require.NoError(t, os.Chmod(name, 0o750|os.ModeSetgid)) // chgrp may clear setgid, so set it afterwards
	before = mustLstat(t, root, "f")
	require.NotZero(t, before.Mode&os.ModeSetgid, "setting up the original")

	_, err = ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	after := mustLstat(t, root, "f")
	require.NotEqual(t, before.ID, after.ID)
	require.Equal(t, uint32(other), after.GID)
	require.Equal(t, before.UID, after.UID)
	require.Equal(t, before.Mode, after.Mode)
}

func TestReplaceInPlaceKeepsTimes(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("timestamps"), 0o644))
	atime := time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.UTC)
	mtime := time.Date(2010, 11, 12, 13, 14, 15, 987654321, time.UTC)
	require.NoError(t, os.Chtimes(name, atime, mtime))
	before := mustLstat(t, root, "f")
	// Compare against what the filesystem stored, in case it keeps less than nanoseconds.
	require.WithinDuration(t, mtime, before.Mtime, time.Second)
	require.WithinDuration(t, atime, before.Atime, time.Second)

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	after := mustLstat(t, root, "f") // before reading the content, which may update atime
	require.NotEqual(t, before.ID, after.ID)
	require.True(t, before.Mtime.Equal(after.Mtime), "mtime %v != %v", after.Mtime, before.Mtime)
	require.True(t, before.Atime.Equal(after.Atime), "atime %v != %v", after.Atime, before.Atime)
}

func testXattrName() string {
	if runtime.GOOS == "linux" {
		return "user.test"
	}
	return "com.example.test"
}

func TestReplaceInPlaceKeepsXattrs(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("with attributes"), 0o644))

	attrs := map[string][]byte{
		testXattrName():            []byte("hello"),
		testXattrName() + ".bin":   {0, 1, 2, 0, 255},
		testXattrName() + ".big":   bytes.Repeat([]byte("v"), 2000),
		testXattrName() + ".empty": {},
	}
	for k, v := range attrs {
		if err := unix.Setxattr(name, k, v, 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
				t.Skipf("this filesystem doesn't support %s: %v", k, err)
			}
			require.NoError(t, err)
		}
	}
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()
	got, err := readXattrs(f)
	require.NoError(t, err)
	for k, v := range attrs {
		value, ok := got[k]
		require.True(t, ok, "%s is missing", k)
		require.Equal(t, v, value, k)
	}
}

// cancelAfter is a context whose Err starts returning context.Canceled after n calls, so a test can
// cancel at every point where ReplaceInPlace checks for cancellation.
type cancelAfter struct {
	context.Context
	n atomic.Int64
}

func (c *cancelAfter) Err() error {
	if c.n.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

func TestReplaceInPlaceCancelled(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	content := randomBytes(t, 64<<10)
	require.NoError(t, os.WriteFile(name, content, 0o644))
	before := mustLstat(t, root, "f")

	cancelled := 0
	for n := int64(0); ; n++ {
		ctx := &cancelAfter{Context: context.Background()}
		ctx.n.Store(n)
		_, err := ReplaceInPlace(ctx, root, "f", Options{BufferSize: 4096})
		if err == nil {
			break // n was past the last check
		}
		cancelled++
		require.ErrorIs(t, err, context.Canceled, "after %d checks", n)
		require.Equal(t, "stopped before the copy was finished — nothing was changed", err.Error())
		requireUntouched(t, root, "f", before)
		requireNoTemps(t, dir)
		require.Less(t, n, int64(1000), "never finished")
	}
	require.Greater(t, cancelled, 30, "expected a cancellation check for every chunk copied and verified")
	requireContent(t, name, content)
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceCancelledFromAnotherGoroutine(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	content := randomBytes(t, 32<<20)
	require.NoError(t, os.WriteFile(name, content, 0o644))
	before := mustLstat(t, root, "f")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hookCalled := make(chan struct{})
	setSwapHook(t, func() { close(hookCalled) })
	go func() {
		time.Sleep(time.Millisecond)
		cancel()
	}()
	_, err := ReplaceInPlace(ctx, root, "f", Options{BufferSize: 4096})
	select {
	case <-hookCalled:
		t.Skip("the copy finished before the cancel arrived")
	default:
	}
	require.ErrorIs(t, err, context.Canceled)
	requireUntouched(t, root, "f", before)
	requireContent(t, name, content)
	requireNoTemps(t, dir)
}

func setSwapHook(t *testing.T, hook func()) {
	t.Helper()
	beforeSwapHook = hook
	t.Cleanup(func() { beforeSwapHook = nil })
}

func TestReplaceInPlaceDetectsChangesBeforeSwap(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, dir string)
	}{
		{"appended", func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, "f"), os.O_WRONLY|os.O_APPEND, 0)
			require.NoError(t, err)
			_, err = f.WriteString(" and more")
			require.NoError(t, err)
			require.NoError(t, f.Close())
		}},
		{"rewritten with the same size", func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, "f"), os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte("O"), 0)
			require.NoError(t, err)
			require.NoError(t, f.Close())
		}},
		{"permissions changed", func(t *testing.T, dir string) {
			require.NoError(t, os.Chmod(filepath.Join(dir, "f"), 0o600))
		}},
		{"mtime changed", func(t *testing.T, dir string) {
			now := time.Now()
			require.NoError(t, os.Chtimes(filepath.Join(dir, "f"), now, now))
		}},
		{"hardlink added", func(t *testing.T, dir string) {
			require.NoError(t, os.Link(filepath.Join(dir, "f"), filepath.Join(dir, "g")))
		}},
		{"replaced by another file", func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "other"), []byte("original content"), 0o644))
			require.NoError(t, os.Rename(filepath.Join(dir, "other"), filepath.Join(dir, "f")))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, []byte("original content"), 0o644))
			before := mustLstat(t, root, "f")
			// Make sure a change within the same clock tick is still visible as a new ctime.
			time.Sleep(10 * time.Millisecond)

			setSwapHook(t, func() { tt.change(t, dir) })
			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.ErrorIs(t, err, ErrModified)
			require.Equal(t, "the file changed while it was being copied — nothing was changed", err.Error())
			requireNoTemps(t, dir)

			after := mustLstat(t, root, "f")
			if tt.name != "replaced by another file" {
				require.Equal(t, before.ID, after.ID, "the original must stay in place")
			}
		})
	}
}

func TestReplaceInPlaceDetectsReplacedTemp(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("original"), 0o644))
	before := mustLstat(t, root, "f")
	setSwapHook(t, func() {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			if IsTempName(e.Name()) {
				require.NoError(t, os.Remove(filepath.Join(dir, e.Name())))
				require.NoError(t, os.WriteFile(filepath.Join(dir, e.Name()), []byte("impostor"), 0o644))
				return
			}
		}
		t.Error("no temporary copy found")
	})

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.ErrorIs(t, err, ErrModified)
	require.Contains(t, err.Error(), "its temporary copy was removed or replaced by another program")
	requireUntouched(t, root, "f", before)
	requireContent(t, filepath.Join(dir, "f"), []byte("original"))
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceAppendKeepsBothWrites(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("original"), 0o644))
	setSwapHook(t, func() {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, err = f.WriteString("+appended")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	})

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.ErrorIs(t, err, ErrModified)
	requireContent(t, name, []byte("original+appended"))
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceRefusesNonRegular(t *testing.T) {
	dir, root := newRoot(t)
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("target"), 0o644))
	require.NoError(t, os.Symlink("victim", filepath.Join(dir, "link")))
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dir"), 0o755))
	victimBefore := mustLstat(t, root, "victim")

	for _, rel := range []string{"link", "fifo", "dir"} {
		t.Run(rel, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := ReplaceInPlace(context.Background(), root, rel, Options{})
				done <- err
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, ErrNotRegular)
			case <-time.After(10 * time.Second):
				t.Fatal("ReplaceInPlace hung")
			}
			requireNoTemps(t, dir)
		})
	}
	requireUntouched(t, root, "victim", victimBefore)
	requireContent(t, victim, []byte("target"))
}

func TestReplaceInPlaceRefusesHardlinkedFile(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("shared"), 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
	before := mustLstat(t, root, "a")

	_, err := ReplaceInPlace(context.Background(), root, "a", Options{})
	require.ErrorIs(t, err, ErrLinkMismatch)
	requireUntouched(t, root, "a", before)
	requireUntouched(t, root, "b", before)
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceRejectsUnknownChecksum(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644))
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{Checksum: "crc32"})
	require.ErrorContains(t, err, "use sha256 (the default) or md5")
	requireUntouched(t, root, "f", before)
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceMissingFile(t *testing.T) {
	_, root := newRoot(t)
	_, err := ReplaceInPlace(context.Background(), root, "missing", Options{})
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Equal(t, "couldn't check the file (no such file or directory) — nothing was changed", err.Error())
}

func TestReplaceInPlaceStaysInsideRoot(t *testing.T) {
	outside := testDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(outside, "f"), []byte("outside"), 0o644))
	dir, root := newRoot(t)
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "escape")))

	for _, rel := range []string{"../f", "escape/f", filepath.Join(outside, "f")} {
		_, err := ReplaceInPlace(context.Background(), root, rel, Options{})
		require.Error(t, err, rel)
	}
	requireContent(t, filepath.Join(outside, "f"), []byte("outside"))
	requireNoTemps(t, outside)
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceInSubdirectory(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755))
	name := filepath.Join(dir, "a", "b", "f")
	require.NoError(t, os.WriteFile(name, []byte("nested"), 0o644))
	before := mustLstat(t, root, "a/b/f")

	_, err := ReplaceInPlace(context.Background(), root, "a/b/f", Options{})
	require.NoError(t, err)
	require.NotEqual(t, before.ID, mustLstat(t, root, "a/b/f").ID)
	requireContent(t, name, []byte("nested"))
	requireNoTemps(t, dir)
}

func TestCreateTempNeverReusesExistingNames(t *testing.T) {
	dir, root := newRoot(t)
	victim := filepath.Join(dir, "victim")
	require.NoError(t, os.WriteFile(victim, []byte("do not touch"), 0o644))
	symlinkName := ".zfs-rebalance.aaaaaaaaaaaa.tmp"
	danglingName := ".zfs-rebalance.bbbbbbbbbbbb.tmp"
	fifoName := ".zfs-rebalance.cccccccccccc.tmp"
	fileName := ".zfs-rebalance.dddddddddddd.tmp"
	require.NoError(t, os.Symlink("victim", filepath.Join(dir, symlinkName)))
	require.NoError(t, os.Symlink("nowhere", filepath.Join(dir, danglingName)))
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, fifoName), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("user data"), 0o644))
	freshName := ".zfs-rebalance.eeeeeeeeeeee.tmp"

	names := []string{symlinkName, danglingName, fifoName, fileName, freshName}
	next := func() string {
		n := names[0]
		names = names[1:]
		return n
	}
	f, rel, err := createTemp(root, ".", next)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, freshName, rel)

	requireContent(t, victim, []byte("do not touch"))
	requireContent(t, filepath.Join(dir, fileName), []byte("user data"))
	target, err := os.Readlink(filepath.Join(dir, danglingName))
	require.NoError(t, err)
	require.Equal(t, "nowhere", target)
	_, err = os.Lstat(filepath.Join(dir, "nowhere"))
	require.ErrorIs(t, err, fs.ErrNotExist, "a dangling symlink must not be followed")
	fi, err := os.Lstat(filepath.Join(dir, fifoName))
	require.NoError(t, err)
	require.Equal(t, fs.ModeNamedPipe, fi.Mode().Type())
	fi, err = os.Lstat(filepath.Join(dir, symlinkName))
	require.NoError(t, err)
	require.Equal(t, fs.ModeSymlink, fi.Mode().Type())
	fi, err = os.Lstat(filepath.Join(dir, freshName))
	require.NoError(t, err)
	require.True(t, fi.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm()&0o600)

	_, _, err = createTemp(root, ".", func() string { return symlinkName })
	require.ErrorIs(t, err, fs.ErrExist, "gives up when every name is taken")
	requireContent(t, victim, []byte("do not touch"))
}

func TestLinkTempNeverReusesExistingNames(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target"), []byte("data"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "victim"), []byte("do not touch"), 0o644))
	taken := ".zfs-rebalance.aaaaaaaaaaaa.tmp"
	fresh := ".zfs-rebalance.bbbbbbbbbbbb.tmp"
	require.NoError(t, os.Symlink("victim", filepath.Join(dir, taken)))

	names := []string{taken, fresh}
	rel, err := linkTemp(root, "target", ".", func() string { n := names[0]; names = names[1:]; return n })
	require.NoError(t, err)
	require.Equal(t, fresh, rel)
	require.Equal(t, mustLstat(t, root, "target").ID, mustLstat(t, root, fresh).ID)
	requireContent(t, filepath.Join(dir, "victim"), []byte("do not touch"))
	fi, err := os.Lstat(filepath.Join(dir, taken))
	require.NoError(t, err)
	require.Equal(t, fs.ModeSymlink, fi.Mode().Type())
}

func TestReplaceGroup(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	content := randomBytes(t, 300<<10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), content, 0o640))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "sub", "b")))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "c")))
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 600, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "a"), mtime, mtime))
	before := mustLstat(t, root, "a")
	require.Equal(t, uint64(3), before.Nlink)

	names := []string{"a", "sub/b", "c"}
	res, err := ReplaceGroup(context.Background(), root, names, Options{})
	require.NoError(t, err)

	for _, rel := range names {
		after := mustLstat(t, root, rel)
		require.NotEqual(t, before.ID, after.ID, rel)
		require.Equal(t, res.NewID, after.ID, rel)
		require.Equal(t, uint64(3), after.Nlink, rel)
		require.Equal(t, before.Mode, after.Mode, rel)
		require.True(t, before.Mtime.Equal(after.Mtime), rel)
		requireContent(t, filepath.Join(dir, filepath.FromSlash(rel)), content)
	}
	requireNoTemps(t, dir)
}

func TestReplaceGroupRejectsWrongNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		twice bool
	}{
		{"missing a name", []string{"a", "b"}, false},
		{"includes another file", []string{"a", "b", "other"}, false},
		{"same name twice", []string{"a", "b", "./a"}, true},
		{"same name through another path", []string{"a", "b", "sub/../a"}, true},
		{"same name through a symlinked directory", []string{"a", "b", "alias/a"}, true},
		{"too many names", []string{"a", "b", "c", "other"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("shared"), 0o644))
			require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
			require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "c")))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "other"), []byte("shared"), 0o644))
			require.NoError(t, os.Symlink(".", filepath.Join(dir, "alias")))
			before := mustLstat(t, root, "a")

			_, err := ReplaceGroup(context.Background(), root, tt.names, Options{})
			require.ErrorIs(t, err, ErrLinkMismatch)
			if tt.twice {
				require.Contains(t, err.Error(), "the same name was given twice")
			}
			for _, rel := range []string{"a", "b", "c"} {
				requireUntouched(t, root, rel, before)
			}
			requireNoTemps(t, dir)
		})
	}
}

func TestReplaceGroupDetectsChangesBeforeSwap(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("shared"), 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
	before := mustLstat(t, root, "a")
	time.Sleep(10 * time.Millisecond)
	setSwapHook(t, func() {
		require.NoError(t, os.Remove(filepath.Join(dir, "b")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "b"), []byte("shared"), 0o644))
	})

	_, err := ReplaceGroup(context.Background(), root, []string{"a", "b"}, Options{})
	require.ErrorIs(t, err, ErrModified)
	require.Equal(t, before.ID, mustLstat(t, root, "a").ID)
	requireNoTemps(t, dir)
}

// If a hardlink group can only be switched part-way, every name must still have the full content
// and no temporary names may be left behind.
func TestReplaceGroupStoppedPartWay(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory used to make the link fail")
	}
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "locked"), 0o755))
	content := []byte("shared content")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), content, 0o644))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "sub", "b")))
	require.NoError(t, os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "locked", "c")))
	require.NoError(t, os.Chmod(filepath.Join(dir, "locked"), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "locked"), 0o755) })
	before := mustLstat(t, root, "a")

	_, err := ReplaceGroup(context.Background(), root, []string{"a", "sub/b", "locked/c"}, Options{})
	require.ErrorIs(t, err, fs.ErrPermission)
	require.Equal(t, "couldn't link the new copy to the file's other names (permission denied) — "+
		"1 of its 3 hardlinked names was switched to the new, identical copy; the others still use the original", err.Error())
	require.True(t, LeftSplit(err), "a part-switched group must be reported as split")

	b := mustLstat(t, root, "sub/b")
	require.NotEqual(t, before.ID, b.ID, "sub/b was switched first")
	require.Equal(t, uint64(1), b.Nlink, "the temporary name must be gone")
	require.Equal(t, before.ID, mustLstat(t, root, "a").ID)
	require.Equal(t, before.ID, mustLstat(t, root, "locked/c").ID)
	for _, rel := range []string{"a", "sub/b", "locked/c"} {
		requireContent(t, filepath.Join(dir, filepath.FromSlash(rel)), content)
	}
	requireNoTemps(t, dir)
}

func TestReplaceInPlaceRootKeepsOwnerAndSetuid(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\necho hi\n"), 0o755))
	require.NoError(t, os.Chown(name, 65534, 65534))
	require.NoError(t, os.Chmod(name, 0o755|os.ModeSetuid|os.ModeSetgid))
	// trusted.* attributes are only visible to root.
	trusted := runtime.GOOS == "linux" && unix.Setxattr(name, "trusted.test", []byte("root only"), 0) == nil
	before := mustLstat(t, root, "f")
	require.Equal(t, uint32(65534), before.UID)
	require.NotZero(t, before.Mode&os.ModeSetuid)

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	after := mustLstat(t, root, "f")
	require.NotEqual(t, before.ID, after.ID)
	require.Equal(t, uint32(65534), after.UID)
	require.Equal(t, uint32(65534), after.GID)
	require.Equal(t, before.Mode, after.Mode)
	require.NotZero(t, after.Mode&os.ModeSetuid)
	require.NotZero(t, after.Mode&os.ModeSetgid)
	require.True(t, before.Mtime.Equal(after.Mtime))
	if trusted {
		buf := make([]byte, 64)
		n, err := unix.Getxattr(name, "trusted.test", buf)
		require.NoError(t, err)
		require.Equal(t, "root only", string(buf[:n]))
	}
}
