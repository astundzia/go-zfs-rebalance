// Package rebalance walks a folder and rewrites every file in place, so that ZFS spreads its data
// across all the drives in the pool. Each rewrite is done by fileutil, which never removes or
// changes the original until a verified, identical copy is ready to take its place.
//
// A run has two steps: Scan finds the work and Execute does it. Progress is kept in a StateStore,
// so a file that has already been rewritten Config.Passes times is skipped by later runs.
package rebalance

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

// StateStore remembers how many times each file has been rewritten. Paths are slash-separated and
// relative to the root. *database.DB implements it. Increment must be safe for concurrent use.
type StateStore interface {
	Counts() (map[string]int, error)
	Increment(rel string) (int, error)
}

// Config describes a rebalance run.
type Config struct {
	Root             string                // absolute path of the folder to rebalance, with symlinks resolved
	Passes           int                   // files already rewritten this many times are skipped (1 or more)
	Concurrency      int                   // files rewritten at the same time (1 or more)
	ProcessHardlinks bool                  // rewrite hardlinked files, keeping their names linked together
	Cleanup          bool                  // remove temporary files left behind by an earlier, interrupted run
	RandomOrder      bool                  // shuffle the files instead of going through them in folder order
	Checksum         fileutil.ChecksumType // hash used to check each copy; "" means sha256
	HaltOnMissing    bool                  // stop the run when a file disappears before it is rewritten
	SizeThresholdMB  int                   // successes for files smaller than this are logged at Debug level
	Exclude          []string              // absolute paths to leave alone, such as the state database files
	Logger           *logrus.Logger        // nil discards all log output
	State            StateStore            // nil keeps progress in memory for the life of the Rebalancer
}

// SkipReason says why a file was left as it was. The values are short phrases meant for a summary
// line, such as "12 already done".
type SkipReason string

// Reasons a file can be skipped.
const (
	SkipAlreadyDone      SkipReason = "already done"
	SkipHardlinked       SkipReason = "hardlinked"
	SkipHardlinksOutside SkipReason = "hardlinks outside folder"
	SkipOrphanBalance    SkipReason = "leftover .balance file"
	SkipUnreadable       SkipReason = "couldn't be read"
	SkipMissing          SkipReason = "missing"
	SkipOwner            SkipReason = "owner can't be kept"
	SkipChanged          SkipReason = "changed while copying"
	SkipLinksChanged     SkipReason = "hardlinks changed"
	SkipNotRegular       SkipReason = "not a regular file"
	SkipMetadata         SkipReason = "permissions can't be kept exactly"
)

// StopReason says why a run ended before every file was handled.
type StopReason int

// Reasons a run can stop early.
const (
	None        StopReason = iota // the run went through every file
	Interrupted                   // Stop was called or the context was cancelled
	MissingFile                   // a file disappeared and Config.HaltOnMissing is set
	NoSpace                       // the pool or a quota ran out of space
)

// String describes the reason in plain words.
func (s StopReason) String() string {
	switch s {
	case None:
		return "finished"
	case Interrupted:
		return "stopped when asked"
	case MissingFile:
		return "stopped because a file went missing"
	case NoSpace:
		return "stopped because there wasn't enough free space"
	}
	return fmt.Sprintf("StopReason(%d)", int(s))
}

// Item is one piece of work: a file, or every name of a hardlinked file.
type Item struct {
	Names []string // slash-separated paths relative to the root; Names[0] is the primary name
	Size  int64
}

// Plan is the work found by Scan. Pass it to Execute.
type Plan struct {
	Items         []Item             // files to rewrite, in the order they will be handed out
	TotalFiles    int                // names in Items (a hardlinked file counts once per name)
	TotalBytes    int64              // bytes to rewrite (a hardlinked file counts once)
	LargestFile   int64              // size of the largest item
	Skipped       map[SkipReason]int // files left alone, by reason (one per name)
	StaleTemps    []string           // temporary files left behind by an earlier run
	LegacyBalance []string           // files ending in ".balance", possibly left behind by version 1
	OrphanBalance []string           // the LegacyBalance files with no matching original; never rewritten

	dirs map[string]dirTimes // times of the folders the run will touch, to put back afterwards
}

