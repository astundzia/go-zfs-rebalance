package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/rebalance"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

const (
	// zfsTimeout bounds each zfs or zpool command run for the safety checks and vdev reports.
	zfsTimeout = 15 * time.Second
	// freeWait is how long to wait for ZFS to release the old copies' space before the
	// after-table, so the old vdevs show the space they got back.
	freeWait = 2 * time.Minute
	// maxListed is how many leftover .balance files are named in the warning about them.
	maxListed = 5
)

// Test seams: the client used to look at ZFS, how the kernel is asked whether a folder is on ZFS,
// and how often the pool's block-cloning counter is read during a --vdev-report run.
var (
	newZFSClient = zfs.NewClient
	zfsOnPath    = zfs.OnZFS
	cloneWatch   = 10 * time.Second
)

// trueNASHint explains why starting zfs or zpool fails with ENOSYS, and what to do instead.
const trueNASHint = "TrueNAS blocks starting other programs once the sudo session that started this run has ended. " +
	"Next time, start tmux first without sudo, then run sudo rebalance inside it."

// zfsView is what a run knows about the ZFS dataset holding the folder. Every check is best
// effort: a problem is logged and the run carries on.
type zfsView struct {
	client  *zfs.Client
	log     *logrus.Logger
	stop    *stopper           // told when the run starts and stops waiting for ZFS; may be nil
	dataset string             // "" when the folder isn't on ZFS, or that couldn't be found out
	noPool  string             // why dataset is "", for the vdev report's warning
	deduped []zfs.DatasetDedup // datasets in the folder with deduplication on
	// blocked is set once starting the zfs tools has been refused and the user told why. The
	// block-cloning watch may set it while the files are being rewritten.
	blocked atomic.Bool
}

// ask runs one zfs query, giving up after zfsTimeout.
func ask[T any](ctx context.Context, query func(context.Context, string) (T, error), arg string) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsTimeout)
	defer cancel()
	return query(ctx, arg)
}

// noDataset is why the ZFS dataset holding a folder couldn't be found.
type noDataset int

const (
	notOnZFS     noDataset = iota // the folder isn't on ZFS
	toolsMissing                  // the zfs and zpool commands weren't found
	execBlocked                   // the system wouldn't start them (TrueNAS once sudo has ended)
	cantReachZFS                  // the folder is on ZFS, but the tools can't reach ZFS from here
	lookupFailed                  // anything else
)

// whyNoDataset works out why DatasetForPath failed for root with err. The kernel is asked
// whether root is on ZFS, so the answer doesn't depend on the zfs tools working.
func whyNoDataset(root string, err error) (why noDataset, onZFS bool) {
	if errors.Is(err, zfs.ErrExecBlocked) {
		return execBlocked, false
	}
	on, statErr := zfsOnPath(root)
	known := statErr == nil
	switch {
	case known && !on:
		return notOnZFS, false
	case errors.Is(err, zfs.ErrToolsMissing):
		return toolsMissing, on
	case errors.Is(err, zfs.ErrNotZFS) && known:
		return cantReachZFS, true
	case errors.Is(err, zfs.ErrNotZFS):
		return notOnZFS, false
	}
	return lookupFailed, on
}

// lookup finds the dataset holding root, warning if that isn't possible.
func (z *zfsView) lookup(ctx context.Context, root string) {
	ds, err := ask(ctx, z.client.DatasetForPath, root)
	switch {
	case ctx.Err() != nil:
		return
	case err == nil:
		z.dataset = ds
		z.log.Debugf("This folder is on the ZFS dataset %s", ds)
		return
	}
	const skipped = "so the ZFS safety checks (snapshots, deduplication and free space) were skipped"
	switch why, onZFS := whyNoDataset(root, err); why {
	case notOnZFS:
		z.noPool = "this folder isn't on ZFS"
		z.log.Warn("This folder isn't on ZFS, so rewriting files won't rebalance anything.")
	case toolsMissing:
		z.noPool = "the zfs and zpool commands weren't found"
		if onZFS {
			z.log.Warn("This folder is on ZFS, but the zfs and zpool commands weren't found, " + skipped + ".")
		} else {
			z.log.Warn("The zfs and zpool commands weren't found, " + skipped + ".")
		}
	case execBlocked:
		z.noPool = "the zfs tools couldn't be started"
		z.execBlocked(skipped)
	case cantReachZFS:
		z.noPool = "the zfs tools can't reach ZFS from here"
		z.log.WithField("reason", zfsReason(err)).Warn("This folder is on ZFS, but the zfs tools can't reach ZFS from here (as inside some containers), " + skipped)
	default:
		z.noPool = "this folder's ZFS dataset couldn't be found"
		z.log.WithField("reason", zfsReason(err)).Warn("Couldn't find out which ZFS dataset holds this folder, " + skipped)
	}
}

