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
  whose files inherit its ID, and `projdir/own-id.bin`, moved to another ID), immutable and
  append-only files, a file with a capability (`capability.bin`, `cap_net_raw=ep`, set with
  `setcap` or, without it, by writing `security.capability` directly), a file with a `trusted.*`
  xattr (`trusted.bin`, root only) and, on ZFS, DOS attributes (hidden/system/archive, read-only,
  and no-unlink). Run `datagen.py --clear-flags <root>` before deleting such a tree

`--skip-out FILE` lists the files a default run is expected to leave alone: hardlinked files, the
two `.balance` files, the immutable, append-only and no-unlink files, `projdir/own-id.bin` (see
[Permissions, attributes and flags](#7-permissions-attributes-and-flags)), and the setuid file
with an ACE. Pass it to `manifest.py diff --skip-file`. Every other file must be rewritten,
including `capability.bin` and `trusted.bin` with their xattrs kept.

`manifest.py snap` records every file's content hash, owner, group, mode, access and modify times,
link count, inode number, symlink target, **every** extended attribute (which is where Linux keeps
ACLs, including TrueNAS's `system.nfs4_acl_xdr`; `trusted.*` ones are only visible to root, so take
snapshots as root), inode flags (`lsattr`), project ID, and ZFS DOS attributes. Taking a snapshot
changes nothing: as root it reads files with `O_NOATIME`, so their access times don't move. (If it
ever has to put an access time back and can't, for example on an immutable file or a dataset at its
quota, it prints a warning naming the file instead of stopping.) `manifest.py diff` then compares
two snapshots. A run passes only if everything matches except the inode numbers, which must change
for every rewritten file. A new inode is the proof that the file was really written again.

The scripts have their own unit tests, which need no root, ZFS or Linux:
`python3 -m unittest discover -s test/lab -v`.

## The test plan

Each scenario lists what must be true for it to pass.

### 1. Go tests on real ZFS

Build Linux test binaries on your machine (without the race detector, so they cross-compile), and
copy them, with `internal/zfs/testdata`, into a dataset that can run programs, such as
`/tank/gotest` (`/mnt/tank/gotest` on TrueNAS, where `/tmp` and `/home` are mounted `noexec`):

```sh
mkdir -p linuxtests
for p in fileutil rebalance database zfs; do
  GOOS=linux GOARCH=amd64 go test -c -o "linuxtests/$p.test" "./internal/$p"
done
GOOS=linux GOARCH=amd64 go test -c -o linuxtests/cmd.test ./cmd/rebalance
cp -R internal/zfs/testdata linuxtests/
```

Then, in that folder on the VM, run each one as root and as an ordinary user, with
`REBALANCE_TEST_DIR` set so the tests work on ZFS (where the DOS-attribute, NFSv4 ACL and project-ID
tests can really run):

```sh
cd /tank/gotest
for t in fileutil rebalance database zfs cmd; do
  mkdir -p "work-root/$t"
  sudo env REBALANCE_TEST_DIR="$PWD/work-root/$t" "./$t.test" -test.v -test.count=1 > "root-$t.log" 2>&1
  echo "$t: $(tail -n 1 "root-$t.log")"
done
```

For the ordinary user, drop `sudo` and use a `work-user` folder that user owns. On TrueNAS, run
them inside tmux started without `sudo`, like the tool itself (see section 8), and use a dataset
that isn't an SMB share: with `aclmode=restricted`, the tests' own setup can't `chmod` the files
it makes. (The lab's SMB-share runs cover that case for the tool itself.)

| Scenario | Must be true |
|---|---|
| Each package, as root | `PASS`. The root-only tests really ran (`--- PASS: Test…Root…` lines), including the ZFS DOS attribute, nounlink, project ID (kept, and refused when its `+P` folder won't take it), capability and immutable ones |
| Each package, as an ordinary user | `PASS`. Root-only tests skip, saying why |
| Expected skips | The chattr `noatime` flag (ZFS has none), nodump inheritance (a filesystem choice), and tests meant only for root or only for an ordinary user. Tests that need to run a program they make skip where they can't, rather than fail |
| `TestFolderTimesKeptWhenAnotherProgramChangesTheFolder`, repeated (`-test.count=10`) | Passes every time, as root and not, on ZFS and on `tmpfs` |

### 2. Installing

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

### 3. Balancing

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

### 4. Hardlinks

| Scenario | Must be true |
|---|---|
| Default run | Hardlinked files skipped and counted |
| `--process-hardlinks` | A group fully inside the folder ends up sharing one new inode (same link count). Its `✓ rebalanced` line lists the other names without `--debug` (up to three, then "+N more") |
| A link outside the folder | That group is left alone and reported |

### 5. Stopping and resuming

| Scenario | Must be true |
|---|---|
| Ctrl+C once | Finishes the files in progress, exit 130, no temporary files, data intact |
| Ctrl+C twice, a second or more apart | Stops right away, still no temporary files, data intact, and the abandoned files' access times unchanged |
| Ctrl+C twice in quick succession (under a second) | Treated as one stop request: the files in progress still finish |
| Ctrl+C while it waits for ZFS to free space | Skips the wait with a message about the wait, not about copying |
| Output piped into `head` (`rebalance … 2>&1 \| head`) | Stops gently when `head` exits, like one Ctrl+C: exit 130 (never 141, killed by the closed pipe), no temporary files, data intact. Repeat it a few dozen times, and with `head -n 1` |
| SSH connection dropped without tmux | Stops gently, exit 130 |
| Ctrl+C three times | Quits at once, exit 130, data intact. Like `kill -9`, it doesn't tidy up: the next run removes any leftover temporary files, folders that were in use may keep a changed modified time, and with `--process-hardlinks` a file whose names were being switched over may be left as separate, identical copies (documented limits) |
| `kill -9` mid-run, then `--resume` | Leftover temporary files cleaned up, data intact. Folders that were in use may keep a changed modified time (a documented limit) |
| `--resume` | Only the files not yet done are rewritten |
| `--resume` again | Nothing to do ("already done"). With `--flags` data, the files skipped for lasting reasons (immutable, append-only, no-unlink, `own-id.bin`) are tried again and skipped again, and the summary says plainly that nothing new was rebalanced, and why (something like "Nothing new to rebalance: the 4 remaining files were skipped again (…)"), not "rebalanced 0 of 4 files" |
| `--resume --passes 2` | Everything is rewritten once more |
| Run without `--resume` | Starts fresh and says the old progress was discarded |

### 6. When things go wrong

| Scenario | Must be true |
|---|---|
| Missing folder, two folders, bad option values | Exit 2 with a clear message; nothing changed. A bad number or choice gets plain words ("--concurrency needs a whole number, like 4"), not Go's `flag -concurrency` wording |
| Options after the folder | Honoured |
| A second run while one is going | Exit 2: "another rebalance is already running" |
| Running as an ordinary user (`sudo -u alice -H rebalance …` on a mixed-owner folder) | Alice's own files rewritten. Files owned by other users, or with a group she isn't in, are skipped **before anything is copied**: their access times are unchanged and no data is written for them. Files she may not replace (unreadable, or in someone else's folder) are skipped, not failed. Exit 0, and the hint says to run it with `sudo` (not `--resume`). Folder times put back, or a warning if they couldn't be |
| Ordinary user on a TrueNAS share where they have `write_owner` | Others' files still skipped up front, never copied and given away |
| Folder not on ZFS | A normal run warns; `--report` exits 2 |
| Dataset with snapshots | Warning before starting, and a note under the after table |
| Dedup turned on | Warning before starting |
| Pool or dataset quota runs out of space | Stops with exit 3 and a plain message; no temporary files; data intact; failed files keep their access times; the folder's modified time is put back (it tries again for a few seconds while ZFS frees the abandoned copies' space) |
| A user or group quota is reached (`zfs set userquota@bob=…`) while the dataset has room | Only that user's files are skipped ("over their quota"), and the run carries on. ZFS only updates quota usage as it writes out each transaction group, so for a few seconds after the quota trips, some of that user's files that would have fit are skipped too; a later `--resume` picks them up |
| Dedup turned on for a child dataset inside the folder | Warning before starting that names that dataset |
| `rebalance … >/tank/data/run.log` (the log file inside the folder) | `run.log` is left alone |
| `--db` pointing at a file that isn't a rebalance progress file | Refuses with a friendly message; the file is untouched |
| A folder named `-dash dir` given without `--` | Exit 2, and the message says to put `--` before it |
| Two runs at once on the same folder with different `--db` files | Neither deletes the other's temporary files (they're reported as in use by another run and left alone). A file the other run is working on is skipped as busy, not as "changed"; no "hardlinks" skips; data identical. A file may be rewritten by both runs, one after the other, which is harmless |
| A file locked by another program (`flock -x big/blob000.bin sleep 600 &`, then a run) | That file is skipped as busy ("another program or another rebalance run is working on it"), untouched (same inode and access time); exit 0. Once the lock is gone, `--resume` rewrites it |
| A file is written to while it's being copied | That file is skipped as "changed", and the new data is kept |
| As a user who can write to the folder, swap a big file's hidden copy for a symlink to another file in the folder while it's being copied (root run) | The file is skipped and left as it was; the symlink's target keeps its content and times, because the copy's times are set through the open file, never by name |
| `--halt-on-missing` and a file is deleted mid-run | Stops with exit 3 |

### 7. Permissions, attributes and flags

Make the data with `datagen.py --flags` (and `--acl nfs4` on TrueNAS), and run as root:

| Scenario | Must be true |
|---|---|
| `chattr +d` (nodump) and project IDs | Rewritten and kept exactly: `lsattr -p` shows the same flags and IDs (`project.bin` 4242, and `projdir/inherits.bin` 777 inside the `+P` folder) |
| `projdir/own-id.bin`: project 888 inside a `+P` folder with 777 | Skipped with its own reason (its project ID is different from its folder's), and left untouched: same inode, still 888. It's in the skip file. ZFS refuses to rename or link a file into a `+P` folder unless it has the folder's ID (`EXDEV`), so a copy can never take its place. (`mv` hides this by falling back to a copy, which resets the ID to the folder's.) |
| ZFS DOS attributes (hidden, system, archive, read-only) | Kept exactly |
| Immutable (`chattr +i`) and append-only (`chattr +a`) files | Skipped as immutable or append-only, left exactly as they were (content, flags, access time), exit 0 |
| DOS no-unlink file (`dos-nounlink.bin`) | Skipped as protected from deletion, left exactly as it was, exit 0 |
| `capability.bin` (`cap_net_raw=ep`) and `trusted.bin` (`trusted.lab`), made by `datagen.py --flags` | Rewritten (new inode) with the xattrs kept: `getcap` and `getfattr -n trusted.lab` show the same, and the manifest diff is identical |
| The summary's skip line | Each label matches the files it counts: immutable or append-only, protected from deletion, project ID, permissions, owner, group and busy are counted separately |
| TrueNAS SMB share (`aclmode=restricted`): setuid/setgid file with an explicit ACE (`owners/alice-setuid-ace`) | Skipped because its permissions can't be kept exactly; original untouched; exit stays 0 |
| While a copy is in progress, as another user | The hidden temporary file can't be read by anyone who couldn't read the original (its owner and ACL are set before any data is written). With `atime=on`, a user who may read the file and keeps opening the hidden copies (like a virus scanner would) doesn't change the rewritten file's access time |

### 8. TrueNAS and tmux

| Scenario | Must be true |
|---|---|
| Start `tmux` as `truenas_admin` (no sudo), then `sudo rebalance --vdev-report …` inside it; close the browser tab (or drop the SSH connection) mid-run and reattach | The run finishes and the before-and-after table is printed |
| `sudo -i`, then `tmux`, then run; close the browser tab | Files are still rebalanced. Once sudo's session ends TrueNAS stops it starting `zfs` and `zpool`, so instead of the table it prints a friendly explanation (start tmux without sudo next time), never a raw error. It doesn't first say it's waiting for ZFS to free space, and if the block-cloning check during the run is blocked, it says so once |

### 9. Version 1 side by side

On a separate pool, version 1.0.1 is run on the same kind of data to show the bugs version 2
fixes: files copied with block cloning (nothing really rewritten), owners changed to root, ACLs and
xattrs dropped, a real `.balance` file deleted, long names failing. Version 2 is then run on that
pool to show it does the right thing.

### 10. Release files

`make dist` output: static binaries built with at least the Go named on go.mod's `toolchain` line
(`go version dist/rebalance-linux-amd64`), every `.sha256` verifies, `--version` shows the tag, and
the release notes the workflow would publish are pulled correctly from `CHANGELOG.md`.

## Latest lab run

**2026-10-03**, with the v2.0.0 release candidate, on:

- **Linux**: Ubuntu 24.04, OpenZFS 2.2.2 (block cloning on)
- **TrueNAS**: TrueNAS SCALE 25.10.7, OpenZFS 2.3.9 (block cloning on, `atime=on` set by hand so
  the access-time checks mean something; new TrueNAS pools have it off)

**Results:** 40 of 42 scenarios passed on Linux, and 12 of 14 on TrueNAS. The ones that failed
were the Go test runs (as root and as an ordinary user on each VM). Most of those failures came
from the tests themselves; one pointed at a small real weakness, now fixed:

- `TestGroupNotYoursSkippedUnlessTheFolderGivesIt` never made its folder setgid, because Go's
  `os.Chmod` ignores the raw `0o2000` bit. It now uses `os.ModeSetgid`. The real case, a file
  whose group comes from its setgid folder (one the user isn't in), was rewritten correctly in the
  run without `sudo`.
- On TrueNAS, `TestExecRunner` tried to run a program from `/tmp`, which is mounted `noexec`. It
  now uses `REBALANCE_TEST_DIR`, or skips where it can't run programs. (Going the other way, with
  the temporary folder on ZFS, two `cmd` tests that expect a folder that isn't on ZFS failed; they
  now treat the folder as not being on ZFS.)
- On TrueNAS, `TestFolderTimesKeptWhenAnotherProgramChangesTheFolder` failed now and then. This
  is the real weakness: if another program changed a folder within the same clock
  tick as one of the run's own renames, the folder's old time could be put back. The folder's
  entry count and link count are now compared too.
- On OpenZFS 2.2.2, the project-ID refusal test skipped, because its helper turned on project
  inheritance in a way that version rejects. It now does it the way ZFS accepts.

The main runs, after adding an empty second mirror to a pool about half full:

Linux: `sudo rebalance --vdev-report /tank/data/share` rebalanced 694 of 698 files (5.9 GiB) in
55 seconds, at 113 MB/s.

```
Before and after:
Pool tank   USED%            ALLOC CHANGE
mirror-0    51.4% -> 21.8%       -2.8 GiB
mirror-1     0.0% -> 29.7%       +2.8 GiB
spread      51.4 pts -> 7.9 pts
```

TrueNAS: `sudo rebalance --vdev-report /mnt/tank`, in tmux started without `sudo`, with the SSH
connection dropped part-way, rebalanced 1,582 of 1,593 files (6.5 GiB) in 1m 05s, at 105.8 MB/s.

```
Before and after:
Pool tank   USED%            ALLOC CHANGE
mirror-0    47.6% -> 19.6%       -2.7 GiB
mirror-1     0.0% -> 27.9%       +2.7 GiB
spread      47.6 pts -> 8.3 pts
```

On both, the manifest diff was identical (content, owners, modes, nanosecond times, xattrs, POSIX
and NFSv4 ACLs, flags, project IDs and DOS attributes), every rewritten file had a new inode, and
`bcloneused` stayed at 0. The only file left alone that the skip file didn't expect was
`flags/projdir/own-id.bin`, which is how the project-ID limit was found. It's now documented and
in the skip file.

Changed since that run, so check these again next time (section numbers in brackets):

- the scripts: `manifest.py` no longer stops on immutable and append-only files, and
  `datagen.py --flags` adds `own-id.bin` to the skip file and makes the capability and `trusted.*`
  files (7)
- the repeated folder-times test (1)
- a rewritten hardlinked file's other names in its log line (4)
- the exit code when output goes into `head`, and the second `--resume` message (5)
- busy files and two runs at once, plain words for bad option values, folder times after a quota
  stop, and a symlink swapped in for the hidden copy (6)
- the summary's skip labels, and access times when something reads the hidden copy just before
  the swap (7)
- no "Waiting for ZFS" line when the tools are blocked, and the block-cloning check saying so (8)

## Tidying up

```sh
./lab.sh destroy       # stop the VMs and delete their disks
rm -rf ~/rebalance-lab # and everything else, including the downloads
```

To delete just a generated tree inside a VM that was made with `--flags`, run
`datagen.py --clear-flags <root>` first: immutable, append-only and no-unlink files can't be removed
until then.
