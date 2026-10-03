//go:build linux || darwin

package rebalance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/sirupsen/logrus"
)

// logEntry is a captured log line.
type logEntry struct {
	level logrus.Level
	msg   string
	data  logrus.Fields
}

// logCapture is a logrus hook that keeps every entry.
type logCapture struct {
	mu      sync.Mutex
	entries []logEntry
}

func (c *logCapture) Levels() []logrus.Level { return logrus.AllLevels }

func (c *logCapture) Fire(e *logrus.Entry) error {
	data := make(logrus.Fields, len(e.Data))
	for k, v := range e.Data {
		data[k] = v
	}
	c.mu.Lock()
	c.entries = append(c.entries, logEntry{level: e.Level, msg: e.Message, data: data})
	c.mu.Unlock()
	return nil
}

// op returns the entries whose "op" field is op.
func (c *logCapture) op(op string) []logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []logEntry
	for _, e := range c.entries {
		if e.data["op"] == op {
			out = append(out, e)
		}
	}
	return out
}

// checkNoPathsInMessages fails if any message repeats the path it is about.
func (c *logCapture) checkNoPathsInMessages(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if p, ok := e.data["path"].(string); ok && strings.Contains(e.msg, p) {
			t.Errorf("log message %q contains its path %q", e.msg, p)
		}
	}
}

func newTestLogger() (*logrus.Logger, *logCapture) {
	l := logrus.New()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.DebugLevel)
	c := &logCapture{}
	l.AddHook(c)
	return l, c
}

// tempRoot returns a new empty folder with symlinks resolved, as the command passes it.
func tempRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails
	return b
}

func lstatInfo(t *testing.T, p string) fileutil.Info {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	info, err := fileutil.InfoOf(fi)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// fileState is what a test remembers about a file to compare after a run.
type fileState struct {
	id  fileutil.FileID
	sum [sha256.Size]byte
}

// snapshot records every regular file under root, keyed by slash-separated relative path.
func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := make(map[string]fileState)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = fileState{id: lstatInfo(t, p).ID, sum: sha256.Sum256(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkRewritten fails unless each rel has the same contents as before but a new inode.
func checkRewritten(t *testing.T, before, after map[string]fileState, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		b, a := before[rel], after[rel]
		switch {
		case b == fileState{}:
			t.Errorf("%s: wasn't there before the run", rel)
		case a == fileState{}:
			t.Errorf("%s: missing after the run", rel)
		case a.sum != b.sum:
			t.Errorf("%s: contents changed", rel)
		case a.id == b.id:
			t.Errorf("%s: wasn't rewritten (same inode)", rel)
		}
	}
}

// checkUntouched fails unless each rel is still the very same file.
func checkUntouched(t *testing.T, before, after map[string]fileState, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if b, a := before[rel], after[rel]; b == (fileState{}) || a != b {
			t.Errorf("%s: should have been left alone (before %v, after %v)", rel, b, a)
		}
	}
}

// checkSameContents fails if any file's contents changed or a file disappeared.
func checkSameContents(t *testing.T, before, after map[string]fileState) {
	t.Helper()
	for rel, b := range before {
		if a, ok := after[rel]; !ok || a.sum != b.sum {
			t.Errorf("%s: contents lost or changed", rel)
		}
	}
}

func checkNoTemps(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && fileutil.IsTempName(d.Name()) {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
}

// newRebalancer fills in test defaults (1 pass, 2 at once, a capturing logger) and opens cfg.
func newRebalancer(t *testing.T, cfg Config) (*Rebalancer, *logCapture) {
	t.Helper()
	if cfg.Passes == 0 {
		cfg.Passes = 1
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	var logs *logCapture
	if cfg.Logger == nil {
		cfg.Logger, logs = newTestLogger()
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, logs
}

func scan(t *testing.T, r *Rebalancer) *Plan {
	t.Helper()
	p, err := r.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return p
}

func execute(t *testing.T, ctx context.Context, r *Rebalancer, p *Plan) Summary {
	t.Helper()
	s, err := r.Execute(ctx, p)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return s
}

func scanAndExecute(t *testing.T, r *Rebalancer) (*Plan, Summary) {
	t.Helper()
	p := scan(t, r)
	return p, execute(t, context.Background(), r, p)
}

// hookState is an in-memory StateStore that counts every Increment and can run a function on
// the first one.
type hookState struct {
	memoryState
	once    sync.Once
	onFirst func()
}

func newHookState() *hookState { return &hookState{memoryState: memoryState{counts: map[string]int{}}} }

func (h *hookState) Increment(rel string) (int, error) {
	n, err := h.memoryState.Increment(rel)
	if h.onFirst != nil {
		h.once.Do(h.onFirst)
	}
	return n, err
}

func (h *hookState) count(rel string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[rel]
}

// itemNames lists every name in a plan, sorted.
func itemNames(p *Plan) []string {
	var names []string
	for _, it := range p.Items {
		names = append(names, it.Names...)
	}
	slices.Sort(names)
	return names
}

// stubReplace makes the rewrite of the item whose primary name is rel fail with err, and lets
// the others through. It is undone when the test ends.
func stubReplace(t *testing.T, rel string, err error) {
	t.Helper()
	t.Cleanup(func() { replace = fileutil.ReplaceGroup })
	replace = func(ctx context.Context, root *os.Root, rels []string, opts fileutil.Options) (fileutil.Result, error) {
		if rels[0] == rel {
			return fileutil.Result{}, err
		}
		return fileutil.ReplaceGroup(ctx, root, rels, opts)
	}
}
