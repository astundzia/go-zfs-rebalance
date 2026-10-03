package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// phase is the part of a run in progress. It decides what a stop request does and what the run
// says about it.
type phase int32

const (
	phaseStarting  phase = iota // getting ready and looking through the folder: nothing changed yet
	phaseCopying                // rewriting files
	phaseWaiting                // waiting for ZFS to free the old copies' space
	phaseFinishing              // the files are done; summing up and showing the vdev table
)

const (
	// repeatGap is how long after the last stop request a Ctrl+C or SIGTERM must come to count as
	// a new one. Anything sooner is taken as the same request arriving twice: one Ctrl+C can reach
	// the program both directly and through sudo, and a service manager may signal the whole group.
	repeatGap = time.Second
	// exitLogWait is how long the immediate exit waits for its message to be written.
	exitLogWait = 200 * time.Millisecond
)

// Test seams: how the run hears about signals, the clock that tells a repeated signal from a new
// one, how the process quits on a third Ctrl+C, and a call made once each signal is handled.
var (
	// notifySignals sends Ctrl+C (SIGINT), SIGTERM, SIGPIPE and SIGHUP to c until the returned
	// function is called. SIGHUP is left alone when it is already ignored, as under nohup, so the
	// run carries on after a closed SSH session. Catching SIGPIPE means a closed pipe on stdout or
	// stderr makes writes fail instead of killing the process, so the run can stop gently.
	//
	// SIGPIPE stays caught after that, for the rest of the process (see keepCatchingSIGPIPE).
	notifySignals = func(c chan<- os.Signal) (stop func()) {
		sigs := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE}
		if !signal.Ignored(syscall.SIGHUP) {
			sigs = append(sigs, syscall.SIGHUP)
		}
		signal.Notify(c, sigs...)
		keepCatchingSIGPIPE()
		return func() { signal.Stop(c) }
	}
	clock       = time.Now
	exitNow     = os.Exit
	afterSignal = func() {}
)

// keepCatchingSIGPIPE makes sure SIGPIPE is caught until the process ends, so a write to a closed
// stdout or stderr fails instead of killing the process (exit code 141). Without it, a message
// about the stop written just after the run stops watching signals, such as one still on its way
// to a pipe whose reader has quit, would get the process killed instead of ending with 130.
// SIGPIPE is never ignored instead: changing it to ignored opens a moment in which a late SIGPIPE
// from an earlier failed write, which macOS can deliver some time afterwards, kills the process.
// The signals sent to the channel are never read; the signal package drops them once it is full.
var keepCatchingSIGPIPE = sync.OnceFunc(func() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
})

// stopper turns signals into stop requests:
//
//   - The first signal of any kind stops gently: the files in progress are finished and nothing
//     new is started, or the wait for ZFS to free space is skipped.
//   - A later Ctrl+C or SIGTERM, at least repeatGap after the first, stops right away: the files in
//     progress are abandoned, each left exactly as it was.
//   - Another, at least repeatGap after that, quits immediately with exit code 130.
//
// A hangup (SIGHUP) or a closed output (SIGPIPE) never does more than the gentle stop, since
// nobody pressed anything: a dropped SSH session can deliver several hangups at once, and every
// write to a closed pipe raises another SIGPIPE.
type stopper struct {
	phase atomic.Int32
}

// setPhase records which part of the run is in progress. It does nothing on a nil stopper.
func (s *stopper) setPhase(p phase) {
	if s != nil {
		s.phase.Store(int32(p))
	}
}

// watchSignals starts turning signals into calls to gentle and now (see stopper). The returned
// function stops watching and waits for any message about a stop to be written; calling it again
// does nothing.
func watchSignals(log *logrus.Logger, gentle, now context.CancelFunc) (s *stopper, unwatch func()) {
	s = &stopper{}
	sigs := make(chan os.Signal, 8)
	stopNotify := notifySignals(sigs)
	done := make(chan struct{})
	var wg sync.WaitGroup
	// Messages are written from their own goroutines, so a log that can't be written to (a full
	// pipe, say) never holds up the next signal.
	say := func(level logrus.Level, msg string) (written <-chan struct{}) {
		c := make(chan struct{})
		wg.Go(func() {
			defer close(c)
			log.Log(level, msg)
		})
		return c
	}
	wg.Go(func() {
		stopping, stoppingNow := false, false
		var last time.Time // when the last stop request that did something arrived
		for {
			var sig os.Signal
			select {
			case <-done:
				return
			case sig = <-sigs:
			}
			t, p := clock(), phase(s.phase.Load())
			pressed := sig == os.Interrupt || sig == syscall.SIGTERM
			switch {
			case !stopping:
				stopping, last = true, t
				gentle()
				say(logrus.WarnLevel, gentleMessage(sig, p))
			case !pressed:
				// A hangup or closed output only ever asks for the gentle stop.
			case t.Sub(last) < repeatGap:
				say(logrus.DebugLevel, "That stop request came straight after the last one, so it was taken as the same one")
			case !stoppingNow:
				stoppingNow, last = true, t
				now()
				say(logrus.WarnLevel, nowMessage(p))
			default:
				select {
				case <-say(logrus.WarnLevel, quitMessage(p)):
				case <-time.After(exitLogWait):
				}
				exitNow(exitInterrupted)
				afterSignal()
				return
			}
			afterSignal()
		}
	})
	var once sync.Once
	return s, func() {
		once.Do(func() {
			stopNotify()
			close(done)
			wg.Wait()
		})
	}
}

// gentleMessage is what the run says when the first stop request, sig, arrives during p.
func gentleMessage(sig os.Signal, p phase) string {
	var what string
	switch p {
	case phaseStarting:
		what = "stopping before any files are changed…"
	case phaseCopying:
		what = "stopping after the files in progress…"
	case phaseWaiting:
		what = "skipping the wait for ZFS to free space…"
	default:
		what = "finishing up, then stopping…"
	}
	switch sig {
	case syscall.SIGHUP:
		return "The terminal or connection was closed, so the run is " + what + still(p)
	case syscall.SIGPIPE:
		return "The output was closed, so the run is " + what + still(p)
	}
	msg := strings.ToUpper(what[:1]) + what[1:]
	if p == phaseCopying {
		msg += " press Ctrl+C again to stop right away (still safe)"
	}
	return msg
}

// still is the reassurance added while files are being copied.
func still(p phase) string {
	if p == phaseCopying {
		return " (still safe)"
	}
	return ""
}

// nowMessage is what the run says when a second Ctrl+C or SIGTERM arrives during p.
func nowMessage(p phase) string {
	switch p {
	case phaseStarting:
		return "Stopping right away. No files have been changed."
	case phaseCopying:
		return "Stopping right away. Files still being copied are abandoned, and their originals are left exactly as they were."
	case phaseWaiting:
		return "Skipping the wait for ZFS to free space…"
	}
	return "Just finishing up… press Ctrl+C once more to quit right away."
}

// quitMessage is what the run says just before a third Ctrl+C or SIGTERM ends it.
func quitMessage(p phase) string {
	if p == phaseCopying {
		return "Quitting right away. The originals are safe; any temporary copies left behind are removed by the next run."
	}
	return "Quitting right away."
}
