package rebalance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path"
	"slices"
	"strings"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

// legacySuffix is what version 1 appended to the name of its temporary copies.
const legacySuffix = ".balance"

// Scan walks the folder once and works out what Execute should do. It leaves every file alone.
//
// It skips ".zfs" snapshot folders, the paths in Config.Exclude and Config.ExcludeIDs, symlinks and
// anything else that isn't a regular file. Temporary files left by an earlier run go into
// Plan.StaleTemps. Files that are already done (Config.Passes), hardlinked files that can't be
// handled and, without root, files owned by someone else are counted in Plan.Skipped. If ctx is
// cancelled the walk stops and ctx.Err() is returned.
func (r *Rebalancer) Scan(ctx context.Context) (*Plan, error) {
	counts, err := r.state.Counts()
	if err != nil {
		return nil, fmt.Errorf("couldn't read the saved progress: %w", err)
	}
	r.excludes.refreshIDs()
	s := &scanner{
		r:       r,
		ctx:     ctx,
		plan:    &Plan{Skipped: make(map[SkipReason]int), dirs: make(map[string]dirTimes)},
		groups:  make(map[fileutil.FileID]linkGroup),
		folders: make(map[string]fileutil.Info),
	}
	err = fs.WalkDir(r.root.FS(), ".", s.visit)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	s.finish(counts)
	return s.plan, nil
}

type scanner struct {
	r       *Rebalancer
	ctx     context.Context
	plan    *Plan
	items   []Item // in walk order; a hardlinked file sits where its first name was found
	groups  map[fileutil.FileID]linkGroup
	folders map[string]fileutil.Info // every folder walked so far
}

// linkGroup is a hardlinked file being collected: where it is in scanner.items, and how many
// names it should end up with.
type linkGroup struct {
	index int
	nlink uint64
}

func (s *scanner) visit(rel string, d fs.DirEntry, err error) error {
	if s.ctx.Err() != nil {
		return fs.SkipAll
	}
	if err != nil {
		if rel == "." {
			return fmt.Errorf("couldn't read the folder (%s)", reasonOf(err))
		}
		s.warn(rel, "Couldn't look inside this folder, so the files in it were left alone", err)
		s.plan.Skipped[SkipUnreadable]++
		return nil
	}

	if d.IsDir() {
		if rel != "." && d.Name() == ".zfs" {
			return fs.SkipDir // ZFS snapshots: read-only copies of the past
		}
		info, err := entryInfo(d)
		if err != nil {
			s.warn(rel, "Couldn't read this folder's details, so the files in it were left alone", err)
			s.plan.Skipped[SkipUnreadable]++
			return fs.SkipDir
		}
		if s.r.excludes.match(rel, info.ID) {
			return fs.SkipDir
		}
		s.folders[rel] = info
		s.plan.dirs[rel] = dirTimes{id: info.ID, atime: info.Atime, mtime: info.Mtime, ctime: info.Ctime}
		return nil
	}
	if !d.Type().IsRegular() {
		return nil
	}

	name := d.Name()
	if fileutil.IsTempName(name) {
		s.plan.StaleTemps = append(s.plan.StaleTemps, rel)
		return nil
	}
	info, err := entryInfo(d)
	if err != nil {
		s.warn(rel, "Couldn't read this file's details, so it was left alone", err)
		s.plan.Skipped[SkipUnreadable]++
		return nil
	}
	if s.r.excludes.match(rel, info.ID) {
		return nil
	}

	if len(name) > len(legacySuffix) && strings.HasSuffix(name, legacySuffix) {
		s.plan.LegacyBalance = append(s.plan.LegacyBalance, rel)
		// With no original next to it, this may be the only copy of a file that version 1 was
		// in the middle of rewriting, so it is left exactly as it is for a person to look at.
		if _, err := s.r.root.Lstat(strings.TrimSuffix(rel, legacySuffix)); errors.Is(err, fs.ErrNotExist) {
			s.plan.OrphanBalance = append(s.plan.OrphanBalance, rel)
			s.plan.Skipped[SkipOrphanBalance]++
			return nil
		}
	}

	// Checked here, before anything is read, so a run without root never copies a file it
	// can't give back to its owner.
	if why := s.r.as.cantRewrite(info, s.folders[path.Dir(rel)]); why != "" {
		s.plan.Skipped[SkipOwner]++
		s.r.log.WithFields(logrus.Fields{"op": "skipped", "path": rel, "reason": why}).Debug("Skipped")
		return nil
	}

	if info.Nlink > 1 {
		if !s.r.cfg.ProcessHardlinks {
			s.plan.Skipped[SkipHardlinked]++
			return nil
		}
		g, ok := s.groups[info.ID]
		if !ok {
			g = linkGroup{index: len(s.items), nlink: info.Nlink}
			s.groups[info.ID] = g
			s.items = append(s.items, Item{Size: info.Size, Allocated: info.Blocks})
		}
		s.items[g.index].Names = append(s.items[g.index].Names, rel)
		return nil
	}
	s.items = append(s.items, Item{Names: []string{rel}, Size: info.Size, Allocated: info.Blocks})
	return nil
}

