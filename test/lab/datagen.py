#!/usr/bin/env python3
"""Generate a deliberately awkward test tree for the rebalance e2e tests (run as root).

  datagen.py <root> <size_mb> [--acl posix|nfs4|none] [--flags] [--outside DIR] [--skip-out FILE]
  datagen.py --clear-flags <root>

Writes incompressible data so ZFS compression can't hide allocation. Users 'alice' and 'bob' must exist.
--acl nfs4 works on TrueNAS SMB-style datasets (aclmode=restricted), where chmod is refused while a file
  has a non-trivial ACL: the ACL is stripped first, then the mode set, then the test ACEs added.
--flags (Linux) adds files with inode flags (nodump, immutable, append-only), ZFS project IDs and, on
  ZFS, DOS attributes, plus a file with a capability (security.capability) and one with a trusted.*
  xattr, which must be rewritten with both kept. Immutable, append-only and no-unlink files can't be
  deleted until `datagen.py --clear-flags <root>` has been run.
--outside DIR creates a hardlink partner outside <root> (that group must be left alone).
--skip-out FILE receives the relative paths a default run is expected to leave alone: hardlinked files,
  .balance files with no original next to them, immutable, append-only and no-unlink files, the file
  whose project ID differs from its +P folder's, and the setuid file with an NFSv4 ACE.
"""
import argparse
import fcntl
import os
import pwd
import random
import shutil
import struct
import subprocess
import sys

rng = random.Random(42)
NS = 1_000_000_000

# Linux ioctls (the same numbers on amd64 and arm64).
FS_IOC_GETFLAGS, FS_IOC_SETFLAGS = 0x80086601, 0x40086602
FS_IMMUTABLE_FL, FS_APPEND_FL = 0x10, 0x20
# OpenZFS include/sys/fs/zfs.h: ZFS_IOC_GETDOSFLAGS/SETDOSFLAGS = _IOR/_IOW(0x83, 1/2, uint64_t).
ZFS_IOC_GETDOSFLAGS, ZFS_IOC_SETDOSFLAGS = 0x80088301, 0x40088302
ZFS_READONLY, ZFS_HIDDEN, ZFS_SYSTEM, ZFS_ARCHIVE = 1 << 32, 1 << 33, 1 << 34, 1 << 35
ZFS_IMMUTABLE, ZFS_NOUNLINK, ZFS_APPENDONLY = 1 << 36, 1 << 37, 1 << 38
# linux/capability.h: a version 2 security.capability value (struct vfs_cap_data) is a magic word
# with the effective flag, then permitted and inheritable masks for capabilities 0-31 and 32-63.
VFS_CAP_REVISION_2, VFS_CAP_FLAGS_EFFECTIVE = 0x02000000, 0x000001
CAP_NET_RAW = 13


def capability_xattr(cap):
    """The security.capability value `setcap <cap>=ep` writes, for a capability below 32."""
    return struct.pack("<5I", VFS_CAP_REVISION_2 | VFS_CAP_FLAGS_EFFECTIVE, 1 << cap, 0, 0, 0)


def write_random(path, size):
    with open(path, "wb") as f:
        left = size
        while left:
            n = min(left, 4 << 20)
            f.write(os.urandom(n))
            left -= n


def stamp(path, base_s):
    # distinct, nanosecond-precision atime and mtime in the past
    atime = base_s * NS + rng.randrange(NS)
    mtime = (base_s - 86400) * NS + rng.randrange(NS)
    os.utime(path, ns=(atime, mtime), follow_symlinks=False)


def nfs4_acl(*args):
    # TrueNAS's tool: nfs4xdr_setfacl -a ACE [index] FILE, or -b FILE to go back to a trivial ACL.
    subprocess.run(["nfs4xdr_setfacl", *args], check=True, stdout=subprocess.DEVNULL)


def set_mode(path, mode, acl):
    """chmod that also works where aclmode=restricted refuses it on a file with a non-trivial ACL
    (for example one inherited from its folder): strip the ACL to a trivial one, then retry."""
    try:
        os.chmod(path, mode)
    except PermissionError:
        if acl != "nfs4" or not shutil.which("nfs4xdr_setfacl"):
            raise
        nfs4_acl("-b", path)
        os.chmod(path, mode)


