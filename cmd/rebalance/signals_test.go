package main

import (
	"os"
	"strings"
	"sync"
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

// testClock replaces the clock that tells a repeated signal from a new one. The returned function
// moves it on.
func testClock(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	old := clock
	clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	t.Cleanup(func() { clock = old })
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

// captureExit records the exit code of a third Ctrl+C instead of quitting.
func captureExit(t *testing.T) <-chan int {
	t.Helper()
	codes := make(chan int, 4)
	old := exitNow
	exitNow = func(code int) { codes <- code }
	t.Cleanup(func() { exitNow = old })
	return codes
}

// signalTest is a stopper watching a test channel, with what it has done so far.
type signalTest struct {
	t       *testing.T
	c       chan<- os.Signal
	s       *stopper
	unwatch func()
	advance func(time.Duration)
	handled chan struct{}
	exits   <-chan int
	gentle  chan struct{}
	now     chan struct{}
	log     *syncBuffer
}

func newSignalTest(t *testing.T) *signalTest {
	sigs := captureSignals(t)
	st := &signalTest{t: t, advance: testClock(t), exits: captureExit(t), handled: make(chan struct{}, 1),
		gentle: make(chan struct{}, 8), now: make(chan struct{}, 8)}
	old := afterSignal
	afterSignal = func() { st.handled <- struct{}{} }
	t.Cleanup(func() { afterSignal = old })
	var log *logrus.Logger
	log, st.log = testLogger(logrus.DebugLevel)
	st.s, st.unwatch = watchSignals(log, func() { st.gentle <- struct{}{} }, func() { st.now <- struct{}{} })
	st.c = <-sigs
	t.Cleanup(st.unwatch)
	return st
}

// send delivers sig after the clock has moved on by after, and waits until it has been handled.
func (st *signalTest) send(after time.Duration, sig os.Signal) {
	st.t.Helper()
	st.advance(after)
	st.c <- sig
	waitFor(st.t, st.handled, "the signal to be handled")
}

// counts reports how many gentle stops, immediate stops and exits there have been.
func (st *signalTest) counts() (gentle, now, exits int) {
	return len(st.gentle), len(st.now), len(st.exits)
}

func TestWatchSignals(t *testing.T) {
	st := newSignalTest(t)
	st.s.setPhase(phaseCopying)
	want := func(gentle, now, exits int, after string) {
		t.Helper()
		if g, n, e := st.counts(); g != gentle || n != now || e != exits {
			t.Fatalf("after %s: %d gentle stops, %d immediate stops, %d exits; want %d, %d, %d", after, g, n, e, gentle, now, exits)
		}
	}

	st.send(0, os.Interrupt)
	want(1, 0, 0, "the first Ctrl+C")
	st.send(500*time.Millisecond, os.Interrupt)
	want(1, 0, 0, "a second Ctrl+C half a second later (the same press delivered twice)")
	st.send(time.Second, syscall.SIGHUP)
	st.send(time.Second, syscall.SIGPIPE)
	want(1, 0, 0, "a hangup and a broken pipe")
	st.send(0, syscall.SIGTERM)
	want(1, 1, 0, "SIGTERM more than a second after the first stop")
	st.send(900*time.Millisecond, os.Interrupt)
	want(1, 1, 0, "a Ctrl+C straight after that")
	st.send(200*time.Millisecond, os.Interrupt)
	want(1, 1, 1, "a third Ctrl+C a second later")
	if code := <-st.exits; code != exitInterrupted {
		t.Errorf("exit code %d, want %d", code, exitInterrupted)
	}
	st.unwatch()

	out := st.log.String()
	for msg, n := range map[string]int{
		"! Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)": 1,
		"! Stopping right away. Files still being copied are abandoned":                              1,
		"! Quitting right away. The originals are safe":                                              1,
		"· That stop request came straight after the last one":                                       2,
	} {
		if got := strings.Count(out, msg); got != n {
			t.Errorf("%q logged %d times, want %d:\n%s", msg, got, n, out)
		}
	}
}

func TestHangupAndBrokenPipeNeverStopRightAway(t *testing.T) {
	for _, first := range []os.Signal{syscall.SIGHUP, syscall.SIGPIPE} {
		t.Run(first.String(), func(t *testing.T) {
			st := newSignalTest(t)
			st.s.setPhase(phaseCopying)
			for range 5 {
				st.send(2*time.Second, first)
				st.send(0, syscall.SIGHUP)
				st.send(0, syscall.SIGPIPE)
			}
			if g, n, e := st.counts(); g != 1 || n != 0 || e != 0 {
				t.Fatalf("%d gentle stops, %d immediate stops, %d exits; want just one gentle stop", g, n, e)
			}
			// Someone pressing Ctrl+C later still stops it right away.
			st.send(2*time.Second, os.Interrupt)
			if _, n, _ := st.counts(); n != 1 {
				t.Errorf("a Ctrl+C after a %v didn't stop the run right away", first)
			}
		})
	}
}

func TestStopMessages(t *testing.T) {
	tests := []struct {
		sig     os.Signal
		p       phase
		gentle  string
		quickly string
	}{
		{os.Interrupt, phaseStarting, "Stopping before any files are changed…",
			"Stopping right away. No files have been changed."},
		{os.Interrupt, phaseCopying, "Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)",
			"Stopping right away. Files still being copied are abandoned, and their originals are left exactly as they were."},
		{syscall.SIGTERM, phaseWaiting, "Skipping the wait for ZFS to free space…", "Skipping the wait for ZFS to free space…"},
		{os.Interrupt, phaseFinishing, "Finishing up, then stopping…",
			"Just finishing up… press Ctrl+C once more to quit right away."},
		{syscall.SIGHUP, phaseCopying, "The terminal or connection was closed, so the run is stopping after the files in progress… (still safe)", ""},
		{syscall.SIGPIPE, phaseStarting, "The output was closed, so the run is stopping before any files are changed…", ""},
		{syscall.SIGPIPE, phaseWaiting, "The output was closed, so the run is skipping the wait for ZFS to free space…", ""},
	}
	for _, tt := range tests {
		if got := gentleMessage(tt.sig, tt.p); got != tt.gentle {
			t.Errorf("gentleMessage(%v, %d) = %q, want %q", tt.sig, tt.p, got, tt.gentle)
		}
		if tt.quickly != "" {
			if got := nowMessage(tt.p); got != tt.quickly {
				t.Errorf("nowMessage(%d) = %q, want %q", tt.p, got, tt.quickly)
			}
		}
	}
	for _, p := range []phase{phaseStarting, phaseWaiting, phaseFinishing} {
		if msg := gentleMessage(os.Interrupt, p) + nowMessage(p) + quitMessage(p); strings.Contains(msg, "being copied") {
			t.Errorf("phase %d talks about files being copied: %q", p, msg)
		}
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
