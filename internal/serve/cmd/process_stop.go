package cmd

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

const processStopBudget = 250 * time.Millisecond

var (
	processStopInstalled atomic.Bool
	activeProcessStop    atomic.Pointer[processStopAction]
)

type processStopAction struct{ stop func(int) }

func requestProcessInterrupt() bool {
	if action := activeProcessStop.Load(); action != nil {
		action.stop(130)
		return true
	}
	return false
}

// InstallProcessStop устанавливается только CLI, не библиотекой SDK.
func InstallProcessStop() func() {
	if !processStopInstalled.CompareAndSwap(false, true) {
		return func() {}
	}
	stop := newProcessStop(session.StopTrackedProcessTrees, os.Exit, func(f func()) func() {
		timer := time.AfterFunc(processStopBudget, f)
		return func() { timer.Stop() }
	})
	activeProcessStop.Store(&processStopAction{stop: stop})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, cancelSignals...)
	uninstallConsole := installConsoleProcessStop(stop)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			code := 130
			if sig == syscall.SIGTERM {
				code = 143
			}
			stop(code)
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			uninstallConsole()
			signal.Stop(signals)
			close(done)
			activeProcessStop.Store(nil)
			processStopInstalled.Store(false)
		})
	}
}

func newProcessStop(stopTrees func(), exit func(int), armDeadline func(func()) func()) func(int) {
	var once sync.Once
	return func(code int) {
		once.Do(func() {
			// Никаких журналов, отмены с cleanup или ожидания дисковых mutex.
			stopDeadline := armDeadline(func() { exit(code) })
			defer stopDeadline()
			stopTrees()
			exit(code)
		})
	}
}

func commandSignals(signals ...os.Signal) []os.Signal {
	if processStopInstalled.Load() {
		return nil
	}
	return signals
}

func commandSignalContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	if processStopInstalled.Load() {
		return context.WithCancel(parent)
	}
	return signal.NotifyContext(parent, signals...)
}
