package rebalance

import (
	"os"
	"runtime"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

// Why a file is left alone when the run doesn't have root.
const (
	reasonOtherOwner = "owned by someone else (run with sudo to include it)"
	reasonOtherGroup = "its group is one you're not in (run with sudo to include it)"
)

// account is who the run works as. Only root can give a copy someone else's owner, or a group
// it isn't in, so without root such files are skipped before anything is read or copied. (Some
// filesystems would let the copy be given away, for example a ZFS share whose ACL grants
// write_owner, but the run could then no longer finish setting it up.)
type account struct {
	root   bool
	uid    uint32
	groups map[uint32]bool // the effective group and every supplementary group
}

func currentAccount() account {
	euid := os.Geteuid()
	if euid == 0 {
		return account{root: true}
	}
	a := account{uid: uint32(euid), groups: map[uint32]bool{uint32(os.Getegid()): true}}
	if gids, err := os.Getgroups(); err == nil {
		for _, g := range gids {
			a.groups[uint32(g)] = true
		}
	}
	return a
}

// cantRewrite returns why a file whose status is file, in the folder whose status is dir, can't be
// rewritten without root (SkipOwner or SkipGroup, with a short explanation), or "" if it can be. A
// new file takes its folder's group on macOS, and on Linux when the folder has the setgid bit, so a
// file with that group needs no change of group.
func (a account) cantRewrite(file, dir fileutil.Info) (SkipReason, string) {
	switch {
	case a.root:
		return "", ""
	case file.UID != a.uid:
		return SkipOwner, reasonOtherOwner
	case a.groups[file.GID]:
		return "", ""
	case dir.Mode.IsDir() && file.GID == dir.GID && (runtime.GOOS == "darwin" || dir.Mode&os.ModeSetgid != 0):
		return "", ""
	}
	return SkipGroup, reasonOtherGroup
}
