# The real-ZFS test lab

Unit tests can't tell you how this tool behaves on a real pool, with real vdevs, real ACLs and a
real TrueNAS. So before each release we build two small virtual machines and put the tool through
its paces on actual ZFS:

| VM | What it is | Why |
|---|---|---|
| **Linux** | Ubuntu 24.04 with OpenZFS 2.2 from the distro | A plain Linux install; POSIX ACLs; a second pool to compare against version 1 |
| **TrueNAS** | TrueNAS SCALE 25.10 from the official ISO, installed hands-off | What most people run; read-only system, NFSv4 ACLs, OpenZFS 2.3 with block cloning on |

Everything runs as an ordinary user on any Linux machine with KVM. It doesn't change the host's
network or install packages: the VMs use QEMU's built-in networking and are only reachable on
`127.0.0.1`.

## What you need

- A Linux host with KVM (`/dev/kvm` readable by you), about 16 GB of free RAM, and ~50 GB of disk
- `qemu-system-x86_64`, `qemu-img`, OVMF (UEFI firmware), `xorriso`, `python3`, `curl`
- On your own machine: Go and `make`, to build the release files

## Build the lab

```sh
# on the host
git clone https://github.com/astundzia/go-zfs-rebalance && cd go-zfs-rebalance/test/lab
./lab.sh setup             # checks tools, makes an SSH key, downloads Ubuntu + TrueNAS (checksum verified)
./lab.sh up-linux          # boots the Linux VM and waits for OpenZFS to be installed
./lab.sh truenas-install   # installs TrueNAS hands-off, reboots it, turns on SSH
```

`truenas-install` drives TrueNAS's own installer API (the same one its web installer uses), so
there's no clicking through screens. The admin password is random and saved in
`~/rebalance-lab/truenas/password`.

Then build the release files on your machine, copy them to the host and serve them to the VMs, so
the installer is tested exactly the way people use it:

```sh
make dist VERSION=v2.0.0
ssh host mkdir -p rebalance-lab/dist-new
scp dist/* host:rebalance-lab/dist-new/
ssh host go-zfs-rebalance/test/lab/lab.sh serve-dist rebalance-lab/dist-new
```

Handy commands: `./lab.sh ssh-linux`, `./lab.sh ssh-truenas`, `./lab.sh status`, and
`./lab.sh destroy` to throw the VMs away (downloads are kept). Set `LAB_DIR` to use another folder
and `TRUENAS_VERSION` / `TRUENAS_TRAIN` to try another TrueNAS release.

## The test data

`datagen.py` builds a deliberately awkward folder (run it as root):

- big files of random data, so compression can't hide where blocks land, and hundreds of small ones
- a sparse file, an empty file, and a 245-character file name
- names with spaces, `" at "`, colons, a newline, a terminal escape code and non-Latin characters
- files owned by other users, including setuid, setgid and sticky-bit files
- extended attributes, plus POSIX ACLs (Linux) or NFSv4 ACLs (TrueNAS)
- a hardlink group fully inside the folder, and one with a link outside it
- a real file called `ledger.balance`, and an orphan `movie.mkv.balance` like version 1 could leave behind
- symlinks (one pointing outside), a FIFO, and a folder tree 12 levels deep
- nanosecond timestamps on everything, including folders

