package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/astundzia/go-zfs-rebalance/v2/internal/zfs"
	"github.com/sirupsen/logrus"
)

func TestMain(m *testing.M) {
	// Keep the tests hermetic: no real signal handlers, and no real zfs or zpool commands even on
	// a computer that has ZFS. Tests that need either replace these again.
	notifySignals = func(chan<- os.Signal) func() { return func() {} }
	newZFSClient = func() *zfs.Client { return &zfs.Client{Runner: noZFS{}} }
	os.Exit(m.Run())
}

// noZFS fails every command as if the zfs tools weren't installed.
type noZFS struct{}

func (noZFS) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
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
