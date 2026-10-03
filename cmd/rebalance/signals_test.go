package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// captureSignals makes run's signal handling use a channel the test can send to. The channel is
// delivered on the returned channel when the run starts watching.
func captureSignals(t *testing.T) <-chan chan<- os.Signal {
	t.Helper()
	got := make(chan chan<- os.Signal, 4)
	old := notifySignals
	notifySignals = func(c chan<- os.Signal) func() {
		got <- c
		return func() {}
	}
	t.Cleanup(func() { notifySignals = old })
	return got
}

func TestWatchSignals(t *testing.T) {
	sigs := captureSignals(t)
	log, buf := testLogger(logrus.InfoLevel)
	gentle, now := make(chan struct{}, 4), make(chan struct{}, 4)
	unwatch := watchSignals(log, func() { gentle <- struct{}{} }, func() { now <- struct{}{} })
	c := <-sigs

	c <- os.Interrupt
	waitFor(t, gentle, "the first signal to stop gently")
	if len(now) != 0 {
		t.Fatal("the first signal stopped right away")
	}
	c <- syscall.SIGTERM
	waitFor(t, now, "the second signal to stop right away")
	c <- syscall.SIGHUP // a third does nothing more
	time.Sleep(10 * time.Millisecond)
	unwatch()

	if len(gentle) != 0 || len(now) != 0 {
		t.Error("a later signal cancelled again")
	}
	out := buf.String()
	if strings.Count(out, "Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)") != 1 ||
		strings.Count(out, "Stopping right away.") != 1 {
		t.Errorf("unexpected messages: %q", out)
	}
}

func waitFor(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
