package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// newZFSClient makes the client used to look at ZFS. Tests replace it with a fake.
var newZFSClient = zfs.NewClient

// zfsView is what a run knows about the ZFS dataset holding the folder. Every check is best
// effort: a problem is logged and the run carries on.
type zfsView struct {
	client  *zfs.Client
	log     *logrus.Logger
	dataset string // "" when the folder isn't on ZFS, or that couldn't be found out
}

// ask runs one zfs query, giving up after zfsTimeout.
func ask[T any](ctx context.Context, query func(context.Context, string) (T, error), arg string) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, zfsTimeout)
	defer cancel()
	return query(ctx, arg)
}

// lookup finds the dataset holding root, warning if it isn't on ZFS.
func (z *zfsView) lookup(ctx context.Context, root string) {
	ds, err := ask(ctx, z.client.DatasetForPath, root)
	switch {
	case ctx.Err() != nil:
	case errors.Is(err, zfs.ErrNotZFS):
		z.log.Warn("This folder isn't on ZFS, so rewriting files won't rebalance anything.")
	case err != nil:
		z.log.WithField("reason", err.Error()).Warn("Couldn't find out which ZFS dataset holds this folder, so the ZFS safety checks were skipped")
	default:
		z.dataset = ds
		z.log.Debugf("This folder is on the ZFS dataset %s", ds)
	}
}

// checkDataset warns about snapshots and deduplication, which both make rebalancing costly.
func (z *zfsView) checkDataset(ctx context.Context) {
	if z.dataset == "" {
		return
	}
	has, err := ask(ctx, z.client.HasSnapshots, z.dataset)
	switch {
	case err != nil:
		z.debug("Couldn't check for snapshots", err)
	case has:
		z.log.Warn("This dataset has snapshots. Every rewritten file will be stored twice until those snapshots are removed — keep an eye on free space.")
	}
	dedup, err := ask(ctx, z.client.Dedup, z.dataset)
	switch {
	case err != nil:
		z.debug("Couldn't check whether deduplication is on", err)
	case dedup != "off" && dedup != "-" && dedup != "":
		z.log.Warnf("Deduplication is on for this dataset (dedup=%s). Rewriting will be slow, and the new copies may just point back at the old blocks, so the data may not move.", dedup)
	}
}

// checkSpace warns when free space looks too low for the files that will be copied at once.
func (z *zfsView) checkSpace(ctx context.Context, plan *rebalance.Plan, concurrency int) {
	if z.dataset == "" || plan.LargestFile == 0 {
		return
	}
	atOnce := min(concurrency, len(plan.Items))
	need := 2 * uint64(plan.LargestFile) * uint64(atOnce)
	avail, err := ask(ctx, z.client.Available, z.dataset)
	switch {
	case err != nil:
		z.debug("Couldn't check the free space", err)
	case avail < need:
		z.log.Warnf("Free space is tight: %s is free, and working on %s at a time may need up to %s. Try a lower --concurrency, or free up some space first.",
			zfs.FormatBytes(avail), countFiles(atOnce), zfs.FormatBytes(need))
	}
}

// vdevBefore is the pool as it was before the run, for the comparison afterwards.
type vdevBefore struct {
	dist     zfs.Distribution
	bclone   uint64
	bcloneOK bool
}

// reportBefore prints how full each vdev is and returns it, or nil if that couldn't be read.
func (z *zfsView) reportBefore(ctx context.Context, stdout io.Writer) *vdevBefore {
	if z.dataset == "" {
		z.log.Warn("Skipping the vdev report, since this folder's ZFS pool couldn't be found.")
		return nil
	}
	pool := zfs.PoolOf(z.dataset)
	d, err := ask(ctx, z.client.Distribution, pool)
	if err != nil {
		if ctx.Err() == nil {
			z.log.WithField("reason", err.Error()).Warn("Couldn't read how full each vdev is, so there will be no vdev report")
		}
		return nil
	}
	b := &vdevBefore{dist: d}
	b.bclone, b.bcloneOK = z.bcloneUsed(ctx, pool)
	fmt.Fprintln(stdout, "Before:")
	if err := zfs.RenderReport(stdout, d); err != nil {
		z.debug("Couldn't print the vdev report", err)
	}
	fmt.Fprintln(stdout)
	return b
}

