package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/fileutil"
	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

// helperEnv makes the test binary run as the rebalance command itself, with real signal handling.
// Its arguments are then the command's. Its value is "1", or helperSlowStart.
const helperEnv = "REBALANCE_CMD_HELPER"

// helperSlowStart makes the helper take a while to look for the folder's ZFS dataset, so a test can
// act while the run is still starting, and write one more line once the run is over, as a message
// written late would be.
const helperSlowStart = "slow-start"

func TestMain(m *testing.M) {
	// Never run real zfs or zpool commands, even on a computer that has ZFS. Tests that need them
	// replace this again.
	newZFSClient = func() *zfs.Client { return &zfs.Client{Runner: noZFS{}} }
	if mode := os.Getenv(helperEnv); mode != "" {
		// Be the rebalance command itself, with real signal handling (see TestBrokenPipeStopsGently).
		os.Exit(runHelper(mode))
	}
	// Keep the tests hermetic: no real signal handlers. Tests that need signals replace this again.
	notifySignals = func(chan<- os.Signal) func() { return func() {} }
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	if mode != helperSlowStart {
		return run(os.Args[1:], os.Stdout, os.Stderr)
	}
	newZFSClient = func() *zfs.Client { return &zfs.Client{Runner: slowNoZFS{}} }
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	fmt.Fprintln(os.Stderr, "A line written after the run")
	return code
}

// noZFS fails every command as if the zfs tools weren't installed.
type noZFS struct{}

func (noZFS) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// slowNoZFS is noZFS, after half a second.
type slowNoZFS struct{}

func (slowNoZFS) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}
	return noZFS{}.Run(ctx, name, args...)
}

// syncBuffer is a bytes.Buffer that is safe to write from several goroutines. With hook set, each
// write is passed to it after being stored.
type syncBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	hook func(p string)
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf.Write(p)
	b.mu.Unlock()
	if b.hook != nil {
		b.hook(string(p))
	}
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger returns a logger using the run's formatter, without colour, writing to a buffer.
func testLogger(level logrus.Level) (*logrus.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	log := newLogger(buf, false, false)
	log.SetLevel(level)
	return log, buf
}

// tempDir is t.TempDir with symlinks resolved, as run resolves the folder it is given (on macOS
// the temporary folder is reached through the /var symlink).
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q", want)
		}
	}
}

func mustNotContain(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("output unexpectedly contains %q", u)
		}
	}
}

func TestOutputIDs(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	want, err := fileutil.InfoOf(fi)
	if err != nil {
		t.Fatal(err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	got := outputIDs(&bytes.Buffer{}, devNull, f)
	if len(got) != 1 || got[0] != want.ID {
		t.Errorf("outputIDs = %v, want just the regular file's %v", got, want.ID)
	}
}
