package rebalance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

// Test seams. replace rewrites one item, freeBytes says how much room is left in a folder and
// setFolderTimes puts back a folder's times; tests swap them to simulate failures, such as a full
// pool, that are hard to cause for real.
var (
	replace        = fileutil.ReplaceGroup
	freeBytes      = folderFreeBytes
	setFolderTimes = fileutil.SetTimes
)

// When putting back a folder's times fails because the filesystem is out of space or over quota,
// it is tried again after each of noSpaceDelays: after a run stops for lack of space, ZFS frees the
// abandoned copies' space a few seconds later. All the folders of a run share noSpaceBudget of
// waiting, so a dataset that stays full can't hold up the end of the run for long.
var (
	noSpaceDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1500 * time.Millisecond, 2 * time.Second}
	noSpaceBudget = 5 * time.Second
)

// quotaMargin is how much free space, beyond twice the file's size on disk, the filesystem must
// still have for a quota error to be blamed on the file's owner or group rather than on the
// dataset itself running out of room.
const quotaMargin = 64 << 20

// Why a file is left alone, for reasons the engine works out itself.
const (
	reasonNoPermission = "no permission to replace it (run with sudo to include it)"
	reasonOverQuota    = "its owner or group is over their quota"
)

// Execute rewrites the files in p, Config.Concurrency at a time, and records each success in the
// StateStore. A file that can't be rewritten is always left exactly as it was.
//
// With Config.Cleanup set, the stale temporary files found by Scan are removed first, except those
// another run is still using. The run ends early when Stop is called, ctx is cancelled (files being
// copied are abandoned and left as they were), the pool runs out of space, or a file goes missing
// and Config.HaltOnMissing is set; Summary.Stopped says which. Folder timestamps changed by the run
// are put back as each folder is finished, and at the end for any left over, unless another
// program changed the folder in the meantime; a filesystem that is out of space is given a few
// seconds to free some first.
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
		folders: make(map[string]*folder),
		skipped: make(map[SkipReason]int),
	}
	for _, it := range p.Items {
		for _, dir := range itemDirs(it) {
			x.folder(dir).pending++
		}
	}

	if r.cfg.Cleanup && len(p.StaleTemps) > 0 && !r.halted(ctx) {
		x.removeStaleTemps(ctx)
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

	mu               sync.Mutex
	folders          map[string]*folder
	skipped          map[SkipReason]int
	timesNotRestored int

	spaceWaited atomic.Int64 // time spent waiting for space to put back folder times, in nanoseconds
}

// folder is what the run knows about a folder it works in. Its times are put back once the run is
// done with it, but only if nothing else changed it meanwhile: each operation of the run's own is
// bracketed by a look at the folder, and anything that changed it outside those brackets (between
// Scan and the first operation, between two operations, or after the last) was someone else.
// Changes another program makes during one of the run's own operations can't be told apart.
type folder struct {
	pending int   // items with a name in it that haven't finished yet
	running int   // the run's own operations in it right now
	touched bool  // the run has worked in it and its times haven't been put back yet
	last    stamp // the folder right after the run's last operation in it ended
	foreign bool  // another program changed it while the run was using it
}

// stamp is what changes whenever a folder's entries do. Linux takes timestamps from a clock that
// only moves every few milliseconds, so an entry added or removed within the same tick as the
// run's own change leaves the times as they were; the size (on ZFS and tmpfs the number of entries,
// on macOS a multiple of it) and the link count (which counts subfolders) still give it away.
type stamp struct {
	id           fileutil.FileID
	mtime, ctime time.Time
	size         int64
	nlink        uint64
}

func stampOf(info fileutil.Info) stamp {
	return stamp{id: info.ID, mtime: info.Mtime, ctime: info.Ctime, size: info.Size, nlink: info.Nlink}
}

func (s stamp) same(o stamp) bool {
	return s.id == o.id && s.mtime.Equal(o.mtime) && s.ctime.Equal(o.ctime) && s.size == o.size && s.nlink == o.nlink
}

