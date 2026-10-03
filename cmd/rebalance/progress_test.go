package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/rebalance"
	"github.com/sirupsen/logrus"
)

func TestProgressLine(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		p    rebalance.Progress
		want string
	}{
		{"a quarter done",
			rebalance.Progress{Started: now.Add(-time.Hour), Total: 5000, Done: 1234,
				BytesTotal: 1_440_000_000_000, BytesDone: 360_000_000_000, BytesRewritten: 360_000_000_000},
			"Progress: 1,234 of 5,000 files, 335.3 GiB of 1.3 TiB (25%), 100.0 MB/s, about 3h 00m left"},
		{"nearly done",
			rebalance.Progress{Started: now.Add(-10 * time.Minute), Total: 10, Done: 9,
				BytesTotal: 1000 << 20, BytesDone: 990 << 20, BytesRewritten: 990 << 20},
			"Progress: 9 of 10 files, 990.0 MiB of 1000.0 MiB (99%), 1.7 MB/s, less than a minute left"},
		{"empty files only",
			rebalance.Progress{Started: now.Add(-20 * time.Minute), Total: 4, Done: 2},
			"Progress: 2 of 4 files, 0 B of 0 B (50%), 0.0 MB/s, about 20 min left"},
		{"not started", rebalance.Progress{Total: 1, BytesTotal: 13},
			"Progress: 0 of 1 file, 0 B of 13 B (0%)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := progressLine(tt.p, now); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

type fakeProgress struct{ calls atomic.Int32 }

func (f *fakeProgress) Progress() rebalance.Progress {
	f.calls.Add(1)
	return rebalance.Progress{Started: time.Now().Add(-time.Second), Total: 3, Done: 1, BytesTotal: 30, BytesDone: 10}
}

func TestStartProgress(t *testing.T) {
	log, buf := testLogger(logrus.InfoLevel)
	src := &fakeProgress{}
	stop := startProgress(log, src, 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	out := buf.String()
	if src.calls.Load() < 2 || !strings.Contains(out, "Progress: 1 of 3 files, 10 B of 30 B (33%)") {
		t.Fatalf("no progress lines after %d reads: %q", src.calls.Load(), out)
	}
	time.Sleep(20 * time.Millisecond)
	if buf.String() != out {
		t.Error("progress was still logged after stop returned")
	}
}

func TestLogSummary(t *testing.T) {
	tests := []struct {
		name    string
		s       rebalance.Summary
		o       options
		want    []string
		notWant []string
	}{
		{"finished",
			rebalance.Summary{Total: 3, Rebalanced: 3, Bytes: 300 << 20, Duration: 135 * time.Second,
				Skipped: map[rebalance.SkipReason]int{rebalance.SkipAlreadyDone: 20, rebalance.SkipHardlinked: 14}},
			options{},
			[]string{"  Finished in 2m 15s: rebalanced 3 files (300.0 MiB at 2.3 MB/s).",
				"  Skipped 34 files: 20 already done, 14 hardlinked.",
				"  To include the 14 files with hardlinks, add --process-hardlinks."},
			[]string{"--resume", "!"}},
		{"nothing to do",
			rebalance.Summary{Skipped: map[rebalance.SkipReason]int{rebalance.SkipAlreadyDone: 1}},
			options{resume: true},
			[]string{"Finished: there was nothing to rebalance.", "Skipped 1 file: 1 already done."},
			[]string{"run the same command"}},
		{"interrupted",
			rebalance.Summary{Total: 5000, Rebalanced: 1234, Remaining: 3766, Duration: 3 * time.Hour, Stopped: rebalance.Interrupted},
			options{},
			[]string{"! Stopped when asked, after 3h 00m: rebalanced 1,234 of 5,000 files.",
				"To finish the rest, run the same command again with --resume added."},
			nil},
		{"interrupted while resuming",
			rebalance.Summary{Total: 2, Remaining: 2, Stopped: rebalance.Interrupted},
			options{resume: true},
			[]string{"To finish the rest, run the same command again."},
			[]string{"--resume added"}},
		{"missing file",
			rebalance.Summary{Total: 4, Rebalanced: 1, Remaining: 2, Stopped: rebalance.MissingFile,
				Skipped: map[rebalance.SkipReason]int{rebalance.SkipMissing: 1}},
			options{haltOnMissing: true},
			[]string{"because a file went missing (--halt-on-missing): rebalanced 1 of 4 files.", "1 missing.", "To finish the rest"},
			nil},
		{"no space",
			rebalance.Summary{Total: 4, Rebalanced: 1, Failed: 1, Remaining: 2, Stopped: rebalance.NoSpace},
			options{},
			[]string{"because the pool ran out of free space", "! 1 file couldn't be rebalanced — see the messages above.",
				"Free up some space, then run the same command again with --resume added to finish the rest."},
			[]string{"quota"}},
		{"quota reached",
			rebalance.Summary{Total: 4, Rebalanced: 1, Failed: 1, Remaining: 2, Stopped: rebalance.NoSpace, QuotaReached: true},
			options{resume: true},
			[]string{"! Stopped after under a second because a quota was reached: rebalanced 1 of 4 files.",
				"  Raise the quota (on this dataset, a dataset above it, or the files' owner or group) or free up some space, " +
					"then run the same command again to finish the rest."},
			[]string{"pool", "Free up some space, then"}},
		{"failures",
			rebalance.Summary{Total: 3, Rebalanced: 1, Failed: 2,
				Skipped: map[rebalance.SkipReason]int{rebalance.SkipOwner: 1}},
			options{},
			[]string{"Finished in under a second: rebalanced 1 of 3 files.",
				"! 2 files couldn't be rebalanced — see the messages above. Each was left exactly as it was.",
				"To try those files again, run the same command again with --resume added.",
				"Skipped 1 file: 1 file owned by someone else.",
				"To include the 1 file owned by other users, run it again with sudo (that run starts from the beginning)."},
			nil},
		{"others' files without root",
			rebalance.Summary{Total: 1000, Rebalanced: 1000,
				Skipped: map[rebalance.SkipReason]int{rebalance.SkipOwner: 1200, rebalance.SkipNoPermission: 34}},
			options{resume: true},
			[]string{"Skipped 1,234 files: 1,200 files owned by someone else, 34 files you aren't allowed to replace.",
				"To include the 1,234 files you don't have permission to change, run it again with sudo (that run starts from the beginning)."},
			[]string{"--resume", "!"}},
		{"plurals",
			rebalance.Summary{Total: 2, Rebalanced: 2, Skipped: map[rebalance.SkipReason]int{
				rebalance.SkipOrphanBalance: 2, rebalance.SkipHardlinksOutside: 1, rebalance.SkipImmutable: 1}},
			options{},
			[]string{"Skipped 4 files: 2 leftover .balance files, 1 file with hardlinks outside the folder, 1 file marked immutable or append-only."},
			nil},
		{"each reason has its own words",
			rebalance.Summary{Total: 20, Rebalanced: 5, Skipped: map[rebalance.SkipReason]int{
				rebalance.SkipImmutable: 6, rebalance.SkipUndeletable: 5, rebalance.SkipProjectID: 4,
				rebalance.SkipMetadata: 3, rebalance.SkipOwnerNotKept: 2, rebalance.SkipBusy: 1}},
			options{},
			[]string{"Skipped 21 files: 6 files marked immutable or append-only, 5 files protected from deletion, " +
				"4 files whose project IDs differ from their folders', 3 files whose permissions can't be kept exactly, " +
				"2 files whose owners or groups couldn't be kept, 1 file in use by another program or run."},
			nil},
		{"a file in use",
			rebalance.Summary{Total: 3, Rebalanced: 2, Skipped: map[rebalance.SkipReason]int{rebalance.SkipBusy: 1}},
			options{},
			[]string{"Finished in under a second: rebalanced 2 of 3 files.", "Skipped 1 file: 1 file in use by another program or run.",
				"To include the 1 file that was in use, run the same command again with --resume added once it's free."},
			[]string{"sudo"}},
		{"files in use while resuming",
			rebalance.Summary{Total: 3, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipBusy: 2}},
			options{resume: true},
			[]string{"To include the 2 files that were in use, run the same command again once they're free."},
			[]string{"--resume"}},
		{"files in groups you're not in",
			rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipGroup: 2}},
			options{},
			[]string{"Skipped 2 files: 2 files in groups you're not in.",
				"To include the 2 files in groups you're not in, run it again with sudo (that run starts from the beginning)."},
			[]string{"owned by"}},
		{"a file in a group you're not in",
			rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipGroup: 1}},
			options{},
			[]string{"Skipped 1 file: 1 file in a group you're not in.",
				"To include the 1 file in a group you're not in, run it again with sudo"},
			nil},
		{"others' files and groups",
			rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipOwner: 3, rebalance.SkipGroup: 1}},
			options{},
			[]string{"Skipped 4 files: 3 files owned by someone else, 1 file in a group you're not in.",
				"To include the 4 files you don't have permission to change, run it again with sudo"},
			nil},
		{"owner not kept without root",
			rebalance.Summary{Total: 2, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipOwnerNotKept: 1}},
			options{},
			[]string{"1 file whose owner or group couldn't be kept.",
				"To include the 1 file you don't have permission to change, run it again with sudo"},
			nil},
		{"no sudo hint for reasons sudo doesn't fix",
			rebalance.Summary{Total: 3, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{
				rebalance.SkipImmutable: 1, rebalance.SkipProjectID: 1, rebalance.SkipMetadata: 1, rebalance.SkipChanged: 1}},
			options{},
			[]string{"Skipped 4 files:"},
			[]string{"sudo", "To include"}},
		{"nothing new",
			rebalance.Summary{Total: 4, Remaining: 0,
				Skipped:    map[rebalance.SkipReason]int{rebalance.SkipAlreadyDone: 20, rebalance.SkipImmutable: 3, rebalance.SkipProjectID: 1},
				RunSkipped: map[rebalance.SkipReason]int{rebalance.SkipImmutable: 3, rebalance.SkipProjectID: 1}},
			options{resume: true},
			[]string{"  Nothing new to rebalance: the 4 remaining files were skipped again (3 files marked immutable or append-only, 1 file whose project ID differs from its folder's).",
				"Skipped 24 files: 20 already done, 3 files marked immutable or append-only, 1 file whose project ID differs from its folder's."},
			[]string{"Finished", "!", "run the same command"}},
		{"folder times not put back",
			rebalance.Summary{Total: 2, Rebalanced: 2, FolderTimesNotRestored: 1},
			options{},
			[]string{"! Couldn't put back the modified time of 1 folder, so it shows when the run changed it — see the warnings above."},
			nil},
		{"several folder times not put back",
			rebalance.Summary{Total: 2, Rebalanced: 2, FolderTimesNotRestored: 1500},
			options{},
			[]string{"! Couldn't put back the modified time of 1,500 folders, so they show when the run changed them — see the warnings above."},
			nil},
		{"a folder changed by another program",
			rebalance.Summary{Total: 2, Rebalanced: 2, FoldersChangedByOthers: 1},
			options{},
			[]string{"  1 folder was changed by another program during the run, so its times were left as they are.\n"},
			[]string{"!", "Couldn't put back"}},
		{"folders changed by another program",
			rebalance.Summary{Total: 2, Rebalanced: 2, FoldersChangedByOthers: 1027},
			options{},
			[]string{"  1,027 folders were changed by another program during the run, so their times were left as they are.\n"},
			[]string{"!"}},
		{"a hardlinked file named",
			rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipHardlinked: 1},
				Hardlinked: []string{"links/shared-outside"}},
			options{},
			[]string{"  This file has hardlinks, so it was left alone:\n", "      links/shared-outside\n",
				"  To include the 1 file with hardlinks, add --process-hardlinks.\n"},
			[]string{"more", "!"}},
		{"hardlinked files named, the first few",
			rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{rebalance.SkipHardlinked: 7},
				Hardlinked: []string{"a/1", "a/2", "b/3", "b/4", "c/5", "c/6", "d/7"}},
			options{},
			[]string{"  These files have hardlinks, so they were left alone:\n",
				"      a/1\n", "      a/2\n", "      b/3\n", "      b/4\n", "      c/5\n",
				"  …and 2 more (--debug lists them all)\n",
				"  To include the 7 files with hardlinks, add --process-hardlinks.\n"},
			[]string{"c/6", "d/7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runAsRoot(t, false)
			log, buf := testLogger(logrus.InfoLevel)
			logSummary(log, tt.s, tt.o)
			mustContain(t, buf.String(), tt.want...)
			mustNotContain(t, buf.String(), tt.notWant...)
		})
	}
}