// finish drops what can't or needn't be done, fills in the totals and orders the work.
func (s *scanner) finish(counts map[string]int) {
	p := s.plan
	// A hardlinked file with names outside the folder (or being changed right now) is left alone:
	// rewriting only some of its names would split it into two separate copies. Each name found
	// is logged, so the user can find the file and point the run at a folder holding all of them.
	incomplete := make(map[int]uint64)
	for _, g := range s.groups {
		if uint64(len(s.items[g.index].Names)) != g.nlink {
			incomplete[g.index] = g.nlink
		}
	}
	for i, it := range s.items {
		if nlink, ok := incomplete[i]; ok {
			p.Skipped[SkipHardlinksOutside] += len(it.Names)
			why := outsideReason(len(it.Names), nlink)
			for _, name := range it.Names {
				s.r.log.WithFields(logrus.Fields{"op": "skipped", "path": name, "reason": why}).Info("Skipped")
			}
			continue
		}
		done := 0
		for _, name := range it.Names {
			done = max(done, counts[name])
		}
		if done >= s.r.cfg.Passes {
			p.Skipped[SkipAlreadyDone] += len(it.Names)
			continue
		}
		p.Items = append(p.Items, it)
		p.TotalFiles += len(it.Names)
		p.TotalBytes += it.Size
		p.LargestFile = max(p.LargestFile, it.Size)
		p.LargestAllocated = max(p.LargestAllocated, it.Allocated)
	}
	if s.r.cfg.RandomOrder {
		rand.Shuffle(len(p.Items), func(i, j int) { p.Items[i], p.Items[j] = p.Items[j], p.Items[i] })
	}

	// Only the folders the run will change need their times put back afterwards.
	keep := make(map[string]bool)
	for _, it := range p.Items {
		for _, dir := range itemDirs(it) {
			keep[dir] = true
		}
	}
	if s.r.cfg.Cleanup {
		for _, rel := range p.StaleTemps {
			keep[path.Dir(rel)] = true
		}
	}
	for dir := range p.dirs {
		if !keep[dir] {
			delete(p.dirs, dir)
		}
	}
}

// outsideReason explains why a hardlinked file found under found of its nlink names is left alone.
func outsideReason(found int, nlink uint64) string {
	if uint64(found) > nlink {
		return "its hardlinks changed while the folder was being looked through, so it was left alone"
	}
	outside := nlink - uint64(found)
	verb := "are"
	if outside == 1 {
		verb = "is"
	}
	return fmt.Sprintf("%d of its %d hardlinked names %s outside this folder, so it was left alone", outside, nlink, verb)
}

func (s *scanner) warn(rel, msg string, err error) {
	s.r.log.WithFields(logrus.Fields{"op": "warning", "path": rel, "reason": reasonOf(err)}).Warn(msg)
}

// entryInfo returns the status of a walked entry. Entries read through an os.Root already carry
// it, so this doesn't touch the disk.
func entryInfo(d fs.DirEntry) (fileutil.Info, error) {
	fi, err := d.Info()
	if err != nil {
		return fileutil.Info{}, err
	}
	return fileutil.InfoOf(fi)
}

// itemDirs returns the folders holding the item's names, each once.
func itemDirs(it Item) []string {
	dirs := make([]string, 0, 1)
	for _, name := range it.Names {
		if dir := path.Dir(name); !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