// execBlocked explains, once per run, that the system refused to start the zfs tools, and what
// that means for the run (what, such as "so there's no vdev table").
func (z *zfsView) execBlocked(what string) {
	if !z.blocked.CompareAndSwap(false, true) {
		z.log.Debugf("Couldn't start the zfs tools again, %s", what)
		return
	}
	z.log.Warnf("Couldn't start the zfs tools, %s. %s", what, trueNASHint)
}

// problem logs that a check couldn't be done because of err, explaining a refused start.
func (z *zfsView) problem(msg string, err error) {
	if errors.Is(err, zfs.ErrExecBlocked) {
		z.execBlocked("so some ZFS checks were skipped")
		return
	}
	z.log.WithField("reason", zfsReason(err)).Debug(msg)
}

// checkDataset warns about snapshots and deduplication, which both make rebalancing costly. root
// is the folder being rebalanced, used to tell which datasets below this one are inside it.
func (z *zfsView) checkDataset(ctx context.Context, root string) {
	if z.dataset == "" {
		return
	}
	has, err := ask(ctx, z.client.HasSnapshots, z.dataset)
	switch {
	case err != nil:
		z.problem("Couldn't check for snapshots", err)
	case has:
		z.log.Warn("This dataset has snapshots. Every rewritten file will be stored twice until those snapshots are removed — keep an eye on free space.")
	}
	all, err := ask(ctx, z.client.Dedup, z.dataset)
	if err != nil {
		z.problem("Couldn't check whether deduplication is on", err)
		return
	}
	z.deduped = dedupedInside(all, z.dataset, root)
	if msg := dedupWarning(z.deduped, z.dataset); msg != "" {
		z.log.Warn(msg)
	}
}

// dedupedInside returns the datasets in all with deduplication on whose files are in root: the
// dataset holding root, and those mounted inside it. A dataset with a legacy mountpoint can't be
// placed, so it counts if it is mounted.
func dedupedInside(all []zfs.DatasetDedup, dataset, root string) []zfs.DatasetDedup {
	var in []zfs.DatasetDedup
	for _, d := range all {
		switch {
		case d.Dedup == "off" || d.Dedup == "-" || d.Dedup == "":
		case d.Name == dataset:
			in = append(in, d)
		case !d.Mounted:
		case d.Mountpoint == "legacy" || isInside(d.Mountpoint, root):
			in = append(in, d)
		}
	}
	return in
}

// isInside reports whether the absolute path p is dir or inside it.
func isInside(p, dir string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// dedupWarning is the warning about deduplication for the datasets in deduped, or "".
func dedupWarning(deduped []zfs.DatasetDedup, dataset string) string {
	const cost = "Rewriting will be slow, and the new copies may just point back at the old blocks, so the data may not move."
	switch {
	case len(deduped) == 0:
		return ""
	case len(deduped) == 1 && deduped[0].Name == dataset:
		return fmt.Sprintf("Deduplication is on for this dataset (dedup=%s). %s", deduped[0].Dedup, cost)
	case len(deduped) == 1:
		return fmt.Sprintf("Deduplication is on for %s (dedup=%s), which is inside this folder. %s", deduped[0].Name, deduped[0].Dedup, cost)
	}
	names := make([]string, len(deduped))
	for i, d := range deduped {
		names[i] = fmt.Sprintf("%s (dedup=%s)", d.Name, d.Dedup)
	}
	return fmt.Sprintf("Deduplication is on for %d datasets in this folder: %s. %s", len(deduped), strings.Join(names, ", "), cost)
}

// checkSpace warns when free space looks too low for the files that will be copied at once. Each
// copy needs about as much free space as the file takes up on disk now, which for sparse or
// compressed files is less than their size.
func (z *zfsView) checkSpace(ctx context.Context, plan *rebalance.Plan, concurrency int) {
	if z.dataset == "" || plan.LargestAllocated <= 0 {
		return
	}
	atOnce := min(concurrency, len(plan.Items))
	need := 2 * uint64(plan.LargestAllocated) * uint64(atOnce)
	avail, err := ask(ctx, z.client.Available, z.dataset)
	switch {
	case err != nil:
		z.problem("Couldn't check the free space", err)
	case avail < need:
		z.log.Warnf("Free space is tight: %s is free, and working on %s at a time may need up to %s. Try a lower --concurrency, or free up some space first.",
			zfs.FormatBytes(avail), countFiles(atOnce), zfs.FormatBytes(need))
	}
}

// vdevBefore is the pool as it was before the run, for the comparison afterwards.
type vdevBefore struct {
	dist     zfs.Distribution
	bclone   uint64 // the block-cloning counter at the start
	bcloneOK bool   // the pool has a block-cloning counter
	// bclonePeak is the highest the counter was seen during the run. Only watchCloning's
	// goroutine changes it, and only until the function it returns has returned.
	bclonePeak uint64
}

// reportBefore prints how full each vdev is and returns it, or nil if that couldn't be read.
func (z *zfsView) reportBefore(ctx context.Context, stdout io.Writer) *vdevBefore {
	if z.dataset == "" {
		z.log.Warnf("Skipping the vdev report, because %s.", z.noPool)
		return nil
	}
	pool := zfs.PoolOf(z.dataset)
	d, err := ask(ctx, z.client.Distribution, pool)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil
	case errors.Is(err, zfs.ErrExecBlocked):
		z.execBlocked("so there will be no vdev report")
		return nil
	default:
		z.log.WithField("reason", zfsReason(err)).Warn("Couldn't read how full each vdev is, so there will be no vdev report")
		return nil
	}
	b := &vdevBefore{dist: d}
	b.bclone, b.bcloneOK = z.bcloneUsed(ctx, pool, "so block cloning won't be checked")
	b.bclonePeak = b.bclone
	fmt.Fprintln(stdout, "Before:")
	if err := zfs.RenderReport(stdout, d); err != nil {
		z.debug("Couldn't print the vdev report", err)
	}
	fmt.Fprintln(stdout)
	return b
}