// reportAfter prints how each vdev changed during the run. It first gives ZFS up to freeWait to
// release the old copies' space; Ctrl+C skips the wait (gentle is checked first, so after one
// Ctrl+C during the run the wait can still be skipped with another).
func (z *zfsView) reportAfter(gentle, hard context.Context, stdout io.Writer, before *vdevBefore) {
	pool := before.dist.Pool
	var notes []string
	waited := false
	if ctx := firstLive(gentle, hard); ctx != nil {
		z.log.Info("Waiting for ZFS to finish freeing the old copies' space, so the table is accurate (up to 2 minutes; press Ctrl+C to skip)…")
		timedOut, err := z.client.WaitForFrees(ctx, pool, freeWait)
		switch {
		case timedOut:
			notes = append(notes, "ZFS was still freeing space after 2 minutes, so the old vdevs may look fuller than they really are")
			waited = true
		case err == nil:
			waited = true
		case ctx.Err() == nil:
			z.debug("Couldn't wait for ZFS to free space", err)
		}
	}
	if !waited {
		notes = append(notes, "ZFS may not have finished freeing the old copies' space, so the old vdevs may look fuller than they really are")
	}

	// Read the pool even after Ctrl+C: the table is quick, and the run's result is worth seeing.
	ctx := context.WithoutCancel(hard)
	after, err := ask(ctx, z.client.Distribution, pool)
	if err != nil {
		z.log.WithField("reason", err.Error()).Warn("Couldn't read how full each vdev is after the run, so there's no comparison")
		return
	}
	if held, err := ask(ctx, z.client.SnapshotBytes, z.dataset); err != nil {
		z.debug("Couldn't check how much space snapshots hold", err)
	} else if held > 0 {
		notes = append(notes, fmt.Sprintf("snapshots hold %s of old data, so the old vdevs won't shrink until those snapshots are removed", zfs.FormatBytes(held)))
	}
	if now, ok := z.bcloneUsed(ctx, pool); ok && before.bcloneOK && now > before.bclone {
		notes = append(notes, fmt.Sprintf("block cloning grew by %s during the run; if nothing else was copying files, some files may have been cloned instead of rewritten", zfs.FormatBytes(now-before.bclone)))
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

func (z *zfsView) bcloneUsed(ctx context.Context, pool string) (uint64, bool) {
	ctx, cancel := context.WithTimeout(ctx, zfsTimeout)
	defer cancel()
	n, ok, err := z.client.BcloneUsed(ctx, pool)
	if err != nil {
		z.debug("Couldn't check block cloning", err)
		return 0, false
	}
	return n, ok
}

func (z *zfsView) debug(msg string, err error) {
	z.log.WithField("reason", err.Error()).Debug(msg)
}

// report is --report: print how full each vdev of the folder's pool is, and change nothing.
func report(root string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	client := newZFSClient()
	ds, err := ask(ctx, client.DatasetForPath, root)
	switch {
	case errors.Is(err, zfs.ErrNotZFS):
		fmt.Fprintf(stderr, "rebalance: %q isn't on ZFS (or the zfs tools aren't installed here), so there's no pool to report on\n", root)
		return exitUsage
	case err != nil:
		fmt.Fprintf(stderr, "rebalance: couldn't find out which ZFS dataset holds %q: %v\n", root, err)
		return exitFailed
	}
	d, err := ask(ctx, client.Distribution, zfs.PoolOf(ds))
	if err != nil {
		fmt.Fprintf(stderr, "rebalance: couldn't read how full each vdev is: %v\n", err)
		return exitFailed
	}
	if err := zfs.RenderReport(stdout, d); err != nil {
		fmt.Fprintf(stderr, "rebalance: couldn't print the report: %v\n", err)
		return exitFailed
	}
	return exitOK
}
