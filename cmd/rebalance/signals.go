package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/sirupsen/logrus"
)

// notifySignals sends Ctrl+C (SIGINT), SIGTERM and SIGHUP to c until the returned function is
// called. SIGHUP is left alone when it is already ignored, as under nohup, so the run carries on
// after a closed SSH session. Tests replace it to send signals of their own.
var notifySignals = func(c chan<- os.Signal) (stop func()) {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	signal.Notify(c, sigs...)
	return func() { signal.Stop(c) }
}

// watchSignals turns the first signal into a gentle stop (the files in progress are finished and
// nothing new is started) and the second into an immediate one (the files in progress are
// abandoned, each left exactly as it was). The returned function stops watching.
func watchSignals(log *logrus.Logger, gentle, now context.CancelFunc) (unwatch func()) {
	sigs := make(chan os.Signal, 2)
	stopNotify := notifySignals(sigs)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := 1; ; n++ {
			select {
			case <-done:
				return
			case <-sigs:
			}
			// Cancel before logging: the log may be busy, and the stop shouldn't wait for it.
			switch n {
			case 1:
				gentle()
				log.Warn("Stopping after the files in progress… press Ctrl+C again to stop right away (still safe)")
			case 2:
				now()
				log.Warn("Stopping right away. Files still being copied are abandoned, and their originals are left exactly as they were.")
			}
		}
	})
	return func() {
		stopNotify()
		close(done)
		wg.Wait()
	}
}