// watchCloning reads the pool's block-cloning counter every cloneWatch until the returned function
// is called, keeping the highest value in b. A file that was cloned instead of rewritten raises the
// counter only until it replaces the original, which frees the original's share again, so the
// counters at the start and end can be the same even when every file was cloned. If the zfs tools
// can't be started any more, the user is told (once) and the watch ends.
func (z *zfsView) watchCloning(b *vdevBefore) (stop func()) {
	if b == nil || !b.bcloneOK {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(cloneWatch)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			n, ok := z.bcloneUsed(ctx, b.dist.Pool, "so block cloning is no longer being checked")
			switch {
			case ok:
				b.bclonePeak = max(b.bclonePeak, n)
			case z.blocked.Load():
				return
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// reportAfter prints how each vdev changed during the run. It first gives ZFS up to freeWait to
// release the old copies' space; Ctrl+C skips the wait (gentle is checked first, so after one
// Ctrl+C during the run the wait can still be skipped with another).
func (z *zfsView) reportAfter(gentle, hard context.Context, stdout io.Writer, before *vdevBefore) {
	const noTable = "so there's no before-and-after table"
	if z.blocked.Load() {
		// The user was told during the run why the zfs tools can't be started.
		z.log.Warn("Skipping the before-and-after table, because the zfs tools couldn't be started.")
		return
	}
	pool := before.dist.Pool
	var notes []string
	waited := false
	if ctx := firstLive(gentle, hard); ctx != nil {
		// Ask ZFS something quick first, so the run only says it is waiting when it can.
		_, err := ask(ctx, z.client.Freeing, pool)
		if err == nil {
			z.stop.setPhase(phaseWaiting)
			z.log.Info("Waiting for ZFS to finish freeing the old copies' space, so the table is accurate (up to 2 minutes; press Ctrl+C to skip)…")
			var timedOut bool
			timedOut, err = z.client.WaitForFrees(ctx, pool, freeWait)
			z.stop.setPhase(phaseFinishing)
			switch {
			case timedOut:
				notes = append(notes, "ZFS was still freeing space after 2 minutes, so the old vdevs may look fuller than they really are")
				waited = true
			case err == nil:
				waited = true
			}
		}
		switch {
		case errors.Is(err, zfs.ErrExecBlocked):
			z.execBlocked(noTable)
			return
		case err != nil && ctx.Err() == nil:
			z.debug("Couldn't wait for ZFS to free space", err)
		}
	}
	if !waited {
		notes = append(notes, "ZFS may not have finished freeing the old copies' space, so the old vdevs may look fuller than they really are")
	}

	// Read the pool even after Ctrl+C: the table is quick, and the run's result is worth seeing.
	ctx := context.WithoutCancel(hard)
	after, err := ask(ctx, z.client.Distribution, pool)
	switch {
	case err == nil:
	case errors.Is(err, zfs.ErrExecBlocked):
		z.execBlocked(noTable)
		return
	default:
		z.log.WithField("reason", zfsReason(err)).Warn("Couldn't read how full each vdev is after the run, " + noTable)
		return
	}
	if held, err := ask(ctx, z.client.SnapshotBytes, z.dataset); err != nil {
		z.problem("Couldn't check how much space snapshots hold", err)
	} else if held > 0 {
		notes = append(notes, fmt.Sprintf("snapshots hold %s of old data, so the old vdevs won't shrink until those snapshots are removed", zfs.FormatBytes(held)))
	}
	if now, ok := z.bcloneUsed(ctx, pool, "so block cloning wasn't checked at the end"); ok && before.bcloneOK {
		if peak := max(before.bclonePeak, now); peak > before.bclone {
			notes = append(notes, fmt.Sprintf("block cloning grew by %s during the run; if nothing else was copying files, some files may have been cloned instead of rewritten", zfs.FormatBytes(peak-before.bclone)))
		}
	}
	switch n := len(z.deduped); {
	case n == 1:
		notes = append(notes, fmt.Sprintf("deduplication is on for %s, so the files rewritten there may not have moved", z.deduped[0].Name))
	case n > 1:
		notes = append(notes, fmt.Sprintf("deduplication is on for %d datasets in this folder, so the files rewritten there may not have moved", n))
	}
	fmt.Fprintln(stdout, "Before and after:")
	if err := zfs.RenderComparison(stdout, before.dist, after, notes); err != nil {
		z.debug("Couldn't print the vdev comparison", err)
	}
}

// firstLive returns the first of ctxs that isn't cancelled yet, or nil.
func firstLive(ctxs ...context.Context) context.Context {
	for _, ctx := range ctxs {
		if ctx.Err() == nil {
			return ctx
		}
	}
	return nil
}

// bcloneUsed reads the pool's block-cloning counter. ok is false when the pool has none or it
// couldn't be read. A refused start of the zfs tools is explained like any other check's, saying
// what follows from it (ifBlocked, such as "so block cloning won't be checked").
func (z *zfsView) bcloneUsed(ctx context.Context, pool, ifBlocked string) (n uint64, ok bool) {
	qctx, cancel := context.WithTimeout(ctx, zfsTimeout)
	defer cancel()
	n, ok, err := z.client.BcloneUsed(qctx, pool)
	switch {
	case err == nil:
		return n, ok
	case ctx.Err() != nil:
		// No longer wanted, as when the run ends while the watch is asking.
	case errors.Is(err, zfs.ErrExecBlocked):
		z.execBlocked(ifBlocked)
	default:
		z.debug("Couldn't check block cloning", err)
	}
	return 0, false
}

func (z *zfsView) debug(msg string, err error) {
	z.log.WithField("reason", zfsReason(err)).Debug(msg)
}

// zfsReason describes in plain words why a zfs or zpool command failed.
func zfsReason(err error) string {
	var cmdErr *zfs.CommandError
	switch {
	case errors.Is(err, zfs.ErrToolsMissing):
		return "the zfs and zpool commands weren't found"
	case errors.Is(err, zfs.ErrExecBlocked):
		return "the system wouldn't start the zfs tools"
	case errors.Is(err, context.DeadlineExceeded):
		return "the zfs tools took too long to answer"
	case errors.As(err, &cmdErr) && cmdErr.Stderr != "":
		name, _, _ := strings.Cut(cmdErr.Command, " ")
		return name + " said: " + cmdErr.Stderr
	}
	return err.Error()
}

// report is --report: print how full each vdev of the folder's pool is, and change nothing.
func report(root string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	client := newZFSClient()
	ds, err := ask(ctx, client.DatasetForPath, root)
	if err != nil {
		switch why, _ := whyNoDataset(root, err); why {
		case notOnZFS:
			fmt.Fprintf(stderr, "rebalance: %q isn't on ZFS, so there's no pool to report on\n", root)
		case toolsMissing:
			fmt.Fprintf(stderr, "rebalance: the zfs and zpool commands weren't found, so there's no pool to report on. Run this where the ZFS tools are installed\n")
		case execBlocked:
			fmt.Fprintf(stderr, "rebalance: couldn't start the zfs tools, so there's no report. TrueNAS blocks starting other programs once the sudo session that started this has ended; run sudo rebalance --report straight from your shell\n")
		case cantReachZFS:
			fmt.Fprintf(stderr, "rebalance: %q is on ZFS, but the zfs tools can't reach ZFS from here (%s), so there's no report\n", root, zfsReason(err))
		default:
			fmt.Fprintf(stderr, "rebalance: couldn't find out which ZFS dataset holds %q (%s)\n", root, zfsReason(err))
			return exitFailed
		}
		return exitUsage
	}
	d, err := ask(ctx, client.Distribution, zfs.PoolOf(ds))
	if err != nil {
		fmt.Fprintf(stderr, "rebalance: couldn't read how full each vdev is (%s)\n", zfsReason(err))
		return exitFailed
	}
	if err := zfs.RenderReport(stdout, d); err != nil {
		fmt.Fprintf(stderr, "rebalance: couldn't print the report (%v)\n", err)
		return exitFailed
	}
	return exitOK
}
