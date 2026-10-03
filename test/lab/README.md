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
- extended attributes, plus POSIX ACLs (`--acl posix`) or NFSv4 ACLs (`--acl nfs4`, TrueNAS). The
  NFSv4 mode also works on SMB-style datasets (`aclmode=restricted`), where `chmod` is refused while
  a file has a non-trivial ACL: it strips the ACL first, sets the mode, then adds the test ACEs. It
  adds `owners/alice-setuid-ace`, a setuid file with an explicit ACE
- a hardlink group fully inside the folder, and one with a link outside it
- `ledger.balance` and `movie.mkv.balance`, both with no original next to them. Version 1 would
  have deleted them; version 2 can't tell a real file from a version 1 leftover that may be the
  only copy, so it must leave both alone and warn about them
- symlinks (one pointing outside), a FIFO, and a folder tree 12 levels deep
- nanosecond timestamps on everything, including folders
- with `--flags` (Linux): `chattr +d` (nodump), project IDs (`chattr -p`, including a `+P` folder
  whose files inherit its ID and one file moved to another ID), immutable and append-only files and,
  on ZFS, DOS attributes (hidden/system/archive, read-only, and no-unlink). Run
  `datagen.py --clear-flags <root>` before deleting such a tree

`--skip-out FILE` lists the files a default run is expected to leave alone (hardlinked files, the
two `.balance` files, immutable and no-unlink files, and the setuid file with an ACE); pass it to
`manifest.py diff --skip-file`.

`manifest.py snap` records every file's content hash, owner, group, mode, access and modify times,
link count, inode number, symlink target, **every** extended attribute (which is where Linux keeps
ACLs, including TrueNAS's `system.nfs4_acl_xdr`; `trusted.*` ones are only visible to root, so take
snapshots as root), inode flags (`lsattr`), project ID, and ZFS DOS attributes. `manifest.py diff`
then compares two snapshots. A run passes only if everything matches except the inode numbers,
which must change for every rewritten file. A new inode is the proof that the file was really
written again.

## The test plan

Each scenario lists what must be true for it to pass.

### 1. Installing

