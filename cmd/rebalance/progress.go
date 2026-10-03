package main

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/rebalance"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// progressInterval is how often a progress line is logged during a run.
const progressInterval = time.Minute

// progressSource is the part of *rebalance.Rebalancer that progress reporting reads.
type progressSource interface{ Progress() rebalance.Progress }

// startProgress logs a progress line every interval until the returned function is called. That
// function returns once the last line is written.
func startProgress(log *logrus.Logger, src progressSource, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				log.WithField("op", opProgress).Info(progressLine(src.Progress(), now))
			}
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// progressLine describes a running Execute, such as
// "Progress: 1,234 of 5,000 files, 1.2 TiB of 5.0 TiB (24%), 110.5 MB/s, about 3h 12m left".
// The percentage and time left are worked out from bytes, since big files take longer.
func progressLine(p rebalance.Progress, now time.Time) string {
	var frac float64
	pct := 0 // worked out separately, so rounding never shows 98% for 99%
	switch {
	case p.BytesTotal > 0:
		frac = float64(p.BytesDone) / float64(p.BytesTotal)
		pct = int(p.BytesDone * 100 / p.BytesTotal)
	case p.Total > 0:
		frac = float64(p.Done) / float64(p.Total)
		pct = p.Done * 100 / p.Total
	}
	line := fmt.Sprintf("Progress: %s of %s, %s of %s (%d%%)", thousands(p.Done), countFiles(p.Total),
		zfs.FormatBytes(uint64(p.BytesDone)), zfs.FormatBytes(uint64(p.BytesTotal)), pct)
	elapsed := now.Sub(p.Started)
	if p.Started.IsZero() || elapsed <= 0 {
		return line
	}
	line += ", " + formatRate(float64(p.BytesRewritten)/elapsed.Seconds())
	if frac > 0 && frac < 1 {
		line += ", " + timeLeft(time.Duration(float64(elapsed)*(1-frac)/frac))
	}
	return line
}

// logSummary ends a run with what happened and what to do next.
func logSummary(log *logrus.Logger, s rebalance.Summary, o options) {
	took := formatDuration(s.Duration)
	switch s.Stopped {
	case rebalance.None:
		nothingNew := s.NothingNew()
		switch {
		case s.Total == 0:
			log.Info("Finished: there was nothing to rebalance.")
		case nothingNew != "":
			log.Info(nothingNew)
		default:
			log.Infof("Finished in %s: %s.", took, rebalancedText(s))
		}
	case rebalance.Interrupted:
		log.Warnf("Stopped when asked, after %s: %s.", took, rebalancedText(s))
	case rebalance.MissingFile:
		log.Warnf("Stopped after %s because a file went missing (--halt-on-missing): %s.", took, rebalancedText(s))
	case rebalance.NoSpace:
		log.Warnf("Stopped after %s because the pool ran out of free space: %s.", took, rebalancedText(s))
	}
	if line := skippedText(s.Skipped); line != "" {
		log.Info(line)
	}
	if s.Failed > 0 {
		log.Warnf("%s couldn't be rebalanced — see the messages above. Each was left exactly as it was.", countFiles(s.Failed))
	}
	if n := s.FolderTimesNotRestored; n > 0 {
		log.Warnf("Couldn't put back the modified time of %s, so %s when the run changed %s — see the warnings above.",
			choose(n, "1 folder", thousands(n)+" folders"), choose(n, "it shows", "they show"), choose(n, "it", "them"))
	}

	again := "run the same command again with --resume added"
	if o.resume {
		again = "run the same command again"
	}
	switch {
	case s.Stopped == rebalance.NoSpace:
		log.Infof("Free up some space, then %s to finish the rest.", again)
	case s.Stopped != rebalance.None:
		log.Infof("To finish the rest, %s.", again)
	case s.Failed > 0:
		log.Infof("To try those files again, %s.", again)
	}
	if n := s.Skipped[rebalance.SkipHardlinked]; n > 0 {
		log.Infof("To include the %s with hardlinks, add --process-hardlinks.", countFiles(n))
	}
	if n := s.Skipped[rebalance.SkipBusy]; n > 0 {
		log.Infof("To include the %s that %s in use, %s once %s free.", countFiles(n), choose(n, "was", "were"), again, choose(n, "it's", "they're"))
	}
	if hint := sudoHint(s.Skipped); hint != "" {
		log.Info(hint)
	}
}