// folder returns the state of dir, creating it if needed. x.mu must be held, or Execute must not
// have started its workers yet.
func (x *execution) folder(dir string) *folder {
	f, ok := x.folders[dir]
	if !ok {
		f = &folder{}
		x.folders[dir] = f
	}
	return f
}

// process rewrites one item and records the outcome.
func (x *execution) process(ctx context.Context, it Item) {
	defer x.finish(it)
	// Scan checked the owner, but it may have changed since.
	if why, text := x.ownerChanged(it); why != "" {
		x.skip(it, why, text, logrus.InfoLevel)
		return
	}
	dirs := itemDirs(it)
	x.begin(dirs...)
	res, err := replace(ctx, x.r.root, it.Names, x.r.opts)
	x.end(dirs...)
	x.outcome(it, res, err)
}

// ownerChanged returns why the item can no longer be rewritten without root, or "" if it can (or
// if it can't be checked, which replace then reports).
func (x *execution) ownerChanged(it Item) (SkipReason, string) {
	r := x.r
	if r.as.root {
		return "", ""
	}
	file, err := fileutil.Lstat(r.root, it.Names[0])
	if err != nil {
		return "", ""
	}
	dir, err := fileutil.Lstat(r.root, path.Dir(it.Names[0]))
	if err != nil {
		return "", ""
	}
	return r.as.cantRewrite(file, dir)
}