// dirTimes is a folder's identity and timestamps as seen by Scan.
type dirTimes struct {
	id           fileutil.FileID
	atime, mtime time.Time
}

// Summary is the outcome of Execute.
type Summary struct {
	Total      int                // files the run set out to rewrite (Plan.TotalFiles)
	Rebalanced int                // files rewritten
	Failed     int                // files that couldn't be rewritten; each was left as it was
	Skipped    map[SkipReason]int // files left alone, by reason, including those skipped by Scan
	Remaining  int                // files not reached because the run stopped early
	Bytes      int64              // bytes rewritten
	Duration   time.Duration
	Stopped    StopReason
}

// Progress is a snapshot of a running Execute. Counts are per file name, like Summary.
type Progress struct {
	Started        time.Time // zero until Execute starts
	Total          int       // files the run set out to rewrite
	Done           int       // files finished: rebalanced, failed or skipped
	Rebalanced     int
	Failed         int
	Skipped        int   // files skipped during the run (not counting those skipped by Scan)
	BytesTotal     int64 // bytes the run set out to rewrite
	BytesDone      int64 // bytes of the finished files, for a completion percentage
	BytesRewritten int64 // bytes actually rewritten, for a rate
}

// Rebalancer runs Scan and Execute for one folder. Stop and Progress may be called from any
// goroutine; Scan and Execute must not run at the same time. Call Close when finished.
type Rebalancer struct {
	cfg      Config
	log      *logrus.Logger
	state    StateStore
	root     *os.Root
	opts     fileutil.Options
	excludes exclusions

	stopOnce sync.Once
	stopCh   chan struct{}
	reason   atomic.Int32 // StopReason; the first reason given wins

	started                               atomic.Pointer[time.Time]
	total, rebalanced, failed, skipped    atomic.Int64
	bytesTotal, bytesDone, bytesRewritten atomic.Int64
}

// New checks cfg and opens the folder. The returned Rebalancer holds the folder open until Close.
func New(cfg Config) (*Rebalancer, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, fileutil.ErrUnsupportedPlatform
	}
	switch {
	case cfg.Root == "":
		return nil, errors.New("no folder was given to rebalance")
	case !filepath.IsAbs(cfg.Root):
		return nil, fmt.Errorf("the folder %q must be given as a full path, starting with /", cfg.Root)
	case cfg.Passes < 1:
		return nil, fmt.Errorf("the number of passes must be 1 or more (it was %d)", cfg.Passes)
	case cfg.Concurrency < 1:
		return nil, fmt.Errorf("the number of files to work on at once must be 1 or more (it was %d)", cfg.Concurrency)
	case cfg.SizeThresholdMB < 0:
		return nil, fmt.Errorf("the size threshold can't be negative (it was %d)", cfg.SizeThresholdMB)
	}
	cfg.Root = filepath.Clean(cfg.Root)
	if cfg.Checksum != "" {
		sum, err := fileutil.ParseChecksumType(string(cfg.Checksum))
		if err != nil {
			return nil, err
		}
		cfg.Checksum = sum
	}

	fi, err := os.Stat(cfg.Root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("the folder %q doesn't exist", cfg.Root)
	case err != nil:
		return nil, fmt.Errorf("couldn't open the folder %q (%s)", cfg.Root, reasonOf(err))
	case !fi.IsDir():
		return nil, fmt.Errorf("%q isn't a folder", cfg.Root)
	}
	excludes, err := newExclusions(cfg.Root, cfg.Exclude)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("couldn't open the folder %q (%s)", cfg.Root, reasonOf(err))
	}

	r := &Rebalancer{
		cfg:      cfg,
		log:      cfg.Logger,
		state:    cfg.State,
		root:     root,
		opts:     fileutil.Options{Checksum: cfg.Checksum},
		excludes: excludes,
		stopCh:   make(chan struct{}),
	}
	if r.log == nil {
		r.log = logrus.New()
		r.log.SetOutput(io.Discard)
	}
	if r.state == nil {
		r.state = &memoryState{counts: make(map[string]int)}
	}
	return r, nil
}