| Scenario | Must be true |
|---|---|
| Install without `sudo` | Refuses, explains why, and prints the exact `sudo` command, keeping the options given |
| `--uninstall` without `sudo` | Says nothing was removed and suggests `… \| sudo bash -s -- --uninstall` (not the install command) |
| Install with `sudo` on Linux | Installs to `/usr/local/bin`, verifies the SHA-256, `rebalance --version` shows the release |
| Install with `sudo` on TrueNAS | Notices it's TrueNAS, explains why (read-only system, `/home` can't run programs, stays off the pools), installs to `/root/.local/bin`; nothing under `/mnt` or `/home` |
| `--dir /home/truenas_admin/bin` on TrueNAS (`noexec`, and TrueNAS's sudo kills a blocked program) | Says the folder is mounted `noexec` **before** running anything, suggests `--dir /root/.local/bin`, creates no folder, and prints no stray `sudo: … unexpected status` line |
| `--dir /tmp/x` on TrueNAS (`noexec`) | Same as above |
| Output piped into `head -n 1`, under `sh` (dash) and `bash` | No hidden `.rebalance.*` file is left in the install folder |
| A `.rebalance.XXXXXXXX` left by an earlier, killed install | Removed at the start of the next install, and mentioned |
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
| Data and metadata | `manifest.py diff` identical: content, owners, modes (incl. setuid/setgid/sticky), times to the nanosecond, xattrs, POSIX and NFSv4 ACLs, inode flags, project IDs and DOS attributes |
| Really rewritten | Every rebalanced file has a new inode, and `zpool get bcloneused` didn't grow (no block cloning shortcut) |
| Block cloning by something else during a `--vdev-report` run (`cp --reflink=always` of a big file in the pool while it runs) | A note under the table says block cloning grew during the run |
| Leftovers | No temporary files left; folder times unchanged; sparse file still sparse |
| `.balance` files | `ledger.balance` and `movie.mkv.balance` (no original next to either) are **expected** to be left untouched and warned about; both are in the skip file |
| Free-space check | Based on the space files really take, so the sparse 1 GiB file (about 12 MiB on disk) doesn't make it warn |
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
| Ctrl+C twice, a second or more apart | Stops right away, still no temporary files, data intact, and the abandoned files' access times unchanged |
| Ctrl+C twice in quick succession (under a second) | Treated as one stop request: the files in progress still finish |
| Ctrl+C while it waits for ZFS to free space | Skips the wait with a message about the wait, not about copying |
| Output piped into `head` (`rebalance … 2>&1 \| head`) | Stops gently when `head` exits, like one Ctrl+C: no temporary files, data intact |
| SSH connection dropped without tmux | Stops gently, exit 130 |
| `kill -9` mid-run, then `--resume` | Leftover temporary files cleaned up, data intact. Folders that were in use may keep a changed modified time (a documented limit) |
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
| Running as an ordinary user (`sudo -u alice -H rebalance …` on a mixed-owner folder) | Alice's own files rewritten. Files owned by other users, or with a group she isn't in, are skipped **before anything is copied**: their access times are unchanged and no data is written for them. Files she may not replace (unreadable, or in someone else's folder) are skipped, not failed. Exit 0, and the hint says to run it with `sudo` (not `--resume`). Folder times put back, or a warning if they couldn't be |
| Ordinary user on a TrueNAS share where they have `write_owner` | Others' files still skipped up front, never copied and given away |
| Folder not on ZFS | A normal run warns; `--report` exits 2 |
| Dataset with snapshots | Warning before starting, and a note under the after table |
| Dedup turned on | Warning before starting |
| Pool or dataset quota runs out of space | Stops with exit 3 and a plain message; no temporary files; data intact; failed files keep their access times |
| A user or group quota is reached (`zfs set userquota@bob=…`) while the dataset has room | Only that user's files are skipped ("over their quota"), and the run carries on |
| Dedup turned on for a child dataset inside the folder | Warning before starting that names that dataset |
| `rebalance … >/tank/data/run.log` (the log file inside the folder) | `run.log` is left alone |
| `--db` pointing at a file that isn't a rebalance progress file | Refuses with a friendly message; the file is untouched |
| A folder named `-dash dir` given without `--` | Exit 2, and the message says to put `--` before it |
| Two runs at once on the same folder with different `--db` files | Neither deletes the other's temporary files (they're reported as in use by another run and left alone); no spurious "changed" or "hardlinks" skips |
| A file is written to while it's being copied | That file is skipped as "changed", and the new data is kept |
| `--halt-on-missing` and a file is deleted mid-run | Stops with exit 3 |

### 6. Permissions, attributes and flags

Make the data with `datagen.py --flags` (and `--acl nfs4` on TrueNAS), and run as root:

| Scenario | Must be true |
|---|---|
| `chattr +d` (nodump) and project IDs | Kept exactly: `lsattr -p` shows the same flags and IDs, including the file moved to another ID inside a `+P` folder (it must not fall back to the folder's ID) |
| ZFS DOS attributes (hidden, system, archive, read-only) | Kept exactly |
| Immutable (`chattr +i`), append-only (`chattr +a`) and DOS no-unlink files | Skipped as immutable or append-only, left exactly as they were, exit 0 |
| `security.capability` (`setcap cap_net_raw=ep`) and `trusted.*` xattrs | Kept |
| TrueNAS SMB share (`aclmode=restricted`): setuid/setgid file with an explicit ACE (`owners/alice-setuid-ace`) | Skipped because its permissions can't be kept exactly; original untouched; exit stays 0 |
| While a copy is in progress, as another user | The hidden temporary file can't be read by anyone who couldn't read the original (its owner and ACL are set before any data is written) |

### 7. TrueNAS and tmux

| Scenario | Must be true |
|---|---|
| Start `tmux` as `truenas_admin` (no sudo), then `sudo rebalance --vdev-report …` inside it; close the browser tab mid-run and reattach | The run finishes and the before-and-after table is printed |
| `sudo -i`, then `tmux`, then run; close the browser tab | Files are still rebalanced. Once sudo's session ends TrueNAS stops it starting `zfs` and `zpool`, so instead of the table it prints a friendly explanation (start tmux without sudo next time), never a raw error |

### 8. Version 1 side by side

On a separate pool, version 1.0.1 is run on the same kind of data to show the bugs version 2
fixes: files copied with block cloning (nothing really rewritten), owners changed to root, ACLs and
xattrs dropped, a real `.balance` file deleted, long names failing. Version 2 is then run on that
pool to show it does the right thing.

### 9. Release files

`make dist` output: static binaries built with at least the Go named on go.mod's `toolchain` line
(`go version dist/rebalance-linux-amd64`), every `.sha256` verifies, `--version` shows the tag, and
the release notes the workflow would publish are pulled correctly from `CHANGELOG.md`.

## Tidying up

```sh
./lab.sh destroy       # stop the VMs and delete their disks
rm -rf ~/rebalance-lab # and everything else, including the downloads
```

To delete just a generated tree inside a VM that was made with `--flags`, run
`datagen.py --clear-flags <root>` first: immutable, append-only and no-unlink files can't be removed
until then.
