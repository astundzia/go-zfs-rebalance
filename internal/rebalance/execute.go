package rebalance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sync"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

// replace rewrites one item. It is a variable so tests can simulate failures, such as a full pool,
// that are hard to cause for real.
var replace = fileutil.ReplaceGroup

// Execute rewrites the files in p, Config.Concurrency at a time, and records each success in the
// StateStore. A file that can't be rewritten is always left exactly as it was.
//
// With Config.Cleanup set, the stale temporary files found by Scan are removed first. The run ends
// early when Stop is called, ctx is cancelled (files being copied are abandoned and left as they
// were), the pool runs out of space, or a file goes missing and Config.HaltOnMissing is set;
// Summary.Stopped says which. Folder timestamps changed by the run are put back as each folder is
// finished, and at the end for any left over.
//
// Problems with single files are reported in the Summary and the log, not as an error. Scan again
// before each Execute: running the same Plan twice rewrites its files twice.
func (r *Rebalancer) Execute(ctx context.Context, p *Plan) (Summary, error) {
	if p == nil {
		return Summary{}, errors.New("there's nothing to run yet; scan the folder first")
	}
	start := time.Now()
	r.resetProgress(p, start)
	x := &execution{
		r:       r,
		plan:    p,
		pending: make(map[string]int),
		touched: make(map[string]bool),
		skipped: make(map[SkipReason]int),
	}
	for _, it := range p.Items {
		for _, dir := range itemDirs(it) {
			x.pending[dir]++
		}
	}

	if r.cfg.Cleanup && len(p.StaleTemps) > 0 && !r.halted(ctx) {
		x.removeStaleTemps()
	}

	jobs := make(chan Item, r.cfg.Concurrency)
	var wg sync.WaitGroup
	for range r.cfg.Concurrency {
		wg.Go(func() {
			for it := range jobs {
				if !r.halted(ctx) {
					x.process(ctx, it)
				}
			}
		})
	}
dispatch:
	for _, it := range p.Items {
		if r.halted(ctx) {
			break
		}
		select {
		case jobs <- it:
		case <-r.stopCh:
			break dispatch
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()

	x.restoreAll()
	return x.summary(ctx, start), nil
}

func (r *Rebalancer) halted(ctx context.Context) bool { return r.stopping() || ctx.Err() != nil }

func (r *Rebalancer) resetProgress(p *Plan, start time.Time) {
	r.started.Store(&start)
	r.total.Store(int64(p.TotalFiles))
	r.bytesTotal.Store(p.TotalBytes)
	r.rebalanced.Store(0)
	r.failed.Store(0)
	r.skipped.Store(0)
	r.bytesDone.Store(0)
	r.bytesRewritten.Store(0)
}

// execution is the state of one Execute call.
type execution struct {
	r    *Rebalancer
	plan *Plan

	mu      sync.Mutex
	pending map[string]int  // items not yet finished, per folder
	touched map[string]bool // folders changed by the run whose times haven't been put back yet
	skipped map[SkipReason]int
}

// process rewrites one item and records the outcome.
func (x *execution) process(ctx context.Context, it Item) {
	r := x.r
	x.touch(itemDirs(it)...)
	defer x.finish(it)

	res, err := replace(ctx, r.root, it.Names, r.opts)
	switch {
	case err == nil:
		x.succeeded(it, res)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Abandoned part-way: the file is as it was, and a resumed run will pick it up.
		r.log.WithField("path", it.Names[0]).Debug("Stopped before this file was finished; it was left as it was")
	case errors.Is(err, fileutil.ErrNoSpace):
		x.failed(it, "Ran out of free space, so the run is stopping", err)
		r.stopWith(NoSpace)
	case errors.Is(err, fileutil.ErrOwnership):
		x.skip(it, SkipOwner, err.Error(), logrus.WarnLevel)
	case errors.Is(err, fileutil.ErrModified):
		x.skip(it, SkipChanged, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrLinkMismatch):
		x.skip(it, SkipLinksChanged, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrNotRegular):
		x.skip(it, SkipNotRegular, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrMetadata):
		// The copy couldn't carry every permission, ACL or timestamp exactly (for example an NFSv4
		// ACL that forbids the chmod), so the original was kept as it is.
		x.skip(it, SkipMetadata, err.Error(), logrus.WarnLevel)
	case errors.Is(err, fs.ErrNotExist):
		x.skip(it, SkipMissing, "it isn't there any more", logrus.InfoLevel)
		if r.cfg.HaltOnMissing {
			r.log.WithFields(logrus.Fields{"op": "warning", "path": it.Names[0]}).
				Warn("This file went missing, so the run is stopping, as you asked")
			r.stopWith(MissingFile)
		}
	default:
		x.failed(it, "Couldn't rebalance", err)
	}
}

func (x *execution) succeeded(it Item, res fileutil.Result) {
	r := x.r
	for _, name := range it.Names {
		if _, err := r.state.Increment(name); err != nil {
			r.log.WithFields(logrus.Fields{"op": "warning", "path": name, "reason": reasonOf(err)}).
				Warn("Rebalanced, but couldn't save that it's done, so a resumed run may rewrite it again")
		}
	}
	r.rebalanced.Add(int64(len(it.Names)))
	r.bytesRewritten.Add(res.Size)
	r.bytesDone.Add(it.Size)

	var mbps float64
	if secs := res.Duration.Seconds(); secs > 0 {
		mbps = float64(res.Size) / secs / (1 << 20)
	}
	level := logrus.InfoLevel
	if it.Size < int64(r.cfg.SizeThresholdMB)<<20 {
		level = logrus.DebugLevel
	}
	msg := "Rebalanced"
	if len(it.Names) > 1 {
		msg = fmt.Sprintf("Rebalanced, keeping its %d hardlinked names together", len(it.Names))
	}
	r.log.WithFields(logrus.Fields{"op": "rebalanced", "path": it.Names[0], "size": res.Size, "mbps": mbps}).Log(level, msg)
}

func (x *execution) skip(it Item, why SkipReason, reason string, level logrus.Level) {
	x.mu.Lock()
	x.skipped[why] += len(it.Names)
	x.mu.Unlock()
	x.r.skipped.Add(int64(len(it.Names)))
	x.r.bytesDone.Add(it.Size)
	x.r.log.WithFields(logrus.Fields{"op": "skipped", "path": it.Names[0], "reason": reason}).Log(level, "Skipped")
}

func (x *execution) failed(it Item, msg string, err error) {
	x.r.failed.Add(int64(len(it.Names)))
	x.r.bytesDone.Add(it.Size)
	x.r.log.WithFields(logrus.Fields{"op": "failed", "path": it.Names[0], "reason": err.Error()}).Error(msg)
}

// removeStaleTemps deletes the temporary files an earlier, interrupted run left behind.
func (x *execution) removeStaleTemps() {
	r := x.r
	removed := 0
	for _, rel := range x.plan.StaleTemps {
		info, err := fileutil.Lstat(r.root, rel)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode.IsRegular()) {
			continue // already gone, or replaced by something that isn't ours
		}
		if err == nil {
			x.touch(path.Dir(rel))
			err = r.root.Remove(rel)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.log.WithFields(logrus.Fields{"op": "warning", "path": rel, "reason": reasonOf(err)}).
				Warn("Couldn't remove a temporary file left by an earlier run")
			continue
		}
		removed++
		r.log.WithFields(logrus.Fields{"op": "removed-temp", "path": rel}).Info("Removed a temporary file left by an earlier run")
	}
	if removed > 0 {
		r.log.Infof("Cleaned up %d temporary %s left by an earlier run", removed, plural(removed, "file", "files"))
	}
	x.restoreIdle()
}

// touch notes that the run is about to change the given folders.
func (x *execution) touch(dirs ...string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, dir := range dirs {
		x.touched[dir] = true
	}
}

// finish puts back the times of every folder in which it was the last item.
func (x *execution) finish(it Item) {
	var idle []string
	x.mu.Lock()
	for _, dir := range itemDirs(it) {
		x.pending[dir]--
		if x.pending[dir] == 0 && x.touched[dir] {
			delete(x.touched, dir)
			idle = append(idle, dir)
		}
	}
	x.mu.Unlock()
	for _, dir := range idle {
		x.restoreDir(dir)
	}
}

// restoreIdle puts back the times of touched folders that have no items to come.
func (x *execution) restoreIdle() {
	var idle []string
	x.mu.Lock()
	for dir := range x.touched {
		if x.pending[dir] == 0 {
			delete(x.touched, dir)
			idle = append(idle, dir)
		}
	}
	x.mu.Unlock()
	for _, dir := range idle {
		x.restoreDir(dir)
	}
}

// restoreAll puts back the times of every folder still touched, such as after a stop.
func (x *execution) restoreAll() {
	x.mu.Lock()
	dirs := make([]string, 0, len(x.touched))
	for dir := range x.touched {
		dirs = append(dirs, dir)
	}
	clear(x.touched)
	x.mu.Unlock()
	for _, dir := range dirs {
		x.restoreDir(dir)
	}
}

// restoreDir gives a folder back the access and modification times Scan saw, so tools that look
// at folder times don't think every folder changed. A folder that was replaced is left alone.
func (x *execution) restoreDir(dir string) {
	want, ok := x.plan.dirs[dir]
	if !ok {
		return
	}
	now, err := fileutil.Lstat(x.r.root, dir)
	if err != nil || !now.Mode.IsDir() || now.ID != want.id {
		return
	}
	if now.Atime.Equal(want.atime) && now.Mtime.Equal(want.mtime) {
		return
	}
	if err := x.r.root.Chtimes(dir, want.atime, want.mtime); err != nil {
		x.r.log.WithFields(logrus.Fields{"op": "warning", "path": dir, "reason": reasonOf(err)}).
			Debug("Couldn't put back this folder's timestamps")
	}
}

func (x *execution) summary(ctx context.Context, start time.Time) Summary {
	r := x.r
	s := Summary{
		Total:      x.plan.TotalFiles,
		Rebalanced: int(r.rebalanced.Load()),
		Failed:     int(r.failed.Load()),
		Skipped:    make(map[SkipReason]int, len(x.plan.Skipped)+len(x.skipped)),
		Bytes:      r.bytesRewritten.Load(),
		Duration:   time.Since(start),
		Stopped:    StopReason(r.reason.Load()),
	}
	for why, n := range x.plan.Skipped {
		s.Skipped[why] += n
	}
	runSkipped := 0
	for why, n := range x.skipped {
		s.Skipped[why] += n
		runSkipped += n
	}
	s.Remaining = s.Total - s.Rebalanced - s.Failed - runSkipped
	switch {
	case s.Stopped == None && s.Remaining > 0 && ctx.Err() != nil:
		s.Stopped = Interrupted
	case s.Stopped == Interrupted && s.Remaining == 0:
		s.Stopped = None // asked to stop, but everything was already done
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
