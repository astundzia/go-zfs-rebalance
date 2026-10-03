#!/usr/bin/env python3
"""Generate a deliberately awkward test tree for the rebalance e2e tests (run as root).

  datagen.py <root> <size_mb> [--acl posix|nfs4|none] [--outside DIR] [--skip-out FILE]

Writes incompressible data so ZFS compression can't hide allocation. Users 'alice' and 'bob' must exist.
--outside DIR creates a hardlink partner outside <root> (that group must be left alone).
--skip-out FILE receives the relative paths a default run is expected to skip (hardlinked files).
"""
import argparse
import os
import pwd
import random
import shutil
import subprocess

rng = random.Random(42)
NS = 1_000_000_000


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root")
    ap.add_argument("size_mb", type=int)
    ap.add_argument("--acl", default="none", choices=["posix", "nfs4", "none"])
    ap.add_argument("--outside")
    ap.add_argument("--skip-out")
    a = ap.parse_args()
    root = a.root
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
    write_random(f"{s}/ledger.balance", 50_000)          # a real user file that v1 would delete
    write_random(f"{s}/movie.mkv.balance", 3 << 20)      # v1 orphan: no movie.mkv next to it
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
        os.chmod(p, mode)
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
    elif a.acl == "nfs4" and shutil.which("nfs4xdr_setfacl"):
        subprocess.run(["nfs4xdr_setfacl", "-a", "0", "A::alice@localdomain:rwaDxtTnNcCy",
                        f"{o}/bob-shared"], check=False)

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

    if a.skip_out:
        open(a.skip_out, "w").write("\n".join(skip) + "\n")
    print(f"generated test tree in {root} ({a.size_mb} MiB budget, acl={a.acl})")


if __name__ == "__main__":
    main()
