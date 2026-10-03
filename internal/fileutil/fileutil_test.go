package fileutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// testDirEnv names a directory to run the rewrite tests in instead of the system's temporary
// directory, for example a folder on a ZFS dataset, so that ZFS-only behaviour (DOS attributes,
// NFSv4 ACLs, project IDs) is tested too.
const testDirEnv = "REBALANCE_TEST_DIR"

// testDir returns a fresh directory in the one named by testDirEnv, or else in the system's
// temporary directory, removed when the test ends.
func testDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testDirEnv)
	if base == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(base, "fileutil-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestIsTempName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{".zfs-rebalance.0123456789ab.tmp", true},
		{".zfs-rebalance.ffffffffffff.tmp", true},
		{".zfs-rebalance.0123456789AB.tmp", false}, // uppercase is not ours
		{".zfs-rebalance.0123456789a.tmp", false},  // too short
		{".zfs-rebalance.0123456789abc.tmp", false},
		{".zfs-rebalance.0123456789ag.tmp", false},
		{".zfs-rebalance.0123456789ab.tmpx", false},
		{"x.zfs-rebalance.0123456789ab.tmp", false},
		{"zfs-rebalance.0123456789ab.tmp", false},
		{"photo.jpg.balance", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsTempName(tt.name))
		})
	}
	for range 100 {
		name := randomTempName()
		require.Len(t, name, 31)
		require.True(t, IsTempName(name), name)
	}
}

