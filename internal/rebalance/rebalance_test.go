//go:build linux || darwin

package rebalance

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
)

func TestNewRejectsBadConfig(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "file", []byte("x"))
	good := Config{Root: root, Passes: 1, Concurrency: 1}

	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"no folder", func(c *Config) { c.Root = "" }, "no folder"},
		{"relative folder", func(c *Config) { c.Root = "some/dir" }, "full path"},
		{"missing folder", func(c *Config) { c.Root = filepath.Join(root, "nope") }, "doesn't exist"},
		{"a file, not a folder", func(c *Config) { c.Root = filepath.Join(root, "file") }, "isn't a folder"},
		{"zero passes", func(c *Config) { c.Passes = 0 }, "passes"},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }, "at once"},
		{"negative threshold", func(c *Config) { c.SizeThresholdMB = -1 }, "can't be negative"},
		{"unknown checksum", func(c *Config) { c.Checksum = "crc32" }, "checksum"},
		{"relative exclude", func(c *Config) { c.Exclude = []string{"state.db"} }, "full path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.change(&cfg)
			r, err := New(cfg)
			if err == nil {
				_ = r.Close()
				t.Fatal("New accepted a bad config")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q doesn't mention %q", err, tt.want)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	root := tempRoot(t)
	r, err := New(Config{Root: root + "/", Passes: 1, Concurrency: 1, Checksum: " MD5 "})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.log == nil || r.state == nil {
		t.Fatal("a nil Logger or State wasn't given a default")
	}
	if r.cfg.Root != root {
		t.Errorf("root = %q, want it cleaned to %q", r.cfg.Root, root)
	}
	if r.opts.Checksum != fileutil.ChecksumMD5 {
		t.Errorf("checksum = %q, want md5", r.opts.Checksum)
	}
	// The default logger and state must work.
	writeFile(t, root, "f", []byte("data"))
	if _, s := scanAndExecute(t, r); s.Rebalanced != 1 {
		t.Errorf("rebalanced %d files, want 1", s.Rebalanced)
	}
}

func TestStopIsSafeToRepeat(t *testing.T) {
	root := tempRoot(t)
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, root, name, []byte(name))
	}
	r, _ := newRebalancer(t, Config{Root: root})
	p := scan(t, r)
	before := snapshot(t, root)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(r.Stop)
	}
	wg.Wait()
	r.Stop()
	r.Stop()

	s := execute(t, context.Background(), r, p)
	if s.Stopped != Interrupted || s.Rebalanced != 0 || s.Remaining != 3 {
		t.Errorf("summary after Stop = %+v, want nothing done and 3 remaining", s)
	}
	checkUntouched(t, before, snapshot(t, root), "a", "b", "c")
}

func TestStopAfterLastFileIsNotAnInterruption(t *testing.T) {
	root := tempRoot(t)
	writeFile(t, root, "only", []byte("x"))
	state := newHookState()
	r, _ := newRebalancer(t, Config{Root: root, State: state})
	state.onFirst = r.Stop // called as the only file finishes

	if _, s := scanAndExecute(t, r); s.Stopped != None || s.Rebalanced != 1 || s.Remaining != 0 {
		t.Errorf("summary = %+v", s)
	}
}

func TestStopReasonStrings(t *testing.T) {
	for _, s := range []StopReason{None, Interrupted, MissingFile, NoSpace} {
		if s.String() == "" || strings.Contains(s.String(), "StopReason") {
			t.Errorf("StopReason %d has no description", int(s))
		}
	}
}