// runAsRoot makes the run believe it does, or doesn't, have root.
func runAsRoot(t *testing.T, root bool) {
	t.Helper()
	old := isRoot
	isRoot = func() bool { return root }
	t.Cleanup(func() { isRoot = old })
}

// TestNoSudoHintAsRoot checks that a run that already has root isn't told to use sudo.
func TestNoSudoHintAsRoot(t *testing.T) {
	runAsRoot(t, true)
	log, buf := testLogger(logrus.InfoLevel)
	logSummary(log, rebalance.Summary{Total: 1, Rebalanced: 1, Skipped: map[rebalance.SkipReason]int{
		rebalance.SkipOwner: 1, rebalance.SkipGroup: 1, rebalance.SkipOwnerNotKept: 1, rebalance.SkipNoPermission: 1}}, options{})
	mustContain(t, buf.String(), "Skipped 4 files: 1 file in a group you're not in, 1 file you aren't allowed to replace, "+
		"1 file owned by someone else, 1 file whose owner or group couldn't be kept.")
	mustNotContain(t, buf.String(), "sudo")
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		s    rebalance.Summary
		want int
	}{
		{rebalance.Summary{Total: 2, Rebalanced: 2}, exitOK},
		{rebalance.Summary{Total: 2, Rebalanced: 1, Failed: 1}, exitFailed},
		{rebalance.Summary{Total: 2, Failed: 1, Stopped: rebalance.NoSpace}, exitHalted},
		{rebalance.Summary{Total: 2, Stopped: rebalance.MissingFile}, exitHalted},
		{rebalance.Summary{Total: 2, Failed: 1, Stopped: rebalance.Interrupted}, exitInterrupted},
	}
	for _, tt := range tests {
		if got := exitCode(tt.s); got != tt.want {
			t.Errorf("exitCode(%+v) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestNumberFormatting(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4321: "-4,321"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		0: "under a second", 45 * time.Second: "45s", 245 * time.Second: "4m 05s", 2*time.Hour + 13*time.Minute: "2h 13m", 50 * time.Hour: "50h 00m",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := countFiles(1); got != "1 file" {
		t.Errorf("countFiles(1) = %q", got)
	}
	if got := choose(1, "one", "many") + choose(2, "one", "many"); got != "onemany" {
		t.Errorf("choose = %q", got)
	}
}