func TestParseChecksumType(t *testing.T) {
	tests := []struct {
		in      string
		want    ChecksumType
		wantErr bool
	}{
		{"sha256", ChecksumSHA256, false},
		{"SHA256", ChecksumSHA256, false},
		{" Sha256 ", ChecksumSHA256, false},
		{"md5", ChecksumMD5, false},
		{"MD5", ChecksumMD5, false},
		{"", "", true},
		{"sha1", "", true},
		{"sha-256", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseChecksumType(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "use sha256 (the default) or md5")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// The copy loop must never reach copy_file_range or sendfile (which would clone blocks instead of
// rewriting them), so the types it reads and writes through must not offer the io fast paths.
func TestCopyTypesHideFastPaths(t *testing.T) {
	f, err := os.Create(filepath.Join(testDir(t), "f"))
	require.NoError(t, err)
	defer f.Close()

	for _, v := range []any{plainReader{f}, plainWriter{f}} {
		_, isReaderFrom := v.(io.ReaderFrom)
		_, isWriterTo := v.(io.WriterTo)
		require.False(t, isReaderFrom, "%T implements io.ReaderFrom", v)
		require.False(t, isWriterTo, "%T implements io.WriterTo", v)
	}
}

func TestCopyData(t *testing.T) {
	data := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	zeros := func(n int) []byte { return make([]byte, n) }
	ones := func(n int) []byte { return bytes.Repeat([]byte{1}, n) }

	tests := []struct {
		name    string
		content []byte
		buf     int
	}{
		{"empty", nil, 1},
		{"one byte", []byte{7}, 2},
		{"all zero", zeros(3*zeroChunk + 5), defaultBufferSize},
		{"leading hole", data(zeros(2*zeroChunk), ones(10)), defaultBufferSize},
		{"trailing hole", data(ones(10), zeros(5*zeroChunk)), defaultBufferSize},
		{"holes between data", data(ones(zeroChunk), zeros(2*zeroChunk), ones(3), zeros(zeroChunk+1)), 2 * zeroChunk},
		{"zeros inside a chunk are written", data(ones(5), zeros(zeroChunk-10), ones(5)), defaultBufferSize},
		{"tiny buffer", data(ones(1000), zeros(5000), ones(1)), 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := testDir(t)
			src := writeTestFile(t, filepath.Join(dir, "src"), tt.content)
			dst, err := os.Create(filepath.Join(dir, "dst"))
			require.NoError(t, err)
			defer dst.Close()

			h := sha256.New()
			n, err := copyData(context.Background(), plainWriter{dst}, plainReader{src}, h, make([]byte, tt.buf), int64(len(tt.content)))
			require.NoError(t, err)
			require.Equal(t, int64(len(tt.content)), n)
			want := sha256.Sum256(tt.content)
			require.Equal(t, want[:], h.Sum(nil))

			got, err := os.ReadFile(dst.Name())
			require.NoError(t, err)
			require.Equal(t, len(tt.content), len(got))
			require.True(t, bytes.Equal(tt.content, got), "content differs")
		})
	}
}

func TestCopyDataStopsWhenSourceGrows(t *testing.T) {
	dir := testDir(t)
	src := writeTestFile(t, filepath.Join(dir, "src"), bytes.Repeat([]byte("x"), 100))
	dst, err := os.Create(filepath.Join(dir, "dst"))
	require.NoError(t, err)
	defer dst.Close()

	_, err = copyData(context.Background(), plainWriter{dst}, plainReader{src}, sha256.New(), make([]byte, 10), 50)
	require.ErrorIs(t, err, ErrModified)
}

func TestVerifyCopyDetectsMismatch(t *testing.T) {
	content := []byte("hello, world")
	f := writeTestFile(t, filepath.Join(testDir(t), "f"), content)
	sum := sha256.Sum256(content)
	buf := make([]byte, 4)

	require.NoError(t, verifyCopy(context.Background(), f, buf, sha256.New(), sum[:], int64(len(content))))

	bad := sha256.Sum256([]byte("something else"))
	err := verifyCopy(context.Background(), f, buf, sha256.New(), bad[:], int64(len(content)))
	require.ErrorIs(t, err, ErrChecksum)

	err = verifyCopy(context.Background(), f, buf, sha256.New(), sum[:], int64(len(content))+1)
	require.ErrorIs(t, err, ErrChecksum)
}

func TestErrorMessages(t *testing.T) {
	require.Equal(t, "couldn't keep the file's owner or group — nothing was changed", ErrOwnership.Error())
	require.Equal(t, "another program or another rebalance run is working on it, so it was left alone — nothing was changed", ErrBusy.Error())

	for _, sentinel := range []error{ErrModified, ErrNoSpace, ErrOwnership, ErrMetadata, ErrProjectID, ErrChecksum,
		ErrNotRegular, ErrLinkMismatch, ErrImmutable, ErrUndeletable, ErrBusy, ErrUnsupportedPlatform} {
		require.Regexp(t, `^[a-z].* — nothing was changed$`, sentinel.Error())
	}
	// The more specific kinds also match the broader ones, but not the other way round.
	require.ErrorIs(t, ErrProjectID, ErrMetadata)
	require.ErrorIs(t, ErrUndeletable, ErrImmutable)
	require.NotErrorIs(t, ErrMetadata, ErrProjectID)
	require.NotErrorIs(t, ErrImmutable, ErrUndeletable)
	require.NotErrorIs(t, ErrBusy, ErrModified)

	// Only root can give a file away, so only a run without root is told to try sudo.
	ownerMessage := "couldn't keep the file's owner or group (try running with sudo) — nothing was changed"
	if os.Geteuid() == 0 {
		ownerMessage = "couldn't keep the file's owner or group (the filesystem refused, even for root: operation not permitted) — nothing was changed"
	}

	pathErr := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "write", Path: "dir/.zfs-rebalance.0123456789ab.tmp", Err: errno}
	}

	tests := []struct {
		name    string
		err     error
		is      []error
		isNot   []error
		message string
		split   bool
	}{
		{
			name:    "no space",
			err:     fail("couldn't write the new copy", pathErr(syscall.ENOSPC)),
			is:      []error{ErrNoSpace, syscall.ENOSPC},
			isNot:   []error{syscall.EDQUOT},
			message: "not enough free space — nothing was changed",
		},
		{
			name:    "quota",
			err:     fail("couldn't write the new copy", pathErr(syscall.EDQUOT)),
			is:      []error{ErrNoSpace, syscall.EDQUOT},
			isNot:   []error{syscall.ENOSPC},
			message: "not enough free space (a quota was reached) — nothing was changed",
		},
		{
			name:    "no space while setting metadata",
			err:     failKind(ErrMetadata, "extended attribute user.x", pathErr(syscall.ENOSPC)),
			is:      []error{ErrNoSpace, syscall.ENOSPC},
			isNot:   []error{ErrMetadata, syscall.EDQUOT},
			message: "not enough free space — nothing was changed",
		},
		{
			name:    "quota while setting metadata",
			err:     failKind(ErrMetadata, "extended attribute user.x", pathErr(syscall.EDQUOT)),
			is:      []error{ErrNoSpace, syscall.EDQUOT},
			isNot:   []error{ErrMetadata, syscall.ENOSPC},
			message: "not enough free space (a quota was reached) — nothing was changed",
		},
		{
			name:    "owner over quota",
			err:     ownershipErr(&os.SyscallError{Syscall: "fchown", Err: syscall.EDQUOT}),
			is:      []error{ErrNoSpace, syscall.EDQUOT},
			isNot:   []error{ErrOwnership, syscall.ENOSPC},
			message: "not enough free space (its owner or group is over their quota) — nothing was changed",
		},
		{
			name:    "owner can't be kept",
			err:     ownershipErr(&os.SyscallError{Syscall: "fchown", Err: syscall.EPERM}),
			is:      []error{ErrOwnership, syscall.EPERM},
			isNot:   []error{ErrNoSpace},
			message: ownerMessage,
		},
		{
			name:    "project ID its folder won't take",
			err:     failKind(ErrProjectID, "", &os.LinkError{Op: "renameat", Old: "a", New: "b", Err: syscall.EXDEV}),
			is:      []error{ErrProjectID, ErrMetadata, syscall.EXDEV},
			message: "its project ID (used for quotas) is different from its folder's, and that folder only accepts files with its own, so it was left alone — nothing was changed",
		},
		{
			name:    "undeletable",
			err:     failKind(ErrUndeletable, "", nil),
			is:      []error{ErrUndeletable, ErrImmutable},
			message: "it's protected from being deleted (" + undeletableHint + "), so it was left alone — nothing was changed",
		},
		{
			name:    "own wording for a kind",
			err:     &failure{kind: ErrImmutable.(*sentinel), what: "it's protected from being deleted, so it was left alone"},
			is:      []error{ErrImmutable},
			message: "it's protected from being deleted, so it was left alone — nothing was changed",
		},
		{
			name:    "plain I/O error hides the temporary path",
			err:     fail("couldn't read the file", pathErr(syscall.EIO)),
			is:      []error{syscall.EIO},
			message: "couldn't read the file (input/output error) — nothing was changed",
		},
		{
			name:    "ownership",
			err:     failKind(ErrOwnership, "", pathErr(syscall.EPERM)),
			is:      []error{ErrOwnership, syscall.EPERM},
			message: "couldn't keep the file's owner or group — nothing was changed",
		},
		{
			name:    "cancelled",
			err:     fail("", fmt.Errorf("copying: %w", context.Canceled)),
			is:      []error{context.Canceled},
			message: "stopped before the copy was finished — nothing was changed",
		},
		{
			name:    "already wrapped",
			err:     fail("couldn't read the file", failKind(ErrModified, "", nil)),
			is:      []error{ErrModified},
			message: "the file changed while it was being copied — nothing was changed",
		},
		{
			name:    "hardlink group stopped part-way",
			err:     &failure{kind: ErrModified.(*sentinel), swapped: 2, names: 3},
			is:      []error{ErrModified},
			split:   true,
			message: "the file changed while it was being copied — 2 of its 3 hardlinked names were switched to the new, identical copy; the others still use the original",
		},
		{
			name:    "hardlink group stopped after one name",
			err:     &failure{kind: ErrLinkMismatch.(*sentinel), swapped: 1, names: 4},
			is:      []error{ErrLinkMismatch},
			split:   true,
			message: "the file's hardlinks don't match what was expected — 1 of its 4 hardlinked names was switched to the new, identical copy; the others still use the original",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, target := range tt.is {
				require.ErrorIs(t, tt.err, target)
			}
			for _, target := range tt.isNot {
				require.NotErrorIs(t, tt.err, target)
			}
			require.Equal(t, tt.message, tt.err.Error())
			require.Equal(t, tt.split, LeftSplit(tt.err), "LeftSplit")
		})
	}
}

func writeTestFile(t *testing.T, name string, content []byte) *os.File {
	t.Helper()
	require.NoError(t, os.WriteFile(name, content, 0o644))
	f, err := os.Open(name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}