func TestSkipReasonPhrase(t *testing.T) {
	tests := []struct {
		why  SkipReason
		n    int
		want string
	}{
		{SkipOrphanBalance, 1, "1 leftover .balance file"},
		{SkipOrphanBalance, 2, "2 leftover .balance files"},
		{SkipOwner, 1, "1 file owned by someone else"},
		{SkipOwner, 3, "3 files owned by someone else"},
		{SkipHardlinksOutside, 1, "1 file with hardlinks outside the folder"},
		{SkipAlreadyDone, 1234, "1,234 already done"},
		{SkipHardlinked, 1, "1 hardlinked"},
		{SkipNotRegular, 2, "2 not regular files"},
		{SkipMetadata, 2, "2 files whose permissions can't be kept exactly"},
		{SkipNoPermission, 1, "1 file you aren't allowed to replace"},
		{SkipImmutable, 5, "5 files marked immutable or append-only"},
		{SkipQuota, 1, "1 file whose owner or group is over quota"},
		{SkipReason("something new"), 1000000, "1,000,000 something new"},
	}
	for _, tt := range tests {
		if got := tt.why.Phrase(tt.n); got != tt.want {
			t.Errorf("%q.Phrase(%d) = %q, want %q", tt.why, tt.n, got, tt.want)
		}
	}
	every := []SkipReason{SkipAlreadyDone, SkipHardlinked, SkipHardlinksOutside, SkipOrphanBalance, SkipUnreadable,
		SkipMissing, SkipOwner, SkipChanged, SkipLinksChanged, SkipNotRegular, SkipMetadata, SkipNoPermission,
		SkipImmutable, SkipQuota}
	for _, why := range every {
		if _, ok := skipPhrases[why]; !ok {
			t.Errorf("%q has no phrases", why)
		}
		if one, two := why.Phrase(1), why.Phrase(2); !strings.HasPrefix(one, "1 ") || !strings.HasPrefix(two, "2 ") {
			t.Errorf("%q: %q, %q", why, one, two)
		}
	}
	if len(skipPhrases) != len(every) {
		t.Errorf("%d phrases for %d reasons", len(skipPhrases), len(every))
	}
}

func TestOutsideReason(t *testing.T) {
	tests := []struct {
		found int
		nlink uint64
		want  string
	}{
		{1, 2, "1 of its 2 hardlinked names is outside this folder, so it was left alone"},
		{1, 3, "2 of its 3 hardlinked names are outside this folder, so it was left alone"},
		{3, 2, "its hardlinks changed while the folder was being looked through, so it was left alone"},
	}
	for _, tt := range tests {
		if got := outsideReason(tt.found, tt.nlink); got != tt.want {
			t.Errorf("outsideReason(%d, %d) = %q, want %q", tt.found, tt.nlink, got, tt.want)
		}
	}
}

func TestExecuteNeedsAPlan(t *testing.T) {
	r, _ := newRebalancer(t, Config{Root: tempRoot(t)})
	if _, err := r.Execute(context.Background(), nil); err == nil {
		t.Fatal("Execute ran without a plan")
	}
}

func TestProgress(t *testing.T) {
	root := tempRoot(t)
	for i := range 20 {
		writeFile(t, root, filepath.Join("d", string(rune('a'+i))), randomBytes(32<<10))
	}
	r, _ := newRebalancer(t, Config{Root: root, Concurrency: 4})
	if !r.Progress().Started.IsZero() {
		t.Error("Progress says started before Execute")
	}
	p := scan(t, r)

	// Read progress while the run is going, so the race detector can check it.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				if pr := r.Progress(); pr.Done > pr.Total {
					t.Errorf("progress done %d > total %d", pr.Done, pr.Total)
				}
			}
		}
	})
	s := execute(t, context.Background(), r, p)
	close(done)
	wg.Wait()

	pr := r.Progress()
	if pr.Started.IsZero() || pr.Total != 20 || pr.Done != 20 || pr.Rebalanced != 20 ||
		pr.BytesTotal != p.TotalBytes || pr.BytesDone != p.TotalBytes || pr.BytesRewritten != p.TotalBytes {
		t.Errorf("final progress = %+v, plan bytes %d", pr, p.TotalBytes)
	}
	if s.Bytes != p.TotalBytes || s.Duration <= 0 {
		t.Errorf("summary = %+v", s)
	}
}