`manifest.py snap` records every file's content hash, owner, group, mode, access and modify times,
link count, inode number, symlink target and **every** extended attribute (which is where Linux
keeps ACLs, including TrueNAS's `system.nfs4_acl_xdr`). `manifest.py diff` then compares two
snapshots. A run passes only if everything matches except the inode numbers, which must change for
every rewritten file. A new inode is the proof that the file was really written again.

## The test plan

Each scenario lists what must be true for it to pass.

### 1. Installing

| Scenario | Must be true |
|---|---|
| Install without `sudo` | Refuses, explains why, and prints the exact `sudo` command |
| Install with `sudo` on Linux | Installs to `/usr/local/bin`, verifies the SHA-256, `rebalance --version` shows the release |
| Install with `sudo` on TrueNAS | Notices it's TrueNAS, explains why, installs to `/root/.local/bin`; nothing under `/mnt` or `/home` |
| `--dir` pointing at a `noexec` folder (TrueNAS `/home`) | Explains the folder can't run programs and suggests another place |
| Download doesn't match its checksum | Refuses and leaves nothing installed |
| `--uninstall`, then install again | Binary removed, state left in place and mentioned; reinstall works |
| TrueNAS reboot | `/root/.local/bin/rebalance` and saved progress are still there |

### 2. Balancing

The pool starts as one vdev about half full. A second, empty vdev is added (through the TrueNAS
middleware on TrueNAS, like a real user would), then:

| Scenario | Must be true |
|---|---|
| `--report` | Table matches `zpool list -v -p`; works without root; spread around 50 points |
| `--vdev-report` run | Exit 0, a friendly summary, before/after tables, and the spread drops a lot |
| Data and metadata | `manifest.py diff` identical: content, owners, modes (incl. setuid/setgid/sticky), times to the nanosecond, xattrs, POSIX and NFSv4 ACLs |
| Really rewritten | Every rebalanced file has a new inode, and `zpool get bcloneused` didn't grow (no block cloning shortcut) |
| Leftovers | No temporary files left; folder times unchanged; sparse file still sparse |
| `.balance` files | `ledger.balance` rewritten and kept; the orphan `movie.mkv.balance` left untouched and warned about |
| Awkward names | Escape codes and newlines show up escaped in the log, never raw |
| Whole pool mount on TrueNAS | Child datasets inside the folder are included |

### 3. Hardlinks

| Scenario | Must be true |
|---|---|
| Default run | Hardlinked files skipped and counted |
| `--process-hardlinks` | A group fully inside the folder ends up sharing one new inode (same link count) |
| A link outside the folder | That group is left alone and reported |

### 4. Stopping and resuming

| Scenario | Must be true |
|---|---|
| Ctrl+C once | Finishes the files in progress, exit 130, no temporary files, data intact |
| Ctrl+C twice | Stops right away, still no temporary files, data intact |
| `--resume` | Only the files not yet done are rewritten |
| `--resume` again | Nothing to do ("already done") |
| `--resume --passes 2` | Everything is rewritten once more |
| Run without `--resume` | Starts fresh and says the old progress was discarded |

### 5. When things go wrong

| Scenario | Must be true |
|---|---|
| Missing folder, two folders, bad option values | Exit 2 with a clear message; nothing changed |
| Options after the folder | Honoured |
| A second run while one is going | Exit 2: "another rebalance is already running" |
| Running as an ordinary user | Their own files rewritten; others' skipped with a friendly reason and left untouched |
| Folder not on ZFS | A normal run warns; `--report` exits 2 |
| Dataset with snapshots | Warning before starting, and a note under the after table |
| Dedup turned on | Warning before starting |
| Pool or quota runs out of space | Stops with exit 3 and a plain message; no temporary files; data intact |
| A file is written to while it's being copied | That file is skipped as "changed", and the new data is kept |
| `--halt-on-missing` and a file is deleted mid-run | Stops with exit 3 |

### 6. Version 1 side by side

On a separate pool, version 1.0.1 is run on the same kind of data to show the bugs version 2
fixes: files copied with block cloning (nothing really rewritten), owners changed to root, ACLs and
xattrs dropped, a real `.balance` file deleted, long names failing. Version 2 is then run on that
pool to show it does the right thing.

### 7. Release files

`make dist` output: static binaries, every `.sha256` verifies, `--version` shows the tag, and the
release notes the workflow would publish are pulled correctly from `CHANGELOG.md`.

## Tidying up

```sh
./lab.sh destroy       # stop the VMs and delete their disks
rm -rf ~/rebalance-lab # and everything else, including the downloads
```
