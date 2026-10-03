//go:build linux

package fileutil

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestIoctlNumbers(t *testing.T) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "386", "arm", "riscv64", "loong64", "s390x":
	default:
		t.Skipf("%s encodes ioctl numbers differently", runtime.GOARCH)
	}
	// The values of _IOR('X', 31, struct fsxattr) etc. from the kernel and OpenZFS headers.
	require.Equal(t, uint32(0x801c581f), uint32(fsIocFSGetXattr))
	require.Equal(t, uint32(0x401c5820), uint32(fsIocFSSetXattr))
	require.Equal(t, uint32(0x80088301), uint32(zfsIocGetDOSFlags))
	require.Equal(t, uint32(0x40088302), uint32(zfsIocSetDOSFlags))
	require.Equal(t, fsxattrSize, int(unsafe.Sizeof(fsxattr{})))
}

// noexec reports whether dir is on a filesystem mounted noexec.
func noexec(t *testing.T, dir string) bool {
	t.Helper()
	var st unix.Statfs_t
	require.NoError(t, unix.Statfs(dir, &st))
	return st.Flags&unix.ST_NOEXEC != 0
}

// withFile runs fn on an open descriptor of name.
func withFile(t *testing.T, name string, fn func(fd int) error) error {
	t.Helper()
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()
	return withFd(f, fn)
}

func getFlags(t *testing.T, name string) uint32 {
	t.Helper()
	var flags uint32
	require.NoError(t, withFile(t, name, func(fd int) (err error) {
		flags, err = unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		return err
	}))
	return flags
}

// setFlags adds flags to name's chattr flags, skipping the test if the filesystem or the caller
// can't set them. The flags are cleared again when the test ends.
func setFlags(t *testing.T, name string, flags uint32) {
	t.Helper()
	err := withFile(t, name, func(fd int) error {
		cur, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		if err != nil {
			return err
		}
		return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(cur|flags))
	})
	if err != nil {
		t.Skipf("can't set chattr flags %#x here: %v", flags, err)
	}
	t.Cleanup(func() {
		_ = withFile(t, name, func(fd int) error {
			cur, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
			if err != nil {
				return err
			}
			return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(cur&^flags))
		})
	})
}

func TestReplaceInPlaceKeepsChattrFlags(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags uint32
	}{
		{"nodump", fsNodumpFl},
		{"noatime", fsNoatimeFl},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, []byte("flagged"), 0o644))
			setFlags(t, name, tt.flags)
			before := mustLstat(t, root, "f")

			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.NoError(t, err)

			require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
			require.NotZero(t, getFlags(t, name)&tt.flags, "the flag was lost")
		})
	}
}

// A folder can pass flags on to new files; the copy must not keep one the original didn't have.
func TestReplaceInPlaceDropsInheritedChattrFlags(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("plain"), 0o644))
	setFlags(t, dir, fsNodumpFl)
	probe := filepath.Join(dir, "probe")
	require.NoError(t, os.WriteFile(probe, nil, 0o644))
	if getFlags(t, probe)&fsNodumpFl == 0 {
		t.Skip("this filesystem doesn't pass nodump on to new files")
	}
	require.Zero(t, getFlags(t, name)&fsNodumpFl)

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)
	require.Zero(t, getFlags(t, name)&fsNodumpFl, "the folder's nodump flag stuck to the copy")
}

func TestReplaceInPlaceRootRefusesImmutable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to set immutable or append-only")
	}
	for _, tt := range []struct {
		name  string
		flags uint32
	}{
		{"immutable", fsImmutableFl},
		{"append-only", fsAppendFl},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, randomBytes(t, 64<<10), 0o644))
			setOldAtime(t, name)
			setFlags(t, name, tt.flags)
			before := mustLstat(t, root, "f")

			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.ErrorIs(t, err, ErrImmutable)
			require.Equal(t, "it's marked immutable or append-only (chattr +i / +a), so it was left alone — nothing was changed", err.Error())

			after := mustLstat(t, root, "f")
			require.True(t, before.Atime.Equal(after.Atime), "the file was read before it was refused")
			requireUntouched(t, root, "f", before)
			requireNoTemps(t, dir)
		})
	}
}

