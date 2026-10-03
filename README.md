# go-zfs-rebalance

[![CI](https://github.com/astundzia/go-zfs-rebalance/actions/workflows/ci.yml/badge.svg)](https://github.com/astundzia/go-zfs-rebalance/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/astundzia/go-zfs-rebalance)](https://goreportcard.com/report/github.com/astundzia/go-zfs-rebalance)
[![Go Version](https://img.shields.io/github/go-mod/go-version/astundzia/go-zfs-rebalance)](https://github.com/astundzia/go-zfs-rebalance)

**What does this do?** When you add new drives to a ZFS pool, the files you already have stay on
the old drives. So the old drives do most of the work, and the new ones sit mostly empty.
`rebalance` rewrites each file in place, which lets ZFS spread it across *all* your drives. Your
files, owners, permissions and ACLs stay exactly the same.

It works on one folder at a time, and it's careful: each file is copied, checked, and only then
swapped in. If anything goes wrong, the original is left as it was.

**Contents:** [Getting started](#getting-started) · [FAQ](#faq) · [Reference](#reference)

## Getting started

These steps are written for **TrueNAS SCALE** (now called TrueNAS Community Edition). On other
Linux systems or macOS they're the same, apart from where the program is installed (see
[Installation](#installation)).

### Before you start

- **Have a backup.** This tool is careful, but your data is precious.
- **Pause automatic snapshots** while it runs (on TrueNAS, turn off your periodic snapshot tasks
  under **Data Protection**). Why: a snapshot keeps the old copy of every file that gets
  rewritten, so your data is stored twice and the pool can fill up.
- **Check your free space.** Without snapshots, it only needs room for the few files it copies at
  a time. If the folder already has snapshots, it needs room for everything it rewrites.
- **Stop apps that write to the folder**, such as VMs, databases or download clients. A program
  that's in the middle of writing to a file when it's swapped could lose that write.
- **Set aside time.** It takes about as long as copying all the folder's data once. A progress
  line every minute tells you how long is left.
- **Use tmux** (or `screen`), so closing the browser tab or SSH window doesn't stop it. tmux
  keeps your session running in the background. Start it as yourself, **without** `sudo` (and not
  after `sudo -i`), then run `sudo rebalance …` inside it:

  ```sh
  tmux           # start a session as yourself, then run sudo rebalance inside it
                 # to leave it running, press Ctrl+B, then D
  tmux attach    # come back to it later
  ```

  Why without sudo? TrueNAS keeps a log of everything run with `sudo`, and once the `sudo` that
  started tmux has finished, programs still running inside it aren't allowed to start other
  programs, so `rebalance` couldn't run `zpool` for its before-and-after table. (If that happens,
  it tells you, and the files are still rebalanced.)

  Without tmux, a dropped connection stops the run safely (just like pressing Ctrl+C once), and
  you can carry on later with `--resume`.

### 1. Install

On TrueNAS, open **System → Shell** (called **System Settings → Shell** on older versions) and
paste:

```sh
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash
```

`sudo` may ask for your password. The installer downloads the right program for your system,
checks it against its published checksum, and puts it at `/root/.local/bin/rebalance`.

Why there? TrueNAS keeps the system read-only and `/home` can't run programs, so it goes in root's
home folder, which can, and which is kept when TrueNAS updates. The installer stays off your pools,
so rebalancing never touches it.

In the commands below, swap `/mnt/tank/media` for one of your own folders.

### 2. Check your pool

```sh
sudo /root/.local/bin/rebalance --report /mnt/tank/media
```

This only looks; it changes nothing. It finds the pool that the folder is on, and shows how full
each **vdev** in that pool is (a vdev is one group of drives, such as a mirror or a raidz):

```
Pool tank       SIZE      ALLOC   USED%
raidz1-0    21.8 TiB   15.2 TiB   69.7%
raidz1-1    21.8 TiB   12.0 TiB   55.0%
raidz1-2    21.8 TiB    1.8 TiB    8.3%
spread      61.4 pts
```

**Spread** is the gap between your fullest and your emptiest vdev, in percentage points. The
bigger it is, the more rebalancing will help. Close to 0 means your pool is already even. Here,
`raidz1-2` was added recently and is nearly empty.

### 3. Start it

```sh
tmux           # as yourself, without sudo
sudo /root/.local/bin/rebalance --vdev-report /mnt/tank/media
```

`--vdev-report` prints the table from step 2 at the start, and a before-and-after comparison at
the end.

You'll see a few lines about what it found, then a line for each file, and a progress line every
minute:

```
2:14:03 PM  Rebalancing /mnt/tank/media
2:14:03 PM  Settings: 4 files at a time, in a random order, each copy checked with sha256.
2:14:03 PM  Looking through the folder to see what needs doing…
2:14:09 PM  Found 48,210 files to rebalance (12.4 TiB).
2:14:12 PM  ✓ rebalanced  movies/Big Buck Bunny (2008).mkv  1.2 GiB at 412.6 MB/s
…
3:14:09 PM  Progress: 5,402 of 48,210 files, 1.4 TiB of 12.4 TiB (11%), 427.6 MB/s, about 7h 51m left
```

Lines with a `!` are warnings worth reading: see
[What do the warnings at the start mean?](#what-do-the-warnings-at-the-start-mean) A file that
can't be rewritten is skipped or listed as failed, and is always left exactly as it was.

### 4. Need to stop?

Press **Ctrl+C** once, and it finishes the files it's working on, then stops. Press it again (a
second or more later) to stop right away. Either way is safe: a file is only swapped once its new
copy is complete and checked, and a half-made copy is simply deleted. If it ever seems stuck, a
third Ctrl+C quits at once, without tidying up. Your files are still safe, but the next run has to
remove any half-made copy, and the folders it was working in may keep a new modified time (see
[Safety details and limits](#safety-details-and-limits)).

```
3:16:12 PM  ! Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)
3:16:13 PM  ! Stopped when asked, after 1h 02m: rebalanced 5,610 of 48,210 files (1.5 TiB at 442.9 MB/s).
3:16:13 PM  To finish the rest, run the same command again with --resume added.
```

### 5. Pick up where you left off

Run the same command with `--resume` added:

```sh
sudo /root/.local/bin/rebalance --vdev-report --resume /mnt/tank/media
```

It skips the files that are already done. Without `--resume`, it starts again from the beginning
and rewrites everything. If that clears earlier progress, it says so and reminds you about
`--resume`.

The before-and-after table only covers the current run. If you want the whole picture, save the
`--report` output from step 2.

### 6. See the difference

When it finishes, `--vdev-report` prints something like this:

```
Before and after:
Pool tank   USED%            ALLOC CHANGE
raidz1-0    69.7% -> 46.3%       -5.1 TiB
raidz1-1    55.0% -> 44.0%       -2.4 TiB
raidz1-2     8.3% -> 42.7%       +7.5 TiB
spread      61.4 pts -> 3.6 pts
```

ZFS frees the old copies' space in the background, so `rebalance` waits up to 2 minutes for that
before printing the table. The numbers can still settle a little after that, so run `--report`
again in a few minutes to see the final result. If snapshots are holding on to the old copies,
the old vdevs won't shrink until those snapshots are deleted. A `note:` line under the table
tells you when that's the case.

That's it! Remember to turn your snapshot tasks back on.

## FAQ

### Is it safe?

It's built to be. Each file is copied to a new hidden file in the same folder. The copy is read
back and checked, given the same owner, permissions, attributes and times as the original, and
only then swapped in, in a single step. The original isn't changed or removed before that swap.
If anything goes wrong (an error, a power cut, Ctrl+C), the original stays as it was. If a file
changes while it's being copied, it's skipped, and so is a file that another `rebalance` run (or a
program using `flock`) has locked. The hidden copy gets the original's owner and ACL before any data goes into it,
so nobody can read it who couldn't read the original.

Still, no tool is perfect. Keep a backup, and read [Safety details and limits](#safety-details-and-limits)
for the few things it can't protect against.

### Will permissions, owners or ACLs change?

No. The owner, group, permissions (including setuid and setgid), extended attributes, ACLs
(TrueNAS's NFSv4 and POSIX ACLs, and macOS ACLs) and the access and modified times are all
copied, then checked before the swap. On Linux, so are file flags such as nodump (`chattr +d`),
ZFS project IDs, and the DOS attributes that SMB shares use (hidden, archive, read-only and so
on). If any of them can't be copied exactly, the file is skipped: it's left exactly as it was,
the summary at the end counts it, and the exit code stays 0. (One case is always skipped: a file
whose project ID is different from its folder's, in a folder set with `chattr +P`. ZFS won't let
a copy take its place. See [Safety details and limits](#safety-details-and-limits).)

Run it with `sudo`. Only root can give a file someone else's owner. Without `sudo`, it only
rewrites your own files: files owned by someone else, or whose group you're not in, are skipped
before anything is copied, and so are files you aren't allowed to replace. They're never changed,
not even their access times, and the summary tells you how to include them.

What does change: the file's inode number (its internal ID), its "change time" (ctime) and its
creation time. Any rewrite changes these. On macOS, file flags set with `chflags` (such as
`hidden`) aren't kept.

### Why are my old drives still fuller than the new ones?

The common reasons:

- **Snapshots.** A snapshot keeps the old copy of each rewritten file, so the old vdevs won't
  shrink until those snapshots are deleted. The `note:` under the after-table says how much they
  hold.
- **ZFS is still freeing space** in the background. Run `--report` again in a few minutes.
- **Only that folder was rewritten.** Data in other datasets or folders on the pool hasn't moved.
- **Some files were skipped.** The summary at the end says how many, and why (for example
  hardlinked files, or files owned by other users when it wasn't run with `sudo`). See
  [Why were some files skipped?](#why-were-some-files-skipped)
- **ZFS doesn't aim for perfectly equal.** It favours emptier vdevs when it writes, so the gap
  shrinks a lot, but rarely to exactly 0. A second pass (`--resume --passes 2`) can narrow it
  further.
- **Deduplication or block cloning.** Either can make the new copies point at the old blocks. The
  warnings at the start and the notes under the table tell you if that's happening.

### Why does my backup tool or zfs send think everything changed?

Because, underneath, it did: every file now sits on new blocks, with a new inode number and
change time. So:

- The next incremental `zfs send` (and TrueNAS replication task) will be about as big as all the
  data you rewrote.
- Backup tools that look at inode numbers or change times, such as restic and Borg, will read
  every file again. They'll find the contents are the same and won't store them twice, but that
  first backup will take longer.

It's worth planning for: for example, rebalance just before a full backup.

### What about hardlinked files?

A hardlinked file is one file with several names. They're skipped by default: the run names up to
five of them (then how many more), the summary counts them, and it tells you how to include them.
Add `--process-hardlinks` to include them: the file is copied once, and every name is switched
over to the new copy, so they stay linked together. Its line in the log lists its other names too
(up to three, then how many more). Once a hardlinked file has been rewritten that way, a later
`--resume` without `--process-hardlinks` counts it as already done, not as hardlinked.

If some of a file's names are outside the folder you gave, it's left alone, because rewriting only
some of its names would split it into two separate copies. Each of its names in the folder is
listed in the log, so you can find it. Point `rebalance` at a folder that holds all the names.

### How long will it take?

About as long as copying all the folder's data once, sometimes a little longer, because each copy
is read back and checked. Lots of small files take longer than a few big ones. The progress line
shows an estimate, and you can stop and resume at any time.

### What do the warnings at the start mean?

| You see | What it means | What to do |
|---|---|---|
| Not running as root | Only your own files will be rewritten. Files owned by other users, in a group you're not in, or that you aren't allowed to replace, are skipped before anything is copied, and never changed. | Run it with `sudo`. |
| This folder isn't on ZFS | Rewriting files won't rebalance anything. | Check the folder. On TrueNAS, pools are under `/mnt`. |
| The folder is on ZFS, but the zfs tools aren't available | Files can still be rewritten, but the snapshot and dedup checks and the tables are skipped. | Make sure `zfs` and `zpool` are installed and on the command search path. |
| This dataset has snapshots | Every rewritten file is stored twice until those snapshots are gone. | Watch your free space, or delete snapshots you don't need first. |
| Deduplication is on | It will be slow, and the new copies may point back at the old blocks, so the data may not move. The warning lists every dataset in the folder that has it on, including ones mounted inside it. | It's your call. Turning dedup off for those datasets first lets the data move. |
| Free space is tight | Copying several big files at once might run out of room. | Use a lower `--concurrency`, or free up some space. |
| Found … files ending in .balance | Possible leftovers from version 1. | See [the .balance question](#i-used-version-1-what-are-these-balance-files). |
| Found … temporary files … kept because of --no-cleanup | An earlier run was stopped hard (for example, killed or a power cut). | Run without `--no-cleanup` and they're removed. |
| Starting fresh, so the saved progress … was discarded | You didn't add `--resume`, so it's starting over. | Add `--resume` next time to carry on instead. |
| There's no saved progress for this folder to resume | `--resume` found nothing to carry on from. | Nothing. It starts from the beginning. |

Every check is a warning, not a stop: the run carries on either way. If you'd rather sort
something out first, press Ctrl+C. It stops safely, and you can start again whenever you're
ready.

### Why were some files skipped?

A skipped file is always left exactly as it was, and skipping doesn't change the exit code. The
summary at the end counts skipped files by reason, such as
`Skipped 12 files: 8 already done, 4 hardlinked.` Most of them also get a `! skipped` line in the
log that names the file and says why (hardlinked files are named together in one list instead: up
to five of them, or all of them with `--debug`).

| Reason | What it means | What to do |
|---|---|---|
| Already done | Rewritten by an earlier run, and you added `--resume`. That includes hardlinked files rewritten with `--process-hardlinks`. | Nothing. |
| Hardlinked | One file with several names. | Add `--process-hardlinks`. |
| Hardlinks outside the folder | Some of the file's names are outside the folder you gave. | Point it at a folder that holds all the names. |
| Leftover .balance file | May be the only copy of a file, left by version 1. | See [the .balance question](#i-used-version-1-what-are-these-balance-files). |
| Owned by someone else | Without `sudo`, only your own files are rewritten. | Run it with `sudo`. |
| In a group you're not in | Your file, but its group is one you're not in, and only root can give the copy that group. | Run it with `sudo`. |
| You aren't allowed to replace it | Without `sudo`, you can't read it, or can't change its folder. | Run it with `sudo`. |
| Owner can't be kept | Even root couldn't give the copy the file's owner, for example on an idmapped mount or a share that maps root to another user. | Run it where root can set owners, such as on the server itself. |
| Permissions can't be kept exactly | The copy couldn't get exactly the same permissions, ACL, attributes or times. For example, on a TrueNAS SMB share, a setuid or setgid file with ACL entries of its own. | Nothing, unless you want to remove the setuid bit or the extra ACL entries. |
| Project ID different from its folder's | Its folder is set with `chattr +P`, and ZFS won't let a copy take its place unless it has the folder's project ID (see [Safety details and limits](#safety-details-and-limits)). | Nothing. It's a ZFS rule, and the file keeps its own ID. |
| Immutable or append-only | Marked with `chattr +i` or `+a` (or the ZFS equivalents, or `chflags uchg` or `uappnd` on macOS), so it can't be replaced. | To include it, remove the mark (`chattr -i`), run again, then put the mark back. |
| Protected from deletion | Has ZFS's nounlink attribute (or `chflags sunlnk` on macOS), so it can't be replaced. | Nothing. |
| Over quota | Its owner or group has reached their quota, though the dataset still has room. | Raise the quota or free some space, then run again with `--resume`. |
| Busy | Another `rebalance` run, or a program using `flock` locks, was working on it. | Run again with `--resume` later. |
| Changed while copying | Something wrote to it while it was being copied, so the copy was thrown away. | Run again with `--resume` once that program has finished. |
| Missing | It was deleted or renamed after the folder was scanned. | Nothing. |
| Couldn't be read | A folder it couldn't look inside, or a file whose details it couldn't read. The log names it. | Check its permissions. |

`--resume` only passes over the files that are already done, so it tries the skipped ones again,
in case the reason has gone away (for example, you cleared an immutable mark). If they're all
that's left and they're skipped again for a reason that lasts (such as an immutable mark, another
owner or a project ID), the summary says that nothing new was rebalanced, and why.

### What do the exit codes mean?

`0` means it finished; skipped files don't count as failures. `1` means some files couldn't be
rewritten (each was left as it was). `2` means it didn't start, for example because of a typo in an
option. `3` means it stopped early, because a file went missing (`--halt-on-missing`) or the pool or
dataset ran out of space (or reached its quota). `130` means it was stopped with Ctrl+C. See the
[Exit codes](#exit-codes) table for details.

### I used version 1. What are these .balance files?

Version 1 made its temporary copies as `<name>.balance`, and removed the original *before* moving
the copy into place. If it was stopped at the wrong moment, it could leave `.balance` files
behind. Version 2 never deletes them:

- **`photo.jpg.balance` next to `photo.jpg`**: probably a spare copy. Version 2 rewrites it like
  any other file. Once you've checked that `photo.jpg` is fine, you can delete the `.balance` one.
- **`photo.jpg.balance` with no `photo.jpg`**: this may be the **only copy** of `photo.jpg`.
  Version 2 lists these at the start and doesn't touch them. Open it to check it looks right, then
  rename it back: `mv photo.jpg.balance photo.jpg`.

To find them all: `sudo find /mnt/tank/media -name '*.balance'`.

A few other things changed since version 1, notably what `--passes` means and its default (now 1).
See the [changelog](CHANGELOG.md) for the full list.

### How do I update or uninstall?

To update, run the install command again. It replaces the program and keeps your saved progress.

To uninstall:

```sh
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash -s -- --uninstall
```

If you installed it into a folder of your own, add `--dir` and that folder. Saved progress is
left in `/root/.local/state/go-zfs-rebalance` (`/var/root/.local/state/go-zfs-rebalance` on macOS).
You can delete that folder if you won't use `rebalance` again.

### Does it work on macOS, Windows or TrueNAS CORE?

- **TrueNAS SCALE / Community Edition, and other Linux** (64-bit Intel/AMD or ARM): yes.
- **macOS** (Intel and Apple silicon), with OpenZFS on OS X: yes, though it's tested less there
  than on Linux.
- **Windows**: no. It can't keep Windows' permissions, owners and alternate data streams, so it
  refuses to run there.
- **TrueNAS CORE and FreeBSD**: no, sorry.

## Reference

### Installation

#### The installer

The quickest way, on TrueNAS SCALE, other Linux systems and macOS:

```sh
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash
```

It needs root, and it works the same with `sudo sh`. Without root it stops, and shows the exact
command to run instead (with the options you gave). It picks the right build for your system
(Linux or macOS, amd64 or arm64), downloads it, checks its SHA-256 checksum, makes sure it starts,
and then moves it into place in one step. If any of that fails, nothing is installed and an
existing copy is kept. When it's done, it prints the next two commands to run.

Where it goes, unless you choose with `--dir`:

| System | Folder |
|---|---|
| TrueNAS SCALE | `/root/.local/bin` (root's home folder, which is kept when TrueNAS updates) |
| Other Linux | `/usr/local/bin`, or root's `.local/bin` if `/usr/local/bin` can't be used |
| macOS | `/usr/local/bin`, which it creates if it isn't there yet |

It never installs under `/mnt` unless you ask it to. Before it puts anything in place, it checks
that the folder can run programs: a folder on a filesystem mounted `noexec` (such as `/home` and
`/tmp` on TrueNAS) can't, and it tells you so and suggests another folder.

Options (run it with `--help` to see them too):

| Option | Environment variable | What it does |
|---|---|---|
| `--dir DIR` | `INSTALL_DIR` | Install into `DIR` (a full path) instead. |
| `--version VERSION` | `REBALANCE_VERSION` | Install a particular release, such as `v2.0.0`. The default is the latest. |
| `--uninstall` | | Remove `rebalance`. Saved progress files are kept. |
| `-h`, `--help` | | Show the installer's help. Doesn't need root. |
| | `REBALANCE_BASE_URL` | Download from this address instead of GitHub (used for testing). |

Options go after `bash -s --`, and environment variables after `sudo`:

```sh
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash -s -- --dir /opt/bin
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo INSTALL_DIR=/opt/bin bash
```

If you'd like to read the installer before running it:

```sh
curl -fsSLO https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh
less install.sh
sudo sh install.sh
```

#### Manual download (Linux)

Each release has a program for each system, plus a `.sha256` file to check it with. Run `uname -m`
to see which one you need: `x86_64` means `amd64`, and `aarch64` means `arm64`.

```sh
arch=amd64   # or arm64
base=https://github.com/astundzia/go-zfs-rebalance/releases/latest/download
curl -fsSLO "$base/rebalance-linux-$arch"
curl -fsSLO "$base/rebalance-linux-$arch.sha256"
sha256sum -c "rebalance-linux-$arch.sha256"    # should say: OK
sudo install -m 755 -o root -g 0 "rebalance-linux-$arch" /usr/local/bin/rebalance
```

`install` puts in a copy owned by root, so only root can change a program you'll run with `sudo`.
On TrueNAS, `/usr/local/bin` is read-only, so use `/root/.local/bin` instead
(`sudo install -d -m 755 /root/.local/bin` first).

#### Manual download (macOS)

```sh
arch=arm64   # Apple silicon; use amd64 on an Intel Mac
base=https://github.com/astundzia/go-zfs-rebalance/releases/latest/download
curl -fsSLO "$base/rebalance-darwin-$arch"
curl -fsSLO "$base/rebalance-darwin-$arch.sha256"
shasum -a 256 -c "rebalance-darwin-$arch.sha256"    # should say: OK
sudo install -d -m 755 -o root -g wheel /usr/local/bin
sudo install -m 755 -o root -g wheel "rebalance-darwin-$arch" /usr/local/bin/rebalance
```

Keep the downloaded file's name until after the check: the `.sha256` file refers to it by name.

#### A specific version

Use the installer's `--version` option:

```sh
curl -fsSL https://github.com/astundzia/go-zfs-rebalance/releases/latest/download/install.sh | sudo bash -s -- --version v2.0.0
```

For a manual download, swap `releases/latest/download` for `releases/download/v2.0.0` in the
commands above. Every release is listed on the
[Releases page](https://github.com/astundzia/go-zfs-rebalance/releases), and each one includes a
`checksums.txt` covering all its files.

#### From source

You need Go 1.26 or newer, and no C compiler. Go 1.27.1 or newer is best: older Go releases have
security bugs in `os.Root`, the part of Go that keeps `rebalance` inside the folder. With Go's
default settings, an older Go downloads 1.27.1 for the build by itself.

```sh
go install github.com/astundzia/go-zfs-rebalance/v2/cmd/rebalance@latest
```

This puts the program in `$(go env GOPATH)/bin`. Its `--version` will say `dev`. To build from a
copy of the code instead:

```sh
git clone https://github.com/astundzia/go-zfs-rebalance.git
cd go-zfs-rebalance
make build           # makes bin/rebalance
sudo make install    # copies it to /usr/local/bin (add BINDIR=/some/folder to change that)
```

### Command-line options

```
rebalance [options] <folder>
```

Give exactly one folder. Options can go before or after it. Everything under the folder is
included, including child datasets mounted inside it, but not `.zfs` snapshot folders. To give a
folder whose name starts with `-`, put `--` before it.

This is what `rebalance --help` prints:

```
rebalance rewrites files in place, so ZFS spreads them across all your drives.

Usage: rebalance [options] <folder>

Examples:
  See how evenly the pool is filled (this changes nothing):
    rebalance --report /mnt/tank/media
  Rebalance, with a before-and-after table:
    rebalance --vdev-report /mnt/tank/media
  Carry on after stopping part-way:
    rebalance --resume /mnt/tank/media

Options:
  --report              Show how full each vdev is, and change nothing
  --vdev-report         Also show that table before and after the run
  --resume              Carry on where the last run for this folder stopped
  --passes N            Rewrite each file at most N times in all (default 1)
  --concurrency N       Files to rewrite at once (default: half the CPU cores)
  --process-hardlinks   Also rewrite hardlinked files, keeping them linked
  --no-cleanup          Keep temporary files left by an interrupted run
  --no-random           Go through files in folder order, not at random
  --checksum TYPE       How each copy is checked: sha256 (default) or md5
  --size-threshold MB   Only list rewritten files of at least this size
  --halt-on-missing     Stop if a file disappears before it is rewritten
  --filename-only       Show file names without their folders
  --db FILE             Keep progress in FILE instead of the usual place
  --debug               Show more detail
  --version             Show the version
  -h, --help            Show this help

More help: https://github.com/astundzia/go-zfs-rebalance#readme
```

In more detail:

| Option | What it does | Default |
|---|---|---|
| `--report` | Prints how full each vdev in the folder's pool is, then stops. Changes nothing, and doesn't touch the saved progress. | off |
| `--vdev-report` | Prints that table before the run, and a before-and-after comparison at the end (also after Ctrl+C or an early stop). | off |
| `--resume` | Carries on from this folder's saved progress, skipping files that are already done. Without it, a run starts fresh. | off |
| `--passes N` | The most times any file is rewritten in total, across resumed runs. One run rewrites each file at most once. See [Where progress is saved](#where-progress-is-saved). | 1 |
| `--concurrency N` | How many files are rewritten at the same time. `0` means automatic. Anything above 128 is lowered to 128. | half the CPU cores, at least 2 |
| `--process-hardlinks` | Also rewrites hardlinked files, keeping all their names linked together. | off (skipped) |
| `--no-cleanup` | Keeps the temporary files (`.zfs-rebalance.<random>.tmp`) left by a run that was stopped hard, instead of removing them. The old name, `--no-cleanup-balance`, still works. | off (removed) |
| `--no-random` | Works through files in folder order, instead of a random order. | random |
| `--checksum TYPE` | How each copy is checked against the original: `sha256` or `md5`. If you leave the type out, so the folder ends up in its place, it tells you. | `sha256` |
| `--size-threshold MB` | Only lists rewritten files of at least this many megabytes (MiB). Smaller files are still rewritten, and `--debug` lists them. | 0 (list all) |
| `--halt-on-missing` | Stops the run if a file disappears before it's rewritten, instead of skipping it. | off |
| `--filename-only` | Shows just file names in the log, without their folders. | off |
| `--db FILE` | Keeps the saved progress in `FILE` instead of the usual place. A fresh run only replaces `FILE` if it's a `rebalance` progress file; anything else is left alone, and the run stops with a message. | [see below](#where-progress-is-saved) |
| `--debug` | Shows more detail, such as where progress is saved and why a check was skipped. | off |
| `--version` | Prints the version. | |
| `-h`, `--help` | Prints the help above. | |

The tables (`--report` and `--vdev-report`) go to standard output. Everything else (the log,
warnings and progress) goes to standard error. Colours are only used on a terminal, and never
when `NO_COLOR` is set. If you save the output to a file inside the folder you're rebalancing,
that file is left alone. If the output goes to a program that quits early (such as `head`), the
run stops gently, as if Ctrl+C had been pressed once.

### Reading the tables

`--report` and the start of `--vdev-report` show each vdev that holds your data:

- **SIZE**, **ALLOC**: the vdev's size and how much of it is used, as `zpool list -v` shows them.
- **USED%**: how full the vdev is.
- **spread**: the gap between the fullest and the emptiest vdev, in percentage points ("pts").
- **also:** lists any special, dedup, log, cache or spare vdevs. They hold other kinds of data,
  so they don't count towards the spread.

The before-and-after comparison shows each vdev's USED% before and after the run, and how much
data it gained (`+`) or lost (`-`). A `-` instead of a number means the vdev wasn't there at that
time. Any `note:` lines under it explain why the numbers might not tell the whole story: snapshots
holding old data, ZFS still freeing space, or block cloning during the run.

These reports use the `zfs` and `zpool` commands, which TrueNAS and other OpenZFS systems already
have.

### Where progress is saved

Each folder gets its own progress file, recording which files have been rewritten:

| Run as | Folder |
|---|---|
| root (with `sudo`) on TrueNAS or Linux | `/root/.local/state/go-zfs-rebalance/` |
| root (with `sudo`) on macOS | `/var/root/.local/state/go-zfs-rebalance/` |
| your own user | `~/.local/state/go-zfs-rebalance/` |
| anyone, with `XDG_STATE_HOME` set | `$XDG_STATE_HOME/go-zfs-rebalance/` |

With `sudo` (and no `XDG_STATE_HOME`) it uses root's own home folder, even where `sudo` keeps your
`HOME` (as macOS does), so it never puts root-owned folders in your home. Add `--debug` to see the
exact path. `--db FILE` puts the progress file somewhere else.

How it's used:

- **Without `--resume`**, a run starts fresh: that folder's saved progress is cleared first (with
  a warning saying how much was cleared), and every file is rewritten once. If a `--db` file held
  progress for a different folder, the warning names that folder.
- **With `--resume`**, files that are already done are skipped, so an interrupted run carries on
  where it stopped. Files that were skipped (for example immutable ones) aren't done, so they're
  tried again, in case the reason has gone away. Once everything is done, `--resume` simply says
  there's nothing to rebalance. If the only files left are skipped again for a reason that lasts
  (such as an immutable mark or another owner), it says that nothing new was rebalanced, and why.
- **`--passes N`** is the most times a file is rewritten in total, across resumed runs. One run
  never rewrites a file twice. With the default of 1, `--resume` finishes off a run. To rewrite
  everything a second time, use `--resume --passes 2`. Most people never need that.

The same folder also holds `run.lock`, which makes sure only one `rebalance` using that folder runs
at a time. With `--db`, the lock sits next to that file instead, so two `--db` files in the
**same** folder share one lock, and the second run won't start while the first is going. Only runs
whose progress is in **different** folders can go at once, such as `--db` files in two separate
folders, or another user's runs. They still can't trip over each other's files: a file that
another run is working on is skipped as busy, and a temporary copy that another run is still using
is never removed. But each run sees the other's changes as another program's, so folders they both
work in may keep a new modified time, and each run's summary says how many. It's safe to delete
the progress files and `run.lock` once you're done.

### How it works

For each file, `rebalance`:

1. **Makes an empty copy**: a new, hidden file in the same folder, named
   `.zfs-rebalance.<random>.tmp`. Before any data goes in, it gets the original's owner, group,
   ACL and permissions (without setuid or setgid for now), so nobody can read the copy who couldn't
   read the original. Immutable and append-only files are skipped before this step.
2. **Copies the data**. It reads and writes every byte itself, so ZFS has to store the data on new
   blocks, spread across all your vdevs. Long runs of zeros are skipped, so sparse files stay
   sparse. On Linux, the original is read without changing its access time.
3. **Checks the copy**: makes sure it's on disk, reads it back, and compares its checksum with the
   original's.
4. **Copies everything else**: the other extended attributes, setuid and setgid (only now that the
   owner is right), file flags such as nodump, the ZFS project ID and DOS attributes, the access
   and modified times, and, on macOS, the ACL last of all (so a "deny" entry in it can't block the
   earlier steps). Then it checks that they all match the original.
5. **Makes sure nothing changed**: if the original was modified, replaced or moved while it was
   being copied, the copy is thrown away and the file is skipped. (While it works on a file, it
   also holds an `flock` lock on it, so if another `rebalance` run already has the file, it's
   skipped as busy before anything is copied.)
6. **Swaps it in**, in one step: the copy is renamed over the original. At every moment, the name
   points to either the complete original or the complete, checked copy. If something read the
   hidden copy just before the swap and so changed its access time, the time is put back.
7. **Records it** in the progress file. Once a folder's files are done, its own modified time is
   put back, so tools that watch folder times don't rescan everything. If something else changed
   the folder in the meantime, its new time is left alone, and the summary says how many folders
   that happened to. If a time can't be put back, a warning says so, and the summary counts those
   folders too.

If any step fails, or the run is stopped, the copy is deleted and the original is left exactly as
it was.

With `--process-hardlinks`, a hardlinked file is copied once. Then each of its names is switched
over to the new copy, one at a time, with a check before each switch.

Files that are rewritten pick up the dataset's current settings, such as compression and
recordsize.

### Safety details and limits

- **Block cloning is avoided.** On OpenZFS 2.2 and newer, an ordinary file copy can be done by
  "block cloning": the new file just points at the old blocks, so nothing moves. `rebalance`
  copies the data itself so that can't happen. During a `--vdev-report` run, the pool's block
  cloning counter is checked every 10 seconds, and a note says so if it ever grows.
- **Files that are open for writing.** If a program changes a file while it's being copied, the
  file is skipped. A file that another `rebalance` run has locked is skipped as busy and left
  alone, and so is one a program has locked with `flock` (on macOS, other kinds of file lock count
  too). But on Linux, the byte-range locks that databases and Samba use aren't seen, and most
  programs don't lock the files they use at all, and a program that
  already has the file open can still write to the old copy just after the swap, and that write
  would be lost. So stop apps that write to the folder (VMs, databases, download clients) while
  it runs.
- **New inode numbers and change times.** Every rewritten file gets them, and a new creation time.
  Backup tools and incremental `zfs send` will see the files as changed (see
  [the FAQ](#why-does-my-backup-tool-or-zfs-send-think-everything-changed)). Modified times are
  kept.
- **Snapshots and deduplication** keep the old blocks in use. See the warnings in
  [the FAQ](#what-do-the-warnings-at-the-start-mean).
- **File flags.** On Linux, flags such as nodump (`chattr +d`), ZFS project IDs and ZFS DOS
  attributes are kept. Immutable and append-only files (`chattr +i` / `+a`, or the ZFS
  equivalents), and files protected from deletion by ZFS's nounlink attribute, can't be replaced,
  so they're skipped and left exactly as they were. On macOS, files marked with `chflags uchg`,
  `uappnd` or `sunlnk` are skipped the same way, and other `chflags` flags (such as `hidden`)
  aren't kept.
- **Project IDs in `chattr +P` folders.** A folder set with `chattr +P` gives its project ID to
  every new file in it, and ZFS refuses to move a file with a different ID into it. So a file
  whose project ID is different from its folder's can't be swapped for a copy. It's skipped, with
  that reason in the log and the summary, and left untouched, still with its own ID. Files with
  the folder's ID, the usual case, are rewritten as normal.
- **Permissions that can't be kept exactly.** On TrueNAS SMB shares (`aclmode=restricted`), a
  setuid or setgid file that also has ACL entries of its own can't get those bits back on a new
  copy, so it's skipped and left untouched. Its log line says just that: its setuid/setgid bit
  can't be put back on a copy while it has ACL entries of its own. Like every skipped file, it
  doesn't change the exit code.
- **Without `sudo`**, only your own files are rewritten: ones you own, with a group you're in.
  Everything else is skipped before anything is copied.
- **`trusted.*` extended attributes** (used by a few system tools, such as overlayfs) are only
  visible to root. If your files have them, run it with `sudo`. File capabilities (set with
  `setcap`) are kept too, when run with `sudo`.
- **Hardlinks.** A hardlinked file with names outside the folder is skipped, and its names are
  listed in the log. Once it starts switching a hardlinked file's names over to the new copy, one
  or two Ctrl+Cs let it finish. Only a third Ctrl+C, `kill -9` or a power cut at that moment can
  leave those names as separate, identical copies instead of links to one file. No data is lost.
- **After a third Ctrl+C, `kill -9` or a power cut**, the folders it was working in may keep a new
  modified time, because it quits without tidying up. Nothing else changes, and the next run
  removes any leftover temporary files. One or two Ctrl+Cs don't have this problem.
- **Running out of space.** If the pool or the dataset is full, or the dataset reaches its quota,
  the run stops (exit code 3) rather than failing every file after it. When a quota was the cause,
  the message says a quota was reached (not that the pool ran out of space), and suggests raising
  the quota or freeing some space. It still puts folder times back: ZFS only hands back the space
  of the copies it threw away at its next write-out (usually within 5 to 10 seconds), so it keeps
  trying for up to 15 seconds. If a user or group quota is
  reached while the dataset still has room, only that owner's files are skipped, and the run
  carries on.
- **It stays inside the folder.** It never follows a symlink out of it, and only ever removes its
  own temporary files (`.zfs-rebalance.<12 hex digits>.tmp`), and never one that another run is
  still using.
- **Only Linux and macOS are supported.** On Windows, permissions, owners and alternate data
  streams can't be kept, so it refuses to run.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Finished. Files that were skipped (such as hardlinked, immutable or busy ones, other users' files without `sudo`, or ones whose permissions can't be kept exactly) don't change this. |
| `1` | Some files couldn't be rewritten, and each was left as it was. Or the folder couldn't be read. |
| `2` | Didn't start, so nothing was changed. For example: a mistake in the options, the folder doesn't exist, another run is in progress, a problem with the progress file, an unsupported system, or `--report` on a folder that isn't on ZFS. |
| `3` | Stopped early: a file went missing (with `--halt-on-missing`), or the pool or dataset ran out of space (or reached its quota). |
| `130` | Stopped by Ctrl+C, by another stop signal, because the terminal or SSH session closed, or because the program reading its output (such as `head`) quit. |

### Building and testing

You need Go 1.26 or newer (1.27.1 or newer for release files, which `make dist` checks) and
`make`. `make lint` also needs [shellcheck](https://www.shellcheck.net/). On Linux, `make test`
also needs a C compiler (gcc or clang), because Go's race detector uses one there;
`make test-short` doesn't.

```sh
make build                   # bin/rebalance for this computer
make test                    # all tests, with the race detector
make test-short              # just the quick tests
make lint                    # go vet (this system, Linux, macOS, Windows), staticcheck, shellcheck
make dist VERSION=v2.0.0     # release files in dist/
make help                    # list every target
```

`make dist` builds static programs for Linux and macOS (amd64 and arm64), each with a `.sha256`
file, plus `checksums.txt` and `install.sh`.

A few tests need root (and some need Linux), and skip themselves otherwise. Their names contain
`Root`. To run them:

```sh
for pkg in fileutil rebalance; do
  go test -c -o "/tmp/$pkg.test" "./internal/$pkg"
  (cd "internal/$pkg" && sudo "/tmp/$pkg.test" -test.run Root -test.v)
done
```

The tests work in the system's temporary folder. To test ZFS-only behaviour too (DOS attributes,
NFSv4 ACLs, project IDs), set `REBALANCE_TEST_DIR` to a folder on a ZFS dataset, for example
`REBALANCE_TEST_DIR=/mnt/tank/gotest`. Tests that run a program they make put it there too, and
skip themselves if the folder they'd use can't run programs (a filesystem mounted `noexec`, such
as `/tmp` on TrueNAS).

Every push and pull request is checked on GitHub Actions: vet, staticcheck and the race-enabled
tests on Ubuntu and macOS; the root-only tests, as root on Ubuntu (the job fails unless at least
one of them really ran as root); and installer tests on Ubuntu and macOS against locally served
release files.

### How it's tested

This tool rewrites every file you point it at, so it's tested in layers, from small and fast to
slow and real:

| Layer | Where | What it proves |
|---|---|---|
| **Unit tests** | `go test -race ./...`, on every push (Ubuntu and macOS) | The safe-swap steps one at a time: copies match byte for byte; the empty copy gets its owner and ACL before any data; owners, modes, times, xattrs and ACLs carry over; nothing is left behind when a copy is cancelled or fails, and the original's access time is unchanged; a file that changes mid-copy is left alone, and so is one another program has locked; symlinks and pipes planted where the temporary file goes are never followed; a temporary file another run is using is never removed; the copy can't take the block-cloning shortcut; a full quota is told apart from a full pool; other users' files are skipped up front without `sudo`; folder times are only put back when nothing else changed them |
| **Root-only tests** | CI, as root on Ubuntu (the job fails unless at least one really ran as root) | Ownership and setuid bits are kept for files owned by someone else; inode flags (such as nodump), file capabilities and project IDs are kept; a file whose project ID its `chattr +P` folder won't take, and immutable and append-only files, are skipped and left alone. The project-ID and ZFS-only tests need `REBALANCE_TEST_DIR` on ZFS, so they run in the lab |
| **Whole-program tests** | `cmd/rebalance` tests, on every push | Options (including after the folder), exit codes, Ctrl+C timing, a closed output pipe and `--resume`, the run lock, where progress is saved under `sudo`, refusing a `--db` file that isn't a progress file, friendly messages and hints, escaping of strange file names, and the report tables against real `zpool list` output |
| **Installer tests** | CI, on Ubuntu and macOS | The one-line install as root, and without root (it shows the exact command to run); folders mounted `noexec` noticed before anything runs or is created; a program that won't start; an install cut short by a closed output pipe; leftovers of an earlier install cleaned up; the TrueNAS and macOS default folders; a bad checksum is refused |
| **Real-ZFS lab** | Before each release, on two VMs | Everything above on real pools: a Linux VM with OpenZFS 2.2 and a TrueNAS SCALE 25.10 VM with OpenZFS 2.3 |

In the lab, each pool starts as one vdev about half full of deliberately awkward test data. A
second, empty vdev is added, and the tool is run the way a person would: installed with the
one-liner, then `--report`, then `--vdev-report`. A run only counts as a pass if every file's
content, owner, mode, timestamps, xattrs and ACLs are identical afterwards, and every file has a
new inode. The new inode is proof it was really written again. The pool's block-clone counter must
not move, and the spread between vdevs has to drop. The lab also checks stopping and resuming
(including a closed output pipe), hardlinks, running out of space and quotas, files changing
mid-copy or locked by another program, snapshots, runs without `sudo`, file flags, capabilities,
project IDs and immutable files, two runs at once, tmux on TrueNAS, the installer on TrueNAS's
`noexec` `/home`, reboots on TrueNAS, and a side-by-side run of version 1 to confirm its bugs are
gone.

The full test plan, the scripts, the latest results, and how to build the lab yourself on any
Linux machine with KVM are in [test/lab/README.md](test/lab/README.md).

### Releasing

1. Add a section for the new version at the top of [CHANGELOG.md](CHANGELOG.md), headed like
   `## [2.0.1] - 2026-10-20`.
2. Merge it to `main`, then tag and push:

   ```sh
   git tag v2.0.1
   git push origin v2.0.1
   ```

The release workflow runs the tests, builds the release files with `make dist` using the newest
Go 1.27 (older Go releases have security bugs in `os.Root`), and publishes a GitHub release with
the binaries, `.sha256` files, `checksums.txt` and `install.sh`. The release notes are taken from
that version's CHANGELOG section. A tag with a `-` in it, such as `v2.1.0-rc.1`, is published as
a pre-release.

### Contributing

Contributions are welcome. Please open an issue or a pull request, and run `make lint test` first.

## Credits

This project was inspired by Markus Ressel's
[zfs-inplace-rebalancing](https://github.com/markusressel/zfs-inplace-rebalancing) script, which
showed how simple and effective rewriting files in place can be. Thanks also to the ZFS community
for sharing so much about how ZFS places data.

## License

MIT. See [LICENSE](LICENSE).

## Disclaimer

Please keep a backup of your data before running any tool that rewrites it, this one included.
`rebalance` checks every copy and never removes an original before its replacement is ready, but
hardware, other software and plain bad luck can still surprise anyone.

- Try it on a test dataset first, to see how it behaves on your system.
- Expect a lot of disk activity while it runs.
- How much it helps depends on your pool, your data and your hardware.
- It's provided as is, under the MIT License. The authors can't be held responsible for any data
  loss or other problems.
