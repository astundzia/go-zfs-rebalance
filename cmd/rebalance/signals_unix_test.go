//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realNotifySignals is the real signal subscription, before TestMain swaps in one that does nothing.
var realNotifySignals = notifySignals

func TestRealSignalsAreCaught(t *testing.T) {
	c := make(chan os.Signal, 4)
	stop := realNotifySignals(c)
	defer stop()
	sigs := []syscall.Signal{syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	// On macOS, Go ignores a SIGPIPE sent by another process and only reports the one from a
	// failed write to stdout or stderr, which TestBrokenPipeStopsGently checks.
	if runtime.GOOS == "linux" {
		sigs = append(sigs, syscall.SIGPIPE)
	}
	for _, sig := range sigs {
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-c:
			if got != sig {
				t.Errorf("got %v, want %v", got, sig)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v wasn't caught", sig)
		}
	}
}

// TestBrokenPipeWhileStarting closes the run's output while it is still getting ready (looking for
// its ZFS dataset), and has the program write once more after the run is over, as a stop message
// written late would. Neither may kill it with SIGPIPE: it stops before rewriting every file, leaves
// everything as it was, and exits with 130.
func TestBrokenPipeWhileStarting(t *testing.T) {
	isolateState(t)
	root := tempDir(t)
	content := bytes.Repeat([]byte("rebalance "), 25_000)
	var names []string
	for i := range 50 {
		name := fmt.Sprintf("f%02d", i)
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := inodes(t, root, names)

	cmd := exec.Command(os.Args[0], "--no-random", "--concurrency", "1", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	cmd.Env = append(os.Environ(), helperEnv+"="+helperSlowStart)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	first, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil || !strings.Contains(first, "Rebalancing "+root) {
		t.Fatalf("first line %q, %v", first, err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("the run didn't stop after its output was closed")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exit error %v, want exit code %d", err, exitInterrupted)
	}
	if ws := exitErr.Sys().(syscall.WaitStatus); ws.Signaled() {
		t.Fatalf("the run was killed by %v instead of stopping gently", ws.Signal())
	}
	if code := exitErr.ExitCode(); code != exitInterrupted {
		t.Fatalf("exit %d, want %d", code, exitInterrupted)
	}
	rewritten := 0
	for name, ino := range inodes(t, root, names) {
		if ino != before[name] {
			rewritten++
		}
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: contents changed (%v)", name, err)
		}
	}
	t.Logf("%d of %d files were rewritten before the run stopped", rewritten, len(names))
	if rewritten == len(names) {
		t.Error("every file was rewritten; the closed output didn't stop the run")
	}
	checkTree(t, root, nil)
}

// TestBrokenPipeStopsGently runs the command with its log going into a pipe that is closed part-way
// through, as when piping into head or quitting a pager. The run must stop gently: finish the file
// in progress, start no more, leave no temporary files, and exit with 130 rather than being killed.
func TestBrokenPipeStopsGently(t *testing.T) {
	isolateState(t)
	root := tempDir(t)
	const n = 300
	var names []string
	content := bytes.Repeat([]byte("rebalance "), 25_000) // 250 KB each, so the run takes a while
	for i := range n {
		name := fmt.Sprintf("f%03d", i)
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := inodes(t, root, names)

	cmd := exec.Command(os.Args[0], "--no-random", "--concurrency", "1", "--db", filepath.Join(t.TempDir(), "p.db"), root)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var seen []string
	lines := bufio.NewScanner(stderr)
	for lines.Scan() {
		seen = append(seen, lines.Text())
		if strings.Contains(lines.Text(), "✓ rebalanced") {
			break
		}
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatalf("the run didn't stop after its output was closed; it said:\n%s", strings.Join(seen, "\n"))
	}
	t.Logf("output before the pipe was closed:\n%s", strings.Join(seen, "\n"))

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		t.Fatal("exit 0, want 130: the run didn't stop")
	case !errors.As(err, &exitErr):
		t.Fatal(err)
	}
	if ws := exitErr.Sys().(syscall.WaitStatus); ws.Signaled() {
		t.Fatalf("the run was killed by %v instead of stopping gently", ws.Signal())
	}
	if code := exitErr.ExitCode(); code != exitInterrupted {
		t.Fatalf("exit %d, want %d", code, exitInterrupted)
	}

	rewritten := 0
	for name, ino := range inodes(t, root, names) {
		if ino != before[name] {
			rewritten++
		}
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("%s: contents changed (%v)", name, err)
		}
	}
	if rewritten == 0 || rewritten == n {
		t.Errorf("%d of %d files were rewritten; want some but not all", rewritten, n)
	}
	checkTree(t, root, nil)
}