// The original must not have its access time changed by a rewrite that doesn't finish.
func TestReplaceInPlaceKeepsAtimeWhenStopped(t *testing.T) {
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, randomBytes(t, 256<<10), 0o644))
	setOldAtime(t, name)
	before := mustLstat(t, root, "f")

	ctx := &cancelAfter{Context: context.Background()}
	ctx.n.Store(20) // part-way through the copy
	_, err := ReplaceInPlace(ctx, root, "f", Options{BufferSize: 4096})
	require.ErrorIs(t, err, context.Canceled)

	after := mustLstat(t, root, "f")
	require.True(t, before.Atime.Equal(after.Atime), "atime %v -> %v", before.Atime, after.Atime)
	requireUntouched(t, root, "f", before)
	requireNoTemps(t, dir)
}

func getFsx(t *testing.T, name string) (fsxattr, bool) {
	t.Helper()
	var fsx fsxattr
	err := withFile(t, name, func(fd int) error { return ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&fsx)) })
	ok, err := supported(err)
	require.NoError(t, err)
	return fsx, ok
}

// setProject sets name's project ID (and, if inherit, project inheritance, as chattr +P does),
// skipping the test if the filesystem has no project quotas.
func setProject(t *testing.T, name string, projid uint32, inherit bool) {
	t.Helper()
	err := withFile(t, name, func(fd int) error {
		var fsx fsxattr
		if err := ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&fsx)); err != nil {
			return err
		}
		fsx.projid = projid
		if err := ioctlPtr(fd, fsIocFSSetXattr, unsafe.Pointer(&fsx)); err != nil || !inherit {
			return err
		}
		// ZFS and ext4 take project inheritance as an inode flag (OpenZFS 2.2 refuses
		// FS_XFLAG_PROJINHERIT in FS_IOC_FSSETXATTR); XFS only as the extended flag.
		flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
		if err == nil {
			err = unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags|fsProjinheritFl))
		}
		if err != nil {
			if err := ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&fsx)); err != nil {
				return err
			}
			fsx.xflags |= 0x200 // FS_XFLAG_PROJINHERIT
			if err := ioctlPtr(fd, fsIocFSSetXattr, unsafe.Pointer(&fsx)); err != nil {
				return err
			}
		}
		if err := ioctlPtr(fd, fsIocFSGetXattr, unsafe.Pointer(&fsx)); err != nil {
			return err
		}
		if fsx.xflags&projInheritX == 0 || fsx.projid != projid {
			return fmt.Errorf("project inheritance didn't stick (xflags %#x, project %d)", fsx.xflags, fsx.projid)
		}
		return nil
	})
	if err != nil {
		t.Skipf("can't set a project ID here (needs project quotas, for example ZFS with %s set): %v", testDirEnv, err)
	}
}

func TestReplaceInPlaceRootKeepsProjectID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setting project IDs is tested as root")
	}
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("project data"), 0o644))
	setProject(t, name, 4242, false)
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
	fsx, ok := getFsx(t, name)
	require.True(t, ok)
	require.Equal(t, uint32(4242), fsx.projid)
}

// In a folder that passes its project ID on to new files, a file with a different project ID can't
// be replaced without changing it, so it must be refused before anything is copied.
func TestReplaceInPlaceRootRefusesProjectIDItsFolderWontTake(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setting project IDs is tested as root")
	}
	dir, root := newRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "proj"), 0o755))
	name := filepath.Join(dir, "proj", "f")
	require.NoError(t, os.WriteFile(name, []byte("project data"), 0o644))
	setProject(t, filepath.Join(dir, "proj"), 777, true)
	setProject(t, name, 888, false)
	before := mustLstat(t, root, "proj/f")

	_, err := ReplaceInPlace(context.Background(), root, "proj/f", Options{})
	require.ErrorIs(t, err, ErrProjectID)
	require.ErrorIs(t, err, ErrMetadata)
	require.Equal(t, ErrProjectID.Error(), err.Error())
	requireUntouched(t, root, "proj/f", before)
	requireNoTemps(t, dir)
	fsx, _ := getFsx(t, name)
	require.Equal(t, uint32(888), fsx.projid)
}

