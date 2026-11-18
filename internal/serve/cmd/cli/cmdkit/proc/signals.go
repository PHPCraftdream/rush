// Process stop wiring and command-signal helpers (merged from
// process_stop.go + run_signals.go, lane K1).
package cliproc

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/proctrack"
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
	stop := newProcessStop(proctrack.StopTrackedProcessTrees, os.Exit, func(f func()) func() {
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

func CommandSignals(signals ...os.Signal) []os.Signal {
	if processStopInstalled.Load() {
		return nil
	}
	return signals
}

func CommandSignalContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	if processStopInstalled.Load() {
		return context.WithCancel(parent)
	}
	return signal.NotifyContext(parent, signals...)
}

var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// ProcessStopInstalled reports whether the process-level stop handler is
// active (lane K1 export: serve.go/ps.go read it).
func ProcessStopInstalled() bool { return processStopInstalled.Load() }

// RequestProcessInterrupt triggers the active process stop (Ctrl+C filter).
func RequestProcessInterrupt() bool { return requestProcessInterrupt() }

// CancelSignals returns the process cancel signal set (lane K1 export; a
// function so no mutable slice is exported).
func CancelSignals() []os.Signal {
	return append([]os.Signal(nil), cancelSignals...)
}

// SetProcessStopInstalledForTests force-sets the installed flag and returns
// a restore func (test seam).
func SetProcessStopInstalledForTests(v bool) func() {
	previous := processStopInstalled.Swap(v)
	return func() { processStopInstalled.Store(previous) }
}

// SwapActiveProcessStopForTests replaces the active stop action (test seam)
// and returns a restore func.
func SwapActiveProcessStopForTests(stop func(int)) (restore func()) {
	previous := activeProcessStop.Swap(&processStopAction{stop: stop})
	return func() { activeProcessStop.Store(previous) }
}