// outcome records how the rewrite of it went.
func (x *execution) outcome(it Item, res fileutil.Result, err error) {
	r := x.r
	// fileutil's own kinds of error may also wrap a permission error, so they come first.
	switch {
	case err == nil:
		x.succeeded(it, res)
	case fileutil.LeftSplit(err):
		// Some names already use the new copy: nothing was lost, but the group is now split, so
		// this needs a person's attention whatever stopped it.
		x.failed(it, "Couldn't finish switching this file's hardlinked names", err)
		if errors.Is(err, fileutil.ErrNoSpace) {
			r.stopWith(NoSpace)
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Abandoned part-way: the file is as it was, and a resumed run will pick it up.
		r.log.WithField("path", it.Names[0]).Debug("Stopped before this file was finished; it was left as it was")
	case errors.Is(err, fileutil.ErrNoSpace):
		if errors.Is(err, syscall.EDQUOT) && x.roomLeft(it) {
			// The filesystem has room, so the quota is the owner's or group's own.
			x.skip(it, SkipQuota, reasonOverQuota, logrus.WarnLevel)
			return
		}
		x.failed(it, "Ran out of free space, so the run is stopping", err)
		r.stopWith(NoSpace)
	case errors.Is(err, fileutil.ErrBusy):
		x.skip(it, SkipBusy, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrUndeletable):
		x.skip(it, SkipUndeletable, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrImmutable):
		x.skip(it, SkipImmutable, err.Error(), logrus.InfoLevel)
	case errors.Is(err, fileutil.ErrOwnership):
		x.skip(it, SkipOwnerNotKept, err.Error(), logrus.WarnLevel)
	case errors.Is(err, fileutil.ErrProjectID):
		// Linux and ZFS won't move a copy into a folder that gives new files another project ID.
		x.skip(it, SkipProjectID, err.Error(), logrus.InfoLevel)
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
	case errors.Is(err, fs.ErrPermission) && !r.as.root:
		// Opening the file, or adding a copy to its folder, was refused. Only root can fix that.
		x.skip(it, SkipNoPermission, reasonNoPermission, logrus.InfoLevel)
	default:
		x.failed(it, "Couldn't rebalance", err)
	}
}

// roomLeft reports whether the filesystem holding it still has clearly more free space than the
// item needs, so that a quota error must come from a per-user or per-group quota. The margin
// covers space ZFS hasn't yet freed from the abandoned copy.
func (x *execution) roomLeft(it Item) bool {
	dir := path.Dir(it.Names[0])
	free, err := freeBytes(x.r.root, dir)
	if err != nil {
		x.r.log.WithFields(logrus.Fields{"path": dir, "reason": reasonOf(err)}).Debug("Couldn't check how much free space is left")
		return false
	}
	return free > 2*uint64(max(it.Allocated, 0))+quotaMargin
}

// fields are the log fields that name an item: "path", and for a hardlinked file "names" with
// every one of its names.
func fields(op string, it Item) logrus.Fields {
	f := logrus.Fields{"op": op, "path": it.Names[0]}
	if len(it.Names) > 1 {
		f["names"] = slices.Clone(it.Names)
	}
	return f
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
	f := fields("rebalanced", it)
	f["size"], f["mbps"] = res.Size, mbps
	r.log.WithFields(f).Log(level, msg)
}

func (x *execution) skip(it Item, why SkipReason, reason string, level logrus.Level) {
	x.mu.Lock()
	x.skipped[why] += len(it.Names)
	x.mu.Unlock()
	x.r.skipped.Add(int64(len(it.Names)))
	x.r.bytesDone.Add(it.Size)
	f := fields("skipped", it)
	f["reason"] = reason
	x.r.log.WithFields(f).Log(level, "Skipped")
}

func (x *execution) failed(it Item, msg string, err error) {
	x.r.failed.Add(int64(len(it.Names)))
	x.r.bytesDone.Add(it.Size)
	f := fields("failed", it)
	f["reason"] = err.Error()
	x.r.log.WithFields(f).Error(msg)
}

// removeStaleTemps deletes the temporary files an earlier, interrupted run left behind. Those
// another run is still using (it holds their lock) are left alone.
func (x *execution) removeStaleTemps(ctx context.Context) {
	r := x.r
	removed := 0
	for _, rel := range x.plan.StaleTemps {
		if r.halted(ctx) {
			break
		}
		dir := path.Dir(rel)
		x.begin(dir)
		ok, err := fileutil.RemoveStaleTemp(r.root, rel)
		x.end(dir)
		switch {
		case ok:
			removed++
			r.log.WithFields(logrus.Fields{"op": "removed-temp", "path": rel}).Info("Removed a temporary file left by an earlier run")
		case err == nil:
			r.log.WithField("path", rel).Info("This temporary file is being used by another run, so it was left alone")
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fileutil.ErrNotRegular):
			// Already gone, or replaced by something that isn't ours.
		default:
			r.log.WithFields(logrus.Fields{"op": "warning", "path": rel, "reason": reasonOf(err)}).
				Warn("Couldn't remove a temporary file left by an earlier run")
		}
	}
	if removed > 0 {
		r.log.Infof("Cleaned up %d temporary %s left by an earlier run", removed, plural(removed, "file", "files"))
	}
	x.restoreIdle()
}

// begin notes that one of the run's own operations is about to change the given folders. If none
// was going on in a folder, the folder must look exactly as the run last left it (or as Scan saw
// it); otherwise another program changed it, and its times are left alone at the end.
func (x *execution) begin(dirs ...string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, dir := range dirs {
		f := x.folder(dir)
		if f.running == 0 && !f.foreign {
			want, known := f.last, f.touched
			if !known {
				var t dirTimes
				t, known = x.plan.dirs[dir]
				want = t.stamp
			}
			if known {
				if now, err := x.stamp(dir); err != nil || !now.same(want) {
					f.foreign = true
				}
			}
		}
		f.running++
		f.touched = true
	}
}

// end notes that an operation begun with begin has finished, remembering how it left each folder.
func (x *execution) end(dirs ...string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, dir := range dirs {
		f := x.folder(dir)
		f.running--
		now, err := x.stamp(dir)
		if err != nil {
			f.foreign = true // can't tell what happened to it, so it is left as it is
			continue
		}
		f.last = now
	}
}

func (x *execution) stamp(dir string) (stamp, error) {
	info, err := fileutil.Lstat(x.r.root, dir)
	if err != nil {
		return stamp{}, err
	}
	return stampOf(info), nil
}