// Close releases the folder opened by New. Scan and Execute can't be used afterwards.
func (r *Rebalancer) Close() error { return r.root.Close() }

// Stop asks a running Execute to finish the files it is working on and then return, without
// starting any more. It is safe to call from any goroutine, any number of times, and is permanent:
// a later Execute does nothing.
func (r *Rebalancer) Stop() { r.stopWith(Interrupted) }

func (r *Rebalancer) stopWith(reason StopReason) {
	r.reason.CompareAndSwap(int32(None), int32(reason))
	r.stopOnce.Do(func() { close(r.stopCh) })
}

func (r *Rebalancer) stopping() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// Progress returns a snapshot of the current (or last) Execute.
func (r *Rebalancer) Progress() Progress {
	p := Progress{
		Total:          int(r.total.Load()),
		Rebalanced:     int(r.rebalanced.Load()),
		Failed:         int(r.failed.Load()),
		Skipped:        int(r.skipped.Load()),
		BytesTotal:     r.bytesTotal.Load(),
		BytesDone:      r.bytesDone.Load(),
		BytesRewritten: r.bytesRewritten.Load(),
	}
	p.Done = p.Rebalanced + p.Failed + p.Skipped
	if t := r.started.Load(); t != nil {
		p.Started = *t
	}
	return p
}

// memoryState is the StateStore used when Config.State is nil.
type memoryState struct {
	mu     sync.Mutex
	counts map[string]int
}

func (m *memoryState) Counts() (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.counts), nil
}

func (m *memoryState) Increment(rel string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[rel]++
	return m.counts[rel], nil
}

// exclusions are the paths the walk must leave alone. Paths are compared both as given and with
// symlinks in their folders resolved (on macOS /var is a symlink to /private/var, for example), and
// files are also matched by identity, so a spelling difference can never get a database rewritten.
type exclusions struct {
	given    []string                 // as passed in Config.Exclude
	paths    map[string]bool          // cleaned absolute paths, in every spelling known
	rootDirs []string                 // the root, as given and resolved
	ids      map[fileutil.FileID]bool // filled in by Scan, since files may appear after New
}

func newExclusions(root string, given []string) (exclusions, error) {
	e := exclusions{given: given, paths: make(map[string]bool), rootDirs: []string{root}}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		e.rootDirs = append(e.rootDirs, resolved)
	}
	for _, p := range given {
		if !filepath.IsAbs(p) {
			return exclusions{}, fmt.Errorf("the path to leave alone %q must be a full path, starting with /", p)
		}
		p = filepath.Clean(p)
		e.paths[p] = true
		if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			e.paths[filepath.Join(dir, filepath.Base(p))] = true
		}
	}
	return e, nil
}

// refreshIDs records the identity of every excluded path that exists right now.
func (e *exclusions) refreshIDs() {
	e.ids = make(map[fileutil.FileID]bool, len(e.given))
	for _, p := range e.given {
		if fi, err := os.Stat(p); err == nil {
			if info, err := fileutil.InfoOf(fi); err == nil {
				e.ids[info.ID] = true
			}
		}
	}
}

// match reports whether rel (relative to the root, slash-separated), whose identity is id, must be
// left alone.
func (e *exclusions) match(rel string, id fileutil.FileID) bool {
	if len(e.given) == 0 {
		return false
	}
	if e.ids[id] {
		return true
	}
	for _, dir := range e.rootDirs {
		if e.paths[filepath.Join(dir, filepath.FromSlash(rel))] {
			return true
		}
	}
	return false
}

// reasonOf is a short, path-free description of err, such as "permission denied".
func reasonOf(err error) string {
	var pe *os.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return pe.Err.Error()
	case errors.As(err, &le):
		return le.Err.Error()
	}
	return err.Error()
}
