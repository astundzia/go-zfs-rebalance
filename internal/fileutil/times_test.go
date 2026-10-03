//go:build linux || darwin

package fileutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SetTimes works through any open descriptor, read-only ones and folders included, and keeps as
// much precision as the filesystem does.
func TestSetTimes(t *testing.T) {
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	for _, rel := range []string{"f", "ref"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(rel), 0o644))
	}
	atime := time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.UTC)
	mtime := time.Date(2010, 11, 12, 13, 14, 15, 987654321, time.UTC)
	// The same times set by name show what this filesystem keeps of them.
	require.NoError(t, os.Chtimes(filepath.Join(dir, "ref"), atime, mtime))
	want := mustLstat(t, root, "ref")

	for _, rel := range []string{"f", "sub"} {
		f, err := root.Open(rel)
		require.NoError(t, err)
		require.NoError(t, SetTimes(f, atime, mtime))
		require.NoError(t, f.Close())
		got := mustLstat(t, root, rel)
		require.True(t, want.Mtime.Equal(got.Mtime), "%s: mtime %v, want %v", rel, got.Mtime, want.Mtime)
		require.True(t, want.Atime.Equal(got.Atime), "%s: atime %v, want %v", rel, got.Atime, want.Atime)
	}
}

// The copy's times are set through its open descriptor. Someone who can write to the folder may
// swap the copy's name for a symlink while the data is copied; the file it points to must keep its
// own times, and the swap must not happen.
func TestReplaceInPlaceSetsTimesOnTheCopyNotItsName(t *testing.T) {
	dir, root := newRoot(t)
	doc := filepath.Join(dir, "doc")
	require.NoError(t, os.WriteFile(doc, randomBytes(t, 64<<10), 0o644))
	backdated := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(doc, backdated, backdated))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ledger"), []byte("someone else's"), 0o644))
	docBefore := mustLstat(t, root, "doc")
	ledgerBefore := mustLstat(t, root, "ledger")

	setPrepareHook(t, func(tmpRel string) {
		require.NoError(t, os.Rename(filepath.Join(dir, tmpRel), filepath.Join(dir, "moved-away")))
		require.NoError(t, os.Symlink("ledger", filepath.Join(dir, tmpRel)))
	})
	_, err := ReplaceInPlace(context.Background(), root, "doc", Options{})
	require.ErrorIs(t, err, ErrModified)
	require.Contains(t, err.Error(), tempGone)

	ledgerAfter := mustLstat(t, root, "ledger")
	require.True(t, ledgerBefore.Mtime.Equal(ledgerAfter.Mtime), "the symlink's target got mtime %v", ledgerAfter.Mtime)
	require.True(t, ledgerBefore.Atime.Equal(ledgerAfter.Atime), "the symlink's target got atime %v", ledgerAfter.Atime)
	requireUntouched(t, root, "doc", docBefore)
	requireContent(t, filepath.Join(dir, "ledger"), []byte("someone else's"))
	requireNoTemps(t, dir)
}

// Reading the hidden copy between setting its times and the swap updates its access time (an
// indexer or virus scanner might). The new file must still end up with the original's times.
func TestReplaceInPlaceKeepsAtimeWhenTheCopyIsRead(t *testing.T) {
	for _, how := range []string{"read", "access time changed"} {
		t.Run(how, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, randomBytes(t, 64<<10), 0o644))
			setOldAtime(t, name)
			before := mustLstat(t, root, "f")

			setSwapHook(t, func() {
				tmp := filepath.Join(dir, findTemp(t, dir))
				if how == "read" {
					_, err := os.ReadFile(tmp)
					require.NoError(t, err)
					return
				}
				require.NoError(t, os.Chtimes(tmp, time.Now(), before.Mtime))
			})
			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.NoError(t, err)

			after := mustLstat(t, root, "f")
			require.NotEqual(t, before.ID, after.ID)
			require.True(t, before.Atime.Equal(after.Atime), "atime %v, want %v", after.Atime, before.Atime)
			require.True(t, before.Mtime.Equal(after.Mtime), "mtime %v, want %v", after.Mtime, before.Mtime)
		})
	}
}

// Something that writes to the hidden copy after it was checked makes it differ from the original,
// so it must never be swapped in.
func TestReplaceInPlaceRefusesCopyWrittenBeforeSwap(t *testing.T) {
	for _, how := range []string{"appended", "overwritten"} {
		t.Run(how, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			content := []byte("original content")
			require.NoError(t, os.WriteFile(name, content, 0o644))
			old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			require.NoError(t, os.Chtimes(name, old, old)) // so any write gives the copy a new mtime
			before := mustLstat(t, root, "f")

			setSwapHook(t, func() {
				f, err := os.OpenFile(filepath.Join(dir, findTemp(t, dir)), os.O_WRONLY, 0)
				require.NoError(t, err)
				if how == "appended" {
					_, err = f.WriteAt([]byte(" and more"), int64(len(content)))
				} else {
					_, err = f.WriteAt([]byte("O"), 0)
				}
				require.NoError(t, err)
				require.NoError(t, f.Close())
			})
			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.ErrorIs(t, err, ErrModified)
			require.Equal(t, "the file changed while it was being copied (its temporary copy was changed by another program) — nothing was changed", err.Error())
			requireUntouched(t, root, "f", before)
			requireContent(t, name, content)
			requireNoTemps(t, dir)
		})
	}
}