// posixACL encodes a POSIX ACL as the kernel stores it in system.posix_acl_access/default.
func posixACL(entries ...[3]uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 2) // POSIX_ACL_XATTR_VERSION
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, uint16(e[0]))
		b = binary.LittleEndian.AppendUint16(b, uint16(e[1]))
		b = binary.LittleEndian.AppendUint32(b, e[2])
	}
	return b
}

const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclMask     = 0x10
	aclOther    = 0x20
	aclNoID     = 0xffffffff
)

// A folder's default ACL is applied to the new copy when it is created. It must be replaced by the
// original's own ACL (or none) before any data goes into the copy.
func TestReplaceInPlaceReplacesInheritedPOSIXACL(t *testing.T) {
	const access = "system.posix_acl_access"
	for _, tt := range []struct {
		name string
		own  []byte // the original's ACL, nil for none
	}{
		{"original without an ACL", nil},
		{"original with its own ACL", posixACL(
			[3]uint32{aclUserObj, 6, aclNoID}, [3]uint32{aclUser, 6, 12345},
			[3]uint32{aclGroupObj, 0, aclNoID}, [3]uint32{aclMask, 6, aclNoID}, [3]uint32{aclOther, 0, aclNoID})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, root := newRoot(t)
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, []byte("private"), 0o600))
			if tt.own != nil {
				if err := unix.Setxattr(name, access, tt.own, 0); err != nil {
					t.Skipf("this filesystem doesn't support POSIX ACLs: %v", err)
				}
			}
			inherited := posixACL(
				[3]uint32{aclUserObj, 7, aclNoID}, [3]uint32{aclUser, 4, 65534},
				[3]uint32{aclGroupObj, 5, aclNoID}, [3]uint32{aclMask, 7, aclNoID}, [3]uint32{aclOther, 0, aclNoID})
			if err := unix.Setxattr(dir, "system.posix_acl_default", inherited, 0); err != nil {
				t.Skipf("this filesystem doesn't support POSIX ACLs: %v", err)
			}
			want := xattrOf(t, name, access)
			require.Equal(t, tt.own != nil, want != nil)

			setPrepareHook(t, func(tmpRel string) {
				require.Equal(t, want, xattrOf(t, filepath.Join(dir, tmpRel), access),
					"the copy must have the original's ACL before data is written")
			})
			_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
			require.NoError(t, err)
			require.Equal(t, want, xattrOf(t, name, access))
		})
	}
}

func xattrOf(t *testing.T, name, attr string) []byte {
	t.Helper()
	buf := make([]byte, 1024)
	n, err := unix.Getxattr(name, attr, buf)
	if errors.Is(err, unix.ENODATA) {
		return nil
	}
	require.NoError(t, err)
	return buf[:n]
}

func TestReplaceInPlaceRootKeepsFileCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to set file capabilities")
	}
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chown(name, 65534, 65534))
	// struct vfs_cap_data, revision 2, effective, permitted = CAP_NET_RAW (13).
	caps := binary.LittleEndian.AppendUint32(nil, 0x02000001)
	caps = binary.LittleEndian.AppendUint32(caps, 1<<13)
	caps = append(caps, make([]byte, 12)...)
	if err := unix.Setxattr(name, "security.capability", caps, 0); err != nil {
		t.Skipf("can't set file capabilities here: %v", err)
	}
	want := xattrOf(t, name, "security.capability")
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.NoError(t, err)

	require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
	require.Equal(t, want, xattrOf(t, name, "security.capability"))
	require.Equal(t, uint32(65534), mustLstat(t, root, "f").UID)
}

// onZFS skips the test unless dir is on ZFS with DOS attributes (OpenZFS 2.2 or later).
func onZFS(t *testing.T, name string) {
	t.Helper()
	var st unix.Statfs_t
	require.NoError(t, unix.Statfs(name, &st))
	if st.Type != zfsSuperMagic {
		t.Skipf("needs ZFS: set %s to a folder on a ZFS dataset", testDirEnv)
	}
	var dos uint64
	err := withFile(t, name, func(fd int) error { return ioctlPtr(fd, zfsIocGetDOSFlags, unsafe.Pointer(&dos)) })
	if ok, err := supported(err); err != nil || !ok {
		t.Skipf("this ZFS has no DOS attributes (needs OpenZFS 2.2 or later): %v", err)
	}
}

