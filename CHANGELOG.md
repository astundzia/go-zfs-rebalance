# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.0.0] - 2026-10-03

### Highlights

Version 2 is mostly about keeping your data safe. Version 1 had bugs that could, in rare cases,
lose a file or quietly change its owner and ACLs. On newer ZFS versions it could also end up doing
nothing at all. All of that is fixed. If you use version 1, please upgrade.

- **Safer rewrites.** Each file is copied, checked, and then swapped in, in a single step. The
  original is never removed first, so a crash or Ctrl+C can't lose it.
- **Everything about a file is kept**: owner, group, permissions, extended attributes, ACLs
  (including TrueNAS's NFSv4 ACLs) and timestamps, plus, on Linux, file flags such as nodump, ZFS
  project IDs and DOS attributes. If anything can't be kept exactly, the file is skipped and left
  exactly as it was.
- **Stop and carry on later** with Ctrl+C and `--resume`.
- **See the difference** with `--report` and `--vdev-report`, which show how full each vdev is.
- **One-line installer** for TrueNAS SCALE, Linux and macOS.

### Breaking changes

- `--passes N` now means the most times a file is rewritten **in total, across `--resume`d
  runs**. A single run rewrites each file at most once. The default is now 1 (it was 10, which
  rewrote every file 10 times), and `--passes 0` is no longer allowed (it meant 999).
- Without `--resume`, a run now starts fresh and clears that folder's saved progress.
- Temporary copies are now hidden files named `.zfs-rebalance.<12 hex digits>.tmp`, instead of
  `<name>.balance`. Files ending in `.balance` are never deleted any more (see Fixed).
- `--no-cleanup-balance` is now called `--no-cleanup`. The old name still works, with a notice.
- Exit codes now say what happened: 0 finished, 1 some files failed, 2 didn't start, 3 stopped
  early (a missing file or no space), 130 stopped with Ctrl+C. Before, a stopped run exited 0.
- `--process-hardlinks` now keeps a hardlinked file's names linked together, instead of splitting
  them into separate copies.
- Log lines look different, and the before/after tables go to standard output.
- Windows builds are gone. Windows can't keep NTFS permissions, owners or alternate data streams,
  so the program now refuses to run there. Linux and macOS are supported.
- The Docker-based build scripts are gone. Plain `make` and Go build everything, with no C
  compiler needed.
- The Go module path is now `github.com/astundzia/go-zfs-rebalance/v2`, so
  `go install github.com/astundzia/go-zfs-rebalance/v2/cmd/rebalance@latest` installs version 2.
  The `pkg/rebalance` package moved to `internal/` and can no longer be imported.

### Added

- `--resume` carries on where an earlier run for the same folder stopped. Progress is now saved
  in `~/.local/state/go-zfs-rebalance/`, one file per folder. With `sudo` that's root's own home
  folder, even on macOS, where `sudo` keeps your `HOME`.
- `--db FILE` keeps the progress somewhere else. A fresh run only replaces `FILE` if it's a
  `rebalance` progress file; any other file is left alone, and the run stops with a message.
- `--report` shows how full each vdev in the pool is, and the "spread" between the fullest and
  emptiest one, then exits without changing anything.
- `--vdev-report` shows that table at the start, and a before-and-after comparison at the end,
  even after Ctrl+C. It waits for ZFS to finish freeing space first, and adds notes about
  snapshots holding old data or block cloning (checked every 10 seconds during the run).
- Safety checks before starting, as friendly warnings: not running as root, the folder isn't on
  ZFS (or the zfs tools aren't available), snapshots exist, deduplication is on (for the folder's
  dataset or any dataset inside it), free space looks tight (judged by the space files really
  take, so sparse files don't set it off), `.balance` leftovers from version 1, and kept temporary
  files.
- Hardlinked files can be rewritten together with `--process-hardlinks`. A hardlinked file with
  names outside the folder is left alone, and its names are listed in the log. The log line for a
  rewritten hardlinked file lists its other names (up to three, then how many more).
- A progress line every minute, with an estimate of the time left, and a summary at the end that
  says what to do next.
- Only one run at a time per progress folder, and a run never removes a temporary file that
  another run is still using.
- A file that another `rebalance` run (or a program using `flock`) has locked is skipped as busy and
  left alone. So two runs on the same folder (with different `--db` files) never copy the same file at
  the same time.
- Runs without `sudo` only rewrite your own files. Files owned by someone else, in a group you're
  not in, or that you aren't allowed to replace, are skipped before anything is copied, and the
  summary says how to include them.
- Immutable and append-only files (`chattr +i` / `+a`, or the ZFS equivalents), and files
  protected from deletion by ZFS's nounlink attribute, are skipped and left alone.
- A file whose project ID is different from that of its `chattr +P` folder is skipped and left
  untouched, because ZFS won't let a copy take its place.
- The summary names each kind of skipped file plainly: owned by someone else, in a group you're
  not in, owner can't be kept, permissions can't be kept exactly, project ID different from its
  folder's, immutable or append-only, protected from deletion, over quota, busy, and so on.
- `--resume` tries skipped files again, in case the reason has gone away. If those are the only
  files left and they're skipped again for a reason that lasts, it says plainly that nothing new
  was rebalanced, and why.
- When a user or group quota is reached, only that owner's files are skipped and the run carries
  on. A full pool or dataset still stops the run.
- Stopping is gentler and clearer: a second Ctrl+C only stops right away if it comes a second or
  more after the first, a third quits at once (without tidying up, like `kill -9`), and the
  messages say what is being stopped (scanning, copying, or waiting for ZFS). Output piped into a
  program that quits early, such as `head`, stops the run gently, with exit code 130.
- On TrueNAS, if `zfs` and `zpool` can't be started because the `sudo` session that started the
  run has ended, a friendly message explains it once (start tmux without `sudo`, then run
  `sudo rebalance` inside it) instead of a raw error. That includes the block-cloning check during
  the run, and it no longer says it's waiting for ZFS to free space when it can't.
- If you save the output to a file inside the folder being rebalanced, that file is left alone.
- A bad option value gets a plain message, such as "--concurrency needs a whole number, like 4".
  An unknown option that looks like a folder name starting with `-` gets a hint to put `--` before
  it.
- `install.sh`, a one-line installer (`curl … | sudo bash`, see the README). It checks the
  download's checksum and that the folder can run programs, and on TrueNAS installs into
  `/root/.local/bin`, never onto your pools. On macOS it creates `/usr/local/bin` if it's missing.
  Without root, it shows the exact command to run, keeping the options you gave. It can also
  install a given version, or uninstall.
- Releases now include `install.sh` and a `checksums.txt` covering every file.
- Automatic checks on every change (tests on Linux and macOS, root-only tests, an installer test)
  and automatic releases from version tags.
- `--version` now reports the real release version.

### Fixed

- **A file could be lost.** The original was deleted *before* the new copy was moved into its
  place. A crash, kill or power cut at that moment left only `<name>.balance`, and the next run's
  cleanup then deleted it. Now the copy replaces the original in one step, and nothing is deleted
  first.
- **Your own `.balance` files were deleted, or emptied.** Files ending in `.balance` were never
  rewritten and were removed by the cleanup. Rewriting `x` also wiped an existing `x.balance`.
- **On OpenZFS 2.2 and newer, nothing was actually moved.** The copy could be done by block
  cloning, which just points at the old blocks. The data is now copied byte by byte, so ZFS really
  writes new blocks.
- **Owners, extended attributes, ACLs and access times were lost.** Run as root, every file became
  owned by root and lost its ACL, even though the docs said they were kept. They are now copied,
  then checked before the swap, and if something reads the hidden copy just before the swap, its
  access time is put back afterwards. On Linux, a file that's skipped or fails also keeps its
  access time, because the original is read without updating it.
- Changes made to a file while it was being copied were lost. They are now noticed, and the file
  is skipped.
- When a file couldn't be rescued after an error, the program still said it had been saved.
- A run that ran out of space failed every remaining file and left half-written copies behind. Now
  it stops cleanly, with exit code 3.
- The macOS release programs stopped with an error as soon as they ran (they were built without the
  C compiler that the old SQLite library needed). The new SQLite library is pure Go, so every
  build works.
- Options placed after the folder (`rebalance /mnt/tank --passes 1`) were silently ignored.
- The progress database was a temporary file deleted at exit, so nothing could be resumed.
- Stopping:
  - Stopping twice (Ctrl+C together with `--halt-on-missing`, or two files going missing) could
    crash the program.
  - Ctrl+C did nothing while the folder was being scanned, and a second Ctrl+C was ignored.
  - Closing the terminal or SSH session wasn't handled.
  - The "forcing exit" after 90 seconds only printed a message.
  - With `--no-cleanup-balance`, Ctrl+C still deleted every `.balance` file.
- "database is locked" errors when working on several files at once, and progress counts that
  could be wrong.
- Each pass walked the whole folder 4 or 5 times, and read each file twice. Now there's one walk,
  and each file is checksummed in the same read that copies it.
- The copy wasn't flushed to disk before the swap, and errors when closing it were ignored.
- Permissions and modified times were only fixed up after the swap.
- The program walked into `.zfs` snapshot folders.
- A folder that didn't exist was reported as a success.
- Files with very long names (over 247 bytes) could never be rewritten.
- A pipe (FIFO) named `<name>.balance` could make the program hang forever.
- On datasets with compression off, sparse files (files with holes) were filled in with real
  zeros. Holes are now kept.
- `--debug` didn't show any debug messages.
- `--filename-only` only applied to some log lines.
- Log lines mangled file names containing " at " or ":", and dropped the reason for errors.
- Colour codes were written even when the output wasn't a terminal.
- Every folder's modified time changed. Now each folder gets its times back once its files are
  done, so tools that watch folder times don't rescan everything. After a quota stop it keeps
  trying for a few seconds while ZFS frees space. A folder that something else changed in the meantime keeps its new
  time, even if that change came in the same clock tick as one of the run's own. If a time can't
  be put back, the summary says so.
- The README's macOS checksum steps didn't work.
- Skip counts in the summary now read properly ("1 leftover .balance file", "2 leftover .balance
  files").

### Security

- **A root run could create setuid-root programs.** The temporary copy got the original's setuid
  and setgid bits while it was still owned by root, so any user's setuid program became a
  setuid-root one. Those bits are now only set after the owner is right.
- **Symlink and FIFO tricks on the temporary file.** A symlink placed at `<name>.balance` was
  followed, so a root run could overwrite any file it pointed to. Temporary files now have random
  names, are created only if nothing is there, never follow symlinks, and must be regular files.
  Their owner, permissions and times are set through the open file, never by name, so a symlink
  put in a temporary file's place can't redirect them. All file access stays inside the folder
  being rebalanced.
- **The temporary copy could be read by others.** It now gets the original's owner, ACL and
  permissions before any data is written into it, so nobody can read it who couldn't read the
  original.
- **Release programs are built with Go 1.27.1 or newer.** Go 1.26.0 has security bugs in `os.Root`
  (GO-2026-4970, GO-2026-4602), which `rebalance` relies on to stay inside the folder. go.mod now
  names that toolchain, CI and releases use the newest Go 1.27, and `make dist` refuses anything
  older.
- **The manual install steps left a program your own account could change, then run as root.** The
  README now uses `sudo install -o root`, as the installer and `sudo make install` already did.
- **Terminal escape codes in file names.** File names were printed as they were, so a crafted name
  could send control codes to your terminal. Unusual characters are now shown escaped.
- Updated the test library, which pulled in a version of yaml.v3 with a known vulnerability
  (CVE-2022-28948). It was only used by the tests.

## [1.0.1] - 2025-04-08

> **Note:** the macOS programs attached to this release don't work: they stop with an error as
> soon as they run, because they were built without the C compiler their SQLite library needed.
> Please use 2.0.0 or later.

### Fixed
- Fixed bug where multiple passes would stop after the first pass when some files failed to rebalance
- Now continues processing through all configured passes even when some files fail

### Added
- Maximum concurrency limit (128) to prevent resource exhaustion
- Enhanced multi-pass capability that continues through all passes
- Updated documentation with improved multi-pass details

## [1.0.0] - 2025-04-06

### Added
- Initial public release
- In-place file rebalancing with checksums verification
- Multi-pass rebalancing for heavily fragmented file systems
- Concurrent processing with automatic thread calculation
- Progress display with pass information
- Database tracking of rebalanced files
- Graceful shutdown handling
- Size-based filtering for log messages
- Hardlink awareness and skipping
- Customizable options for processing

[2.0.0]: https://github.com/astundzia/go-zfs-rebalance/compare/v1.0.1...v2.0.0
[1.0.1]: https://github.com/astundzia/go-zfs-rebalance/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/astundzia/go-zfs-rebalance/releases/tag/v1.0.0
