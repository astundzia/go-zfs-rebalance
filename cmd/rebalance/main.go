// Command rebalance rewrites every file in a folder in place, so that ZFS spreads the data across
// all the drives in the pool. Each file is copied, checked and swapped in atomically, keeping its
// contents, owner, permissions, extended attributes, ACLs and timestamps.
//
// Run it with --help for the options, or see the README.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/database"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/rebalance"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// version is set when building a release, with -ldflags "-X main.version=v2.0.0".
var version = "dev"

// Exit codes.
const (
	exitOK          = 0
	exitFailed      = 1   // some files couldn't be rebalanced; each was left as it was
	exitUsage       = 2   // bad options, unusable setup, or another run in progress; nothing was changed
	exitHalted      = 3   // stopped early because a file went missing (--halt-on-missing) or space ran out
	exitInterrupted = 130 // stopped by Ctrl+C or another signal
)

// lockFileName is the run lock's name inside its folder (see database.AcquireLock). The run must
// never rewrite it: a new inode would let a second run take the lock while this one holds it.
const lockFileName = "run.lock"

// goos is runtime.GOOS. Tests change it to check the message on unsupported systems.
var goos = runtime.GOOS

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command. It returns the exit code rather than exiting, so that deferred
// cleanup, such as closing the progress file and releasing the run lock, always happens.
// Tables go to stdout; log lines and errors go to stderr.
func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args)
	switch {
	case errors.Is(err, errNoArgs):
		writeHelp(stderr)
		return exitUsage
	case err != nil:
		fmt.Fprintf(stderr, "rebalance: %v\nRun \"rebalance --help\" to see the options.\n", err)
		return exitUsage
	case o.help:
		writeHelp(stdout)
		return exitOK
	case o.version:
		fmt.Fprintf(stdout, "go-zfs-rebalance %s\n", version)
		return exitOK
	}
	if goos != "linux" && goos != "darwin" {
		fmt.Fprintf(stderr, "rebalance: this tool only works on Linux and macOS, not %s, so nothing was changed\n", goos)
		return exitUsage
	}
	root, err := resolveRoot(o.path)
	if err != nil {
		fmt.Fprintf(stderr, "rebalance: %v\n", err)
		return exitUsage
	}
	if o.report {
		return report(root, stdout, stderr)
	}
	return rebalanceFolder(o, root, stdout, newLogger(stderr, o.debug, o.filenameOnly))
}

// rebalanceFolder is a normal run: it rewrites the files in root and returns the exit code.
func rebalanceFolder(o options, root string, stdout io.Writer, log *logrus.Logger) int {
	// The first signal cancels gentle: the files in progress are finished and nothing new is
	// started. The second cancels hard: the files in progress are abandoned, each left as it was.
	hard, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	gentle, stopGently := context.WithCancel(hard)
	defer stopGently()
	defer watchSignals(log, stopGently, stopNow)()

	concurrency, note := resolveConcurrency(o.concurrency, runtime.NumCPU())
	log.Infof("Rebalancing %s", root)
	log.Info(settingsLine(o, concurrency))
	if note != "" {
		log.Info(note)
	}
	if o.oldNoCleanup {
		log.Info("--no-cleanup-balance is now called --no-cleanup. The old name still works for now.")
	}
	if os.Geteuid() != 0 {
		log.Warn("Not running as root: files owned by other users will be skipped, never changed.")
	}

	lockDir, dbPath, err := statePaths(o.db, root)
	if err != nil {
		log.Errorf("Can't start: %v", err)
		return exitUsage
	}
	release, err := database.AcquireLock(lockDir)
	if err != nil {
		log.Errorf("Can't start: %v", err)
		return exitUsage
	}
	defer func() {
		if err := release(); err != nil {
			log.WithField("reason", err.Error()).Debug("Couldn't release the run lock")
		}
	}()
	db, info, err := database.Open(dbPath, root, o.resume)
	if err != nil {
		log.Errorf("Can't start: %v", err)
		return exitUsage
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.WithField("reason", err.Error()).Warn("Couldn't close the progress file cleanly; a resumed run may redo a few files")
		}
	}()
	logOpenInfo(log, info, db.Path())

	z := &zfsView{client: newZFSClient(), log: log}
	z.lookup(gentle, root)
	z.checkDataset(gentle)
	if gentle.Err() != nil {
		return stoppedBeforeStart(log)
	}

	r, err := rebalance.New(rebalance.Config{
		Root:             root,
		Passes:           o.passes,
		Concurrency:      concurrency,
		ProcessHardlinks: o.processHardlinks,
		Cleanup:          !o.noCleanup,
		RandomOrder:      !o.noRandom,
		Checksum:         o.checksumType,
		HaltOnMissing:    o.haltOnMissing,
		SizeThresholdMB:  o.sizeThresholdMB,
		Exclude:          append(db.Files(), filepath.Join(lockDir, lockFileName)),
		Logger:           log,
		State:            db,
	})
	if err != nil {
		log.Errorf("Can't start: %v", err)
		return exitUsage
	}
	defer r.Close()
	stopOnSignal := context.AfterFunc(gentle, r.Stop)
	defer stopOnSignal()

	log.Info("Looking through the folder to see what needs doing…")
	plan, err := r.Scan(gentle)
	switch {
	case gentle.Err() != nil:
		return stoppedBeforeStart(log)
	case err != nil:
		log.WithField("reason", err.Error()).Error("Couldn't look through the folder, so nothing was changed")
		return exitFailed
	}
	logPlan(log, plan, !o.noCleanup)
	z.checkSpace(gentle, plan, concurrency)
	var before *vdevBefore
	if o.vdevReport {
		before = z.reportBefore(gentle, stdout)
	}
	if gentle.Err() != nil {
		return stoppedBeforeStart(log)
	}

	stopProgress := startProgress(log, r, progressInterval)
	summary, err := r.Execute(hard, plan)
	stopProgress()
	if err != nil {
		log.WithField("reason", err.Error()).Error("Couldn't start rewriting files")
		return exitFailed
	}
	logSummary(log, summary, o)
	if before != nil {
		z.reportAfter(gentle, hard, stdout, before)
	}
	return exitCode(summary)
}