func getDOS(t *testing.T, name string) uint64 {
	t.Helper()
	var dos uint64
	require.NoError(t, withFile(t, name, func(fd int) error { return ioctlPtr(fd, zfsIocGetDOSFlags, unsafe.Pointer(&dos)) }))
	return dos
}

func setDOS(t *testing.T, name string, dos uint64) {
	t.Helper()
	require.NoError(t, withFile(t, name, func(fd int) error { return ioctlPtr(fd, zfsIocSetDOSFlags, unsafe.Pointer(&dos)) }))
	t.Cleanup(func() {
		var none uint64
		_ = withFile(t, name, func(fd int) error { return ioctlPtr(fd, zfsIocSetDOSFlags, unsafe.Pointer(&none)) })
	})
}

// ZFS sets the archive attribute on a file whenever it is renamed or linked, so this also checks
// that the swap doesn't leave it set on a file that didn't have it.
func TestReplaceInPlaceKeepsZFSDOSAttributes(t *testing.T) {
	for _, names := range [][]string{{"f"}, {"f", "sub/g"}} {
		t.Run(fmt.Sprintf("%d names", len(names)), func(t *testing.T) {
			dir, root := newRoot(t)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
			name := filepath.Join(dir, "f")
			require.NoError(t, os.WriteFile(name, []byte("windows file"), 0o644))
			for _, other := range names[1:] {
				require.NoError(t, os.Link(name, filepath.Join(dir, filepath.FromSlash(other))))
			}
			onZFS(t, name)
			// Hidden, system and read-only, with archive cleared (as backup software leaves it).
			setDOS(t, name, zfsHidden|zfsSystem|zfsReadonly)
			want := getDOS(t, name)
			require.Zero(t, want&zfsArchive)
			before := mustLstat(t, root, "f")

			_, err := ReplaceGroup(context.Background(), root, names, Options{})
			require.NoError(t, err)

			require.NotEqual(t, before.ID, mustLstat(t, root, "f").ID)
			require.Equal(t, want, getDOS(t, name))
		})
	}
}

func TestReplaceInPlaceRootRefusesZFSNounlink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("tested as root")
	}
	dir, root := newRoot(t)
	name := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(name, []byte("keep me"), 0o644))
	onZFS(t, name)
	setDOS(t, name, zfsNounlink)
	before := mustLstat(t, root, "f")

	_, err := ReplaceInPlace(context.Background(), root, "f", Options{})
	require.ErrorIs(t, err, ErrUndeletable)
	require.Equal(t, "it's protected from being deleted (the ZFS nounlink attribute), so it was left alone — nothing was changed", err.Error())
	requireUntouched(t, root, "f", before)
	requireNoTemps(t, dir)
}

// Each kind of protection is reported as what it is, so the run's summary can count it correctly.
func TestProtectedKinds(t *testing.T) {
	tests := []struct {
		name  string
		attrs fileAttrs
		want  error // nil: not protected
		not   error
	}{
		{"plain", fileAttrs{hasFlags: true, flags: fsNodumpFl, hasDOS: true, dos: zfsArchive | zfsHidden}, nil, nil},
		{"immutable", fileAttrs{hasFlags: true, flags: fsImmutableFl}, ErrImmutable, ErrUndeletable},
		{"append-only", fileAttrs{hasFlags: true, flags: fsAppendFl}, ErrImmutable, ErrUndeletable},
		{"ZFS immutable", fileAttrs{hasDOS: true, dos: zfsImmutable}, ErrImmutable, ErrUndeletable},
		{"ZFS append-only", fileAttrs{hasDOS: true, dos: zfsAppendonly}, ErrImmutable, ErrUndeletable},
		{"ZFS nounlink", fileAttrs{hasDOS: true, dos: zfsNounlink}, ErrUndeletable, nil},
		{"flags not reported", fileAttrs{flags: fsImmutableFl, dos: zfsNounlink}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.attrs.protected()
			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
			require.ErrorIs(t, err, ErrImmutable)
			if tt.not != nil {
				require.NotErrorIs(t, err, tt.not)
			}
		})
	}
}