// finish puts back the times of every folder in which it was the last item.
func (x *execution) finish(it Item) {
	var idle []string
	x.mu.Lock()
	for _, dir := range itemDirs(it) {
		f := x.folder(dir)
		f.pending--
		if f.pending == 0 && f.touched {
			idle = append(idle, dir)
		}
	}
	x.mu.Unlock()
	x.restore(idle)
}

// restoreIdle puts back the times of touched folders that have no items to come.
func (x *execution) restoreIdle() {
	var idle []string
	x.mu.Lock()
	for dir, f := range x.folders {
		if f.pending == 0 && f.touched {
			idle = append(idle, dir)
		}
	}
	x.mu.Unlock()
	x.restore(idle)
}

// restoreAll puts back the times of every folder still touched, such as after a stop. No operation
// may be running.
func (x *execution) restoreAll() {
	var dirs []string
	x.mu.Lock()
	for dir, f := range x.folders {
		if f.touched {
			dirs = append(dirs, dir)
		}
	}
	x.mu.Unlock()
	x.restore(dirs)
}

// restore puts back the times of folders in which the run has nothing more to do.
func (x *execution) restore(dirs []string) {
	for _, dir := range dirs {
		x.mu.Lock()
		f := *x.folder(dir)
		x.folder(dir).touched = false
		x.mu.Unlock()
		x.restoreDir(dir, f)
	}
}

// restoreDir gives a folder back the access and modified times Scan saw, so tools that look at
// folder times don't think every folder changed. A folder that was replaced, or that another
// program changed while the run was using it, keeps its new times. The folder is checked and its
// times set through one open descriptor, so they can't land on anything put in its place.
func (x *execution) restoreDir(dir string, f folder) {
	log := x.r.log
	want, ok := x.plan.dirs[dir]
	if !ok {
		return
	}
	d, err := openDir(x.r.root, dir)
	if err != nil {
		return // gone, or replaced by something that isn't a folder
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		return
	}
	now, err := fileutil.InfoOf(fi)
	if err != nil || !now.Mode.IsDir() || now.ID != want.stamp.id {
		return
	}
	if f.foreign || !f.last.same(stampOf(now)) {
		log.WithField("path", dir).Debug("Left this folder's times as they are, because another program changed the folder during the run")
		return
	}
	if now.Atime.Equal(want.atime) && now.Mtime.Equal(want.mtime) {
		return
	}
	err = x.setDirTimes(d, want.atime, want.mtime)
	switch {
	case err == nil:
	case now.Mtime.Equal(want.mtime):
		// Only the access time was off, and nothing relies on that.
		log.WithFields(logrus.Fields{"path": dir, "reason": reasonOf(err)}).Debug("Couldn't put back this folder's access time")
	default:
		x.mu.Lock()
		x.timesNotRestored++
		x.mu.Unlock()
		log.WithFields(logrus.Fields{"op": "warning", "path": dir, "reason": reasonOf(err)}).
			Warn("Couldn't put back this folder's modified time, so it shows when the run changed it")
	}
}

// setDirTimes sets the times of the open folder d, trying again for a while if the filesystem is
// out of space or over quota (see noSpaceDelays).
func (x *execution) setDirTimes(d *os.File, atime, mtime time.Time) error {
	for _, delay := range noSpaceDelays {
		err := setFolderTimes(d, atime, mtime)
		if !errors.Is(err, syscall.EDQUOT) && !errors.Is(err, syscall.ENOSPC) {
			return err
		}
		if x.spaceWaited.Add(int64(delay)) > int64(noSpaceBudget) {
			return err
		}
		time.Sleep(delay)
	}
	return setFolderTimes(d, atime, mtime)
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
	x.mu.Lock()
	s.RunSkipped = maps.Clone(x.skipped)
	for why, n := range x.skipped {
		s.Skipped[why] += n
		runSkipped += n
	}
	s.FolderTimesNotRestored = x.timesNotRestored
	x.mu.Unlock()
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
