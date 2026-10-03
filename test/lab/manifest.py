#!/usr/bin/env python3
"""Snapshot and compare a directory tree for the rebalance e2e tests (run as root).

  manifest.py snap <root> <out.json>
  manifest.py diff <before.json> <after.json> [--expect-rewritten] [--allow-missing PATH ...]

A snapshot records, per entry: type, size, sha256 (regular files), mode, uid, gid, atime_ns, mtime_ns,
nlink, inode, symlink target, every xattr (including system.* ACL xattrs such as system.nfs4_acl_xdr and
system.posix_acl_access; trusted.* ones are only visible to root) and, for directories, mtime_ns. On Linux
it also records each file's and folder's inode flags (chattr: nodump, immutable, ...), project ID and
xflags, and ZFS DOS attributes, where the filesystem has them. It also lists leftover rebalance temp files.

diff exits 0 only if content and all metadata are identical. With --expect-rewritten it also requires that
every regular file got a new inode (proof that it was physically rewritten), except files listed in the
"skipped" set (hardlinks etc.) passed through --skip-file.
"""
import fcntl
import hashlib
import json
import os
import re
import stat
import struct
import sys

TEMP_RE = re.compile(r"^\.zfs-rebalance\.[0-9a-f]{12}\.tmp$")

# Linux ioctls (the same numbers on amd64 and arm64).
FS_IOC_GETFLAGS = 0x80086601      # _IOR('f', 1, long)
FS_IOC_FSGETXATTR = 0x801C581F    # _IOR('X', 31, struct fsxattr), 28 bytes
ZFS_IOC_GETDOSFLAGS = 0x80088301  # OpenZFS include/sys/fs/zfs.h: _IOR(0x83, 1, uint64_t)
# ZFS_DOS_FL_USER_VISIBLE: READONLY..NODUMP plus REPARSE, OFFLINE, SPARSE. ZFS also keeps
# bookkeeping bits (such as AV_MODIFIED, set by every write) that aren't part of the file's
# attributes.
ZFS_DOS_USER_VISIBLE = 0x38FF00000000


def xattrs(path):
    out = {}
    try:
        names = os.listxattr(path, follow_symlinks=False)
    except OSError:
        names = []
    probe = ["system.nfs4_acl_xdr", "system.posix_acl_access", "system.posix_acl_default"]
    for name in sorted(set(names) | set(probe)):
        try:
            out[name] = os.getxattr(path, name, follow_symlinks=False).hex()
        except OSError:
            pass
    return out


def inode_attrs(path):
    """Inode flags, project ID and xflags, and ZFS DOS attributes; None where unsupported."""
    out = {"flags": None, "xflags": None, "projid": None, "dosflags": None}
    if not sys.platform.startswith("linux"):
        return out
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError:
        return out
    try:
        for request, size in ((FS_IOC_GETFLAGS, 8), (FS_IOC_FSGETXATTR, 28), (ZFS_IOC_GETDOSFLAGS, 8)):
            buf = bytearray(size)
            try:
                fcntl.ioctl(fd, request, buf)
            except OSError:
                continue
            if request == FS_IOC_GETFLAGS:
                out["flags"] = struct.unpack_from("I", buf)[0]
            elif request == FS_IOC_FSGETXATTR:
                xflags, _, _, projid = struct.unpack_from("IIII", buf)
                out.update(xflags=xflags, projid=projid)
            else:
                out["dosflags"] = struct.unpack_from("Q", buf)[0] & ZFS_DOS_USER_VISIBLE
    finally:
        os.close(fd)
    return out


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def snap(root):
    entries, temps = {}, []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d != ".zfs"]
        for name in [*dirnames, *filenames]:
            p = os.path.join(dirpath, name)
            rel = os.path.relpath(p, root)
            if TEMP_RE.match(name):
                temps.append(rel)
                continue
            st = os.lstat(p)
            e = {
                "mode": oct(st.st_mode), "uid": st.st_uid, "gid": st.st_gid,
                "mtime_ns": st.st_mtime_ns, "nlink": st.st_nlink, "ino": st.st_ino,
                "xattrs": xattrs(p),
            }
            if stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode):
                e.update(inode_attrs(p))
            if stat.S_ISREG(st.st_mode):
                e.update(type="file", size=st.st_size, sha256=sha256(p), atime_ns=st.st_atime_ns,
                         blocks=st.st_blocks)
                # Hashing reads the file, which can bump atime; put it back so the snapshot doesn't
                # perturb what the next run (and the next snapshot) sees.
                os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns), follow_symlinks=False)
            elif stat.S_ISDIR(st.st_mode):
                e.update(type="dir")
            elif stat.S_ISLNK(st.st_mode):
                e.update(type="symlink", target=os.readlink(p))
            else:
                e.update(type="other")
            entries[rel] = e
    return {"root": os.path.abspath(root), "entries": entries, "temps": temps}


def diff(a, b, expect_rewritten, skip):
    problems = []
    ea, eb = a["entries"], b["entries"]
    for rel in sorted(set(ea) - set(eb)):
        problems.append(f"MISSING after: {rel!r}")
    for rel in sorted(set(eb) - set(ea)):
        problems.append(f"NEW after: {rel!r}")
    rewritten = kept = 0
    for rel in sorted(set(ea) & set(eb)):
        x, y = ea[rel], eb[rel]
        for k in ("type", "size", "sha256", "mode", "uid", "gid", "mtime_ns", "nlink", "target", "xattrs",
                  "flags", "xflags", "projid", "dosflags"):
            if x.get(k) != y.get(k):
                problems.append(f"{k} changed: {rel!r}: {x.get(k)!r} -> {y.get(k)!r}")
        if x["type"] == "file" and x.get("atime_ns") != y.get("atime_ns"):
            problems.append(f"atime changed: {rel!r}: {x['atime_ns']} -> {y['atime_ns']}")
        if x["type"] == "file":
            if x["ino"] != y["ino"]:
                rewritten += 1
            else:
                kept += 1
                if expect_rewritten and rel not in skip:
                    problems.append(f"NOT rewritten (same inode): {rel!r}")
    if b["temps"]:
        problems.append(f"leftover temp files: {b['temps']}")
    return problems, rewritten, kept


if __name__ == "__main__":
    if len(sys.argv) >= 4 and sys.argv[1] == "snap":
        json.dump(snap(sys.argv[2]), open(sys.argv[3], "w"))
        print(f"snapshot of {sys.argv[2]} -> {sys.argv[3]}")
    elif len(sys.argv) >= 4 and sys.argv[1] == "diff":
        skip = set()
        if "--skip-file" in sys.argv:
            skip = set(open(sys.argv[sys.argv.index("--skip-file") + 1]).read().splitlines())
        problems, rewritten, kept = diff(json.load(open(sys.argv[2])), json.load(open(sys.argv[3])),
                                         "--expect-rewritten" in sys.argv, skip)
        print(f"regular files rewritten (new inode): {rewritten}, unchanged inode: {kept}")
        for p in problems:
            print("  PROBLEM:", p)
        print("RESULT:", "IDENTICAL" if not problems else f"{len(problems)} PROBLEM(S)")
        sys.exit(1 if problems else 0)
    else:
        print(__doc__)
        sys.exit(2)