// sudoHint suggests running with sudo to include the files skipped because only root may replace
// them, or returns "" when there are none or the run already has root. Other reasons for a skip,
// such as an immutable file, aren't helped by sudo. A run with sudo keeps its progress in root's
// own state folder, so it can't carry on from this run's progress.
func sudoHint(skipped map[rebalance.SkipReason]int) string {
	owner, group := skipped[rebalance.SkipOwner], skipped[rebalance.SkipGroup]
	// Without root, a copy that couldn't be given the file's owner or group is also down to a
	// permission only root has.
	n := owner + group + skipped[rebalance.SkipNoPermission] + skipped[rebalance.SkipOwnerNotKept]
	if n == 0 || isRoot() {
		return ""
	}
	which := "you don't have permission to change"
	switch n {
	case owner:
		which = "owned by other users"
	case group:
		which = choose(n, "in a group you're not in", "in groups you're not in")
	}
	return fmt.Sprintf("To include the %s %s, run it again with sudo (that run starts from the beginning).", countFiles(n), which)
}

// rebalancedText is "rebalanced 3 files (12.3 MiB at 110.5 MB/s)", or "rebalanced 3 of 5 files ..."
// when some weren't.
func rebalancedText(s rebalance.Summary) string {
	text := "rebalanced " + countFiles(s.Rebalanced)
	if s.Rebalanced != s.Total {
		text = fmt.Sprintf("rebalanced %s of %s", thousands(s.Rebalanced), countFiles(s.Total))
	}
	if s.Bytes > 0 {
		text += " (" + zfs.FormatBytes(uint64(s.Bytes))
		if s.Duration >= time.Second {
			text += " at " + formatRate(float64(s.Bytes)/s.Duration.Seconds())
		}
		text += ")"
	}
	return text
}

// skippedText is "Skipped 35 files: 20 already done, 14 hardlinked, 1 leftover .balance file.",
// most common reason first, or "" if nothing was skipped.
func skippedText(skipped map[rebalance.SkipReason]int) string {
	type reason struct {
		why rebalance.SkipReason
		n   int
	}
	var reasons []reason
	total := 0
	for why, n := range skipped {
		if n > 0 {
			reasons = append(reasons, reason{why, n})
			total += n
		}
	}
	if total == 0 {
		return ""
	}
	slices.SortFunc(reasons, func(a, b reason) int {
		return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.why, b.why))
	})
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = r.why.Phrase(r.n)
	}
	return fmt.Sprintf("Skipped %s: %s.", countFiles(total), strings.Join(parts, ", "))
}

// exitCode is the process exit code for a finished Execute.
func exitCode(s rebalance.Summary) int {
	switch s.Stopped {
	case rebalance.Interrupted:
		return exitInterrupted
	case rebalance.MissingFile, rebalance.NoSpace:
		return exitHalted
	}
	if s.Failed > 0 {
		return exitFailed
	}
	return exitOK
}

// countFiles is "1 file" or "1,234 files".
func countFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return thousands(n) + " files"
}

// thousands writes n with commas between groups of three digits, such as "1,234,567".
func thousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if n < 0 {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}

// formatRate is a speed in megabytes (10^6 bytes) per second, such as "110.5 MB/s".
func formatRate(bytesPerSec float64) string {
	return fmt.Sprintf("%.1f MB/s", bytesPerSec/1e6)
}

// formatDuration is how long something took, such as "45s", "4m 05s" or "2h 13m".
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "under a second"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// timeLeft is a rough estimate of the time left, such as "about 3h 12m left".
func timeLeft(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute left"
	case d < time.Hour:
		return fmt.Sprintf("about %d min left", int(d.Minutes()))
	}
	return fmt.Sprintf("about %dh %02dm left", int(d.Hours()), int(d.Minutes())%60)
}