def ioctl_u64(path, request, value=0):
    """Runs a get/set ioctl that takes a uint64 on path; returns the value, or None if the
    filesystem doesn't support it."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        buf = bytearray(struct.pack("Q", value))
        fcntl.ioctl(fd, request, buf)
        return struct.unpack("Q", buf)[0]
    except OSError:
        return None
    finally:
        os.close(fd)


def make_flag_files(root, skip):
    """Files whose inode flags, project IDs, DOS attributes, capabilities and trusted.* xattrs
    rebalance must keep (or, for the ones it can't replace, leave alone; those go in skip).
    Returns the changes to make once timestamps are set, since an immutable or append-only file
    refuses new times."""
    if not sys.platform.startswith("linux"):
        sys.exit("--flags needs Linux (chattr and the ZFS ioctls)")
    fl = f"{root}/flags"
    os.makedirs(f"{fl}/projdir", exist_ok=True)
    later = []

    def chattr(*args):
        subprocess.run(["chattr", *args], check=True)

    write_random(f"{fl}/nodump.bin", 200_000)
    chattr("+d", f"{fl}/nodump.bin")
    write_random(f"{fl}/project.bin", 200_000)
    chattr("-p", "4242", f"{fl}/project.bin")
    # A folder whose new files inherit project 777, holding one that inherited it (rewritten,
    # keeping 777) and one moved to 888 afterwards. ZFS and Linux refuse to rename a file into a +P
    # folder unless it has the folder's ID, so a copy can never take own-id.bin's place: rebalance
    # skips it and leaves it untouched with 888.
    chattr("+P", "-p", "777", f"{fl}/projdir")
    write_random(f"{fl}/projdir/inherits.bin", 100_000)
    write_random(f"{fl}/projdir/own-id.bin", 100_000)
    chattr("-p", "888", f"{fl}/projdir/own-id.bin")
    skip.append("flags/projdir/own-id.bin")
    for name, flag in [("immutable.bin", "+i"), ("appendonly.bin", "+a")]:
        write_random(f"{fl}/{name}", 100_000)
        later.append(lambda p=f"{fl}/{name}", f=flag: chattr(f, p))
        skip.append(f"flags/{name}")
    make_xattr_files(fl)

    # DOS attributes exist only on ZFS (OpenZFS 2.2+ on Linux); elsewhere they're simply not made.
    probe = f"{fl}/dos-hidden.bin"
    write_random(probe, 100_000)
    if ioctl_u64(probe, ZFS_IOC_GETDOSFLAGS) is None:
        print("note: no ZFS DOS attributes here, so the dos-* files have none")
    else:
        for name, flags in [("dos-hidden.bin", ZFS_HIDDEN | ZFS_ARCHIVE | ZFS_SYSTEM),
                            ("dos-readonly.bin", ZFS_READONLY | ZFS_ARCHIVE),
                            ("dos-nounlink.bin", ZFS_NOUNLINK)]:
            p = f"{fl}/{name}"
            if not os.path.exists(p):
                write_random(p, 100_000)
            later.append(lambda p=p, f=flags: ioctl_u64(p, ZFS_IOC_SETDOSFLAGS, f))
        skip.append("flags/dos-nounlink.bin")
    return later


def make_xattr_files(fl):
    """A file with a capability, as `setcap cap_net_raw=ep` sets it, and one with a trusted.* xattr,
    which only root can see or set. A root run must rewrite both and keep those xattrs."""
    cap = f"{fl}/capability.bin"
    write_random(cap, 100_000)
    os.chmod(cap, 0o755)
    try:
        if shutil.which("setcap"):
            subprocess.run(["setcap", "cap_net_raw=ep", cap], check=True)
        else:
            os.setxattr(cap, "security.capability", capability_xattr(CAP_NET_RAW), follow_symlinks=False)
    except (OSError, subprocess.CalledProcessError) as e:
        print(f"note: couldn't give {cap} a capability ({e})")

    trusted = f"{fl}/trusted.bin"
    write_random(trusted, 100_000)
    if os.geteuid() != 0:
        print(f"note: only root can set trusted.* xattrs, so {trusted} has none")
        return
    try:
        os.setxattr(trusted, "trusted.lab", b"only root can see this", follow_symlinks=False)
    except OSError as e:
        print(f"note: couldn't give {trusted} a trusted.* xattr ({e.strerror})")


def clear_flags(root):
    """Removes the immutable, append-only and no-unlink markers --flags set, so the tree can be
    deleted."""
    for dirpath, _, filenames in os.walk(root):
        for n in filenames:
            p = os.path.join(dirpath, n)
            if os.path.islink(p) or not os.path.isfile(p):
                continue
            flags = ioctl_u64(p, FS_IOC_GETFLAGS)
            if flags is not None and flags & (FS_IMMUTABLE_FL | FS_APPEND_FL):
                ioctl_u64(p, FS_IOC_SETFLAGS, flags & ~(FS_IMMUTABLE_FL | FS_APPEND_FL))
            dos = ioctl_u64(p, ZFS_IOC_GETDOSFLAGS)
            if dos is not None and dos & (ZFS_IMMUTABLE | ZFS_NOUNLINK | ZFS_APPENDONLY | ZFS_READONLY):
                ioctl_u64(p, ZFS_IOC_SETDOSFLAGS,
                          dos & ~(ZFS_IMMUTABLE | ZFS_NOUNLINK | ZFS_APPENDONLY | ZFS_READONLY))
    print(f"cleared immutable, append-only, no-unlink and read-only markers under {root}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root")
    ap.add_argument("size_mb", type=int, nargs="?")
    ap.add_argument("--acl", default="none", choices=["posix", "nfs4", "none"])
    ap.add_argument("--flags", action="store_true")
    ap.add_argument("--clear-flags", action="store_true")
    ap.add_argument("--outside")
    ap.add_argument("--skip-out")
    a = ap.parse_args()
    root = a.root
    if a.clear_flags:
        clear_flags(root)
        return
    if a.size_mb is None:
        ap.error("size_mb is required")
    alice, bob = pwd.getpwnam("alice"), pwd.getpwnam("bob")
    skip = []
    os.makedirs(root, exist_ok=True)

    # big incompressible files: ~90% of the budget, 8-256 MiB each
    os.makedirs(f"{root}/big", exist_ok=True)
    budget, i = a.size_mb * 9 // 10 << 20, 0
    while budget > 0:
        size = min(budget, rng.choice([8, 16, 32, 64, 128, 256]) << 20)
        write_random(f"{root}/big/blob{i:03d}.bin", size)
        budget -= size
        i += 1

    # many small files
    for d in range(10):
        os.makedirs(f"{root}/small/d{d}", exist_ok=True)
        for j in range(60):
            write_random(f"{root}/small/d{d}/f{j:02d}.dat", rng.randrange(1, 64 << 10))

    # awkward names and special cases
    s = f"{root}/special"
    os.makedirs(s, exist_ok=True)
    for name in ["with space.txt", "Meeting at noon.mp4", "colon: here.txt", "newline\nname.txt",
                 "escape\x1b[31mred.txt", "ünïcødé-文件.txt", "x" * 240 + ".long"]:
        write_random(f"{s}/{name}", rng.randrange(1000, 200_000))
    open(f"{s}/empty", "wb").close()
    # Two files ending in .balance with no original next to them: version 2 can't tell a real
    # user file (ledger.balance, which v1 would have deleted) from a v1 leftover that may be the
    # only copy, so it must leave both untouched and warn about them.
    write_random(f"{s}/ledger.balance", 50_000)
    write_random(f"{s}/movie.mkv.balance", 3 << 20)
    skip += ["special/ledger.balance", "special/movie.mkv.balance"]
    with open(f"{s}/sparse.img", "wb") as f:              # 1 GiB apparent, 12 MiB real
        for off in (0, 400 << 20, (1 << 30) - (4 << 20)):
            f.seek(off)
            f.write(os.urandom(4 << 20))
        f.truncate(1 << 30)
    os.symlink("with space.txt", f"{s}/link-to-file")
    os.symlink("/etc/passwd", f"{s}/link-outside")
    os.mkfifo(f"{s}/a-fifo")

    # owners, modes, setuid/setgid
    o = f"{root}/owners"
    os.makedirs(o, exist_ok=True)
    for name, user, mode in [("alice-private", alice, 0o600), ("bob-shared", bob, 0o640),
                             ("alice-setuid", alice, 0o4755), ("bob-setgid", bob, 0o2755),
                             ("sticky-file", alice, 0o1644)]:
        p = f"{o}/{name}"
        write_random(p, rng.randrange(10_000, 2 << 20))
        os.chown(p, user.pw_uid, user.pw_gid)
        set_mode(p, mode, a.acl)
    os.chown(o, bob.pw_uid, bob.pw_gid)

    # xattrs
    for p in [f"{o}/alice-private", f"{s}/with space.txt", f"{root}/big/blob000.bin"]:
        os.setxattr(p, "user.comment", b"keep me", follow_symlinks=False)
        os.setxattr(p, "user.binary", bytes(range(256)), follow_symlinks=False)

    # ACLs
    if a.acl == "posix":
        subprocess.run(["setfacl", "-m", "u:alice:rw,g:bob:r", f"{o}/bob-shared"], check=True)
        subprocess.run(["setfacl", "-d", "-m", "u:alice:rwx", f"{root}/small/d0"], check=True)
        write_random(f"{root}/small/d0/inherits-default-acl.dat", 10_000)
    elif a.acl == "nfs4":
        if not shutil.which("nfs4xdr_setfacl"):
            sys.exit("--acl nfs4 needs nfs4xdr_setfacl (TrueNAS SCALE)")
        nfs4_acl("-a", "user:alice:rwxpDdaARWc--s:-------:allow", "0", f"{o}/bob-shared")
        # setuid plus an explicit ACE: on an aclmode=restricted dataset the copy can't get its
        # setuid bit back once the ACL is on it, so rebalance must skip it and leave it alone.
        p = f"{o}/alice-setuid-ace"
        write_random(p, 100_000)
        os.chown(p, alice.pw_uid, alice.pw_gid)
        set_mode(p, 0o4750, a.acl)
        nfs4_acl("-a", "user:bob:r-x---a-R-c---:-------:allow", "0", p)
        skip.append("owners/alice-setuid-ace")

    # hardlinks: a group fully inside the root, and one with a partner outside
    h = f"{root}/links"
    os.makedirs(h, exist_ok=True)
    write_random(f"{h}/group-a", 5 << 20)
    os.link(f"{h}/group-a", f"{h}/group-b")
    os.link(f"{h}/group-a", f"{root}/small/group-c")
    skip += ["links/group-a", "links/group-b", "small/group-c"]
    if a.outside:
        os.makedirs(a.outside, exist_ok=True)
        write_random(f"{h}/shared-outside", 3 << 20)
        os.link(f"{h}/shared-outside", f"{a.outside}/partner")
        skip.append("links/shared-outside")

    later = make_flag_files(root, skip) if a.flags else []

    # deep tree
    d = root
    for k in range(12):
        d = f"{d}/level{k}"
        os.makedirs(d, exist_ok=True)
        write_random(f"{d}/file{k}", 4096 * (k + 1))

    # timestamps: files first, then directories deepest-first so dir mtimes stick
    base = 1_600_000_000
    for dirpath, dirnames, filenames in os.walk(root):
        for n in filenames:
            p = os.path.join(dirpath, n)
            if not os.path.islink(p) and os.path.isfile(p):
                stamp(p, base + rng.randrange(10_000_000))
    for dirpath, _, _ in sorted(os.walk(root), key=lambda t: -t[0].count("/")):
        stamp(dirpath, base + rng.randrange(10_000_000))
    for change in later:  # flags that would refuse new times; they only change ctime
        change()

    if a.skip_out:
        open(a.skip_out, "w").write("\n".join(skip) + "\n")
    print(f"generated test tree in {root} ({a.size_mb} MiB budget, acl={a.acl}, flags={a.flags})")


if __name__ == "__main__":
    main()