// statePaths returns the folder holding the run lock and the progress file to use for root.
// With --db, the lock sits next to that file.
func statePaths(dbFlag, root string) (lockDir, dbPath string, err error) {
	if dbFlag != "" {
		if dbPath, err = filepath.Abs(dbFlag); err != nil {
			return "", "", fmt.Errorf("couldn't work out the full path of --db %q (%s)", dbFlag, reasonOf(err))
		}
		return filepath.Dir(dbPath), dbPath, nil
	}
	if lockDir, err = database.DefaultDir(); err != nil {
		return "", "", err
	}
	if dbPath, err = database.DefaultPath(root); err != nil {
		return "", "", err
	}
	return lockDir, dbPath, nil
}

func settingsLine(o options, concurrency int) string {
	order := "a random order"
	if o.noRandom {
		order = "folder order"
	}
	line := fmt.Sprintf("Settings: %s at a time, in %s, each copy checked with %s", countFiles(concurrency), order, o.checksumType)
	if o.passes > 1 {
		line += fmt.Sprintf(", each file rewritten up to %d times across resumed runs", o.passes)
	}
	if o.processHardlinks {
		line += ", hardlinked files included"
	}
	if o.haltOnMissing {
		line += ", stopping if a file goes missing"
	}
	if o.noCleanup {
		line += ", leftover temporary files kept"
	}
	return line + "."
}

// logOpenInfo says what happened to the saved progress for this folder.
func logOpenInfo(log *logrus.Logger, info database.OpenInfo, dbPath string) {
	switch {
	case info.Resumed:
		log.Infof("Resuming where the last run stopped (it had rewritten %s).", countFiles(info.PreviousEntries))
	case info.MissingForResume:
		log.Warn("There's no saved progress for this folder to resume, so starting from the beginning.")
	case info.Discarded && info.PreviousEntries > 0:
		log.Warnf("Starting fresh, so the saved progress of an earlier run (%s rewritten) was discarded. Next time, add --resume to carry on where it stopped.",
			countFiles(info.PreviousEntries))
	}
	log.Debugf("Progress is saved in %s", dbPath)
}

// logPlan describes what Scan found, warning about anything that needs a person's attention.
func logPlan(log *logrus.Logger, plan *rebalance.Plan, cleanup bool) {
	if plan.TotalFiles > 0 {
		log.Infof("Found %s to rebalance (%s).", countFiles(plan.TotalFiles), zfs.FormatBytes(uint64(plan.TotalBytes)))
	}
	if n := len(plan.OrphanBalance); n > 0 {
		log.Warn(choose(n,
			"Found a file ending in .balance with no original next to it. It looks like a leftover from version 1 and may be the only copy of a file — please check it. It won't be touched:",
			fmt.Sprintf("Found %s files ending in .balance with no original next to them. They look like leftovers from version 1 and may be the only copies of some files — please check them. They won't be touched:", thousands(n))))
		for _, rel := range plan.OrphanBalance[:min(n, maxListed)] {
			log.WithFields(logrus.Fields{"op": opListed, "path": rel}).Warn()
		}
		if n > maxListed {
			log.Warnf("…and %s more", thousands(n-maxListed))
		}
	}
	if n := len(plan.LegacyBalance) - len(plan.OrphanBalance); n > 0 {
		log.Info(choose(n,
			"Found a file ending in .balance next to one with the same name without it. Version 1 used names like that for its temporary copies, so it may be a leftover worth checking. It will be rewritten like any other file.",
			fmt.Sprintf("Found %s files ending in .balance next to ones with the same name without it. Version 1 used names like that for its temporary copies, so they may be leftovers worth checking. They'll be rewritten like any other file.", thousands(n))))
	}
	if n := len(plan.StaleTemps); n > 0 && !cleanup {
		log.Warn(choose(n,
			"Found a temporary file left behind by an interrupted run. It was kept because of --no-cleanup; run without it to remove it.",
			fmt.Sprintf("Found %s temporary files left behind by an interrupted run. They were kept because of --no-cleanup; run without it to remove them.", thousands(n))))
	}
}

func stoppedBeforeStart(log *logrus.Logger) int {
	log.Warn("Stopped before any files were changed.")
	return exitInterrupted
}

// choose returns one when n is 1, and many otherwise.
func choose(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
