// App-owned batch runtime owner. Construction has no side
// effects: no goroutine, no registration, no Runner. The install wires the
// Runner and the gate into app_lifecycle.go.

package app

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchRunner is the narrow surface the host lifecycle needs from the
// Runner. *jobbatch.Runner satisfies it (runner_commit.go:26-27,
// runner_requests.go:350).
type batchRunner interface {
	// Faulted closes exactly once when a commit fault latches.
	Faulted() <-chan struct{}
	// FaultErr returns the latched fault error, nil before a fault.
	FaultErr() error
	// Close drains the Runner to Final, bounded by ctx. It returns a
	// *jobbatch.CloseTimeoutError (errors.Is jobbatch.ErrCloseTimeout) when
	// nodes are still nonterminal after the bound, the fault error on a
	// faulted Runner, or a *jobbatch.DisposeError for dispose failures.
	Close(ctx context.Context) error
}

// The real Runner satisfies the narrow interface; asserted at compile time
// so an upstream API drift fails the build here, not at R11F install time.
var _ batchRunner = (*jobbatch.Runner)(nil)

// BatchRuntimeConfig configures one BatchRuntime. All fields are optional;
// zero values select production defaults.
type BatchRuntimeConfig struct {
	// CommitHook is the R11B projection adapter (session.NewBatchCommitHook)
	// handed to the Runner at R11F construction time. The runtime only holds
	// it; it never invokes it.
	CommitHook func(context.Context, jobbatch.Transition) error

	// ContainmentWait bounds the fault handler's wait before process exit.
	// Zero selects DefaultBatchFaultContainment (10 s, decision D4).
	ContainmentWait time.Duration

	// WaitFunc is injectable for tests: given a duration it returns the
	// channel that fires when the containment bound expires. Nil selects
	// time.After.
	WaitFunc func(d time.Duration) <-chan time.Time

	// ExitFunc is the injectable process exit. Nil selects os.Exit.
	ExitFunc func(code int)

	// BeforeExit runs after the fault is fenced and containment has elapsed,
	// immediately before the process exit. Hosts may publish a final result.
	BeforeExit func(error)

	// RetainHostIdentity forgets the in-process host-lock handle without
	// releasing the OS lock (the CloseKeepLock convention of the forced
	// shutdown path in app_lifecycle.go). Nil means no host identity.
	RetainHostIdentity func()

	// ReleaseHostIdentity releases the host identity (HostStore.Close
	// convention: OS lock release plus best-effort row cleanup). Nil means
	// no host identity.
	ReleaseHostIdentity func(context.Context) error

	// ReleaseWriter releases the database writer (db.ReleaseConn/Release
	// convention). Nil means no writer.
	ReleaseWriter func(context.Context) error

	// R11F (P1-6), minimal additive hunk: RunnerFactory constructs the
	// Runner LAZILY at Start, so a factory-built runtime has NO side
	// effects (no engine goroutines) until Start runs. When set, Start
	// calls it exactly once and attaches the result with the first-wins
	// SetRunner policy; a construction error is logged and the runtime
	// stays runnerless — the ingress then fails closed on Invoke. Mutually
	// exclusive with constructing the Runner before NewBatchRuntime (an
	// eager SetRunner still wins).
	RunnerFactory func() (batchRunner, error)

	// TimeoutCheckin, when non-nil, is the wake_only check-in timer service
	// owned by the batch runtime: Start arms it (inside Start's once block,
	// after the fault watcher is up) and Stop disarms it. Nil disables the
	// service. Declared as an interface so app never imports session here.
	TimeoutCheckin interface {
		Start()
		Stop()
	}
}

// DefaultBatchFaultContainment is the bounded containment wait the fault
// handler runs before process exit (decision D4).
const DefaultBatchFaultContainment = 10 * time.Second

// BatchRuntime is the dormant app-owned owner of the batch Runner, the
// commit hook, the fault watcher and the drain gate.
//
// It owns the lifecycle responsibilities of the host identity release
// (HostStore.CloseKeepLock/Close through the wired release seams) and the
// DB writer release (db.ReleaseConn/Release on agent-idle) via the drain
// gate DrainAndRelease. Construction has NO side effects: no goroutine, no
// signal handler, no Runner registration. Start arms the fault watcher;
// Stop disarms it.
type BatchRuntime struct {
	cfg BatchRuntimeConfig

	mu        sync.Mutex
	runner    batchRunner
	runnerSet chan struct{} // Buffered 1; wakes the dormant watcher.
	stop      chan struct{}
	watch     sync.Once

	// exitOnce guards exactly-once process exit (code 70) on a fault.
	exitOnce sync.Once

	// fenced is set BEFORE the containment wait on a fault (and observed by
	// the drain gate), so a shutdown that starts after a fault can never
	// release the host lock or the writer. It is never unset: a faulted
	// process only exits.
	fenced atomic.Bool

	// testParked is a per-instance TEST-ONLY seam: when non-nil, the dormant
	// watchFaults select reports each park on it. It lets a test establish
	// THIS runtime's dormant boundary (replacing a process-wide
	// goroutine-stack check that any parallel test's watcher could satisfy).
	// Production never sets it.
	testParked chan struct{}
}

// NewBatchRuntime builds a dormant runtime owner. It performs no I/O and
// starts no goroutines.
func NewBatchRuntime(cfg BatchRuntimeConfig) *BatchRuntime {
	return &BatchRuntime{cfg: cfg, stop: make(chan struct{}), runnerSet: make(chan struct{}, 1)}
}

// SetRunner attaches the live Runner. It is safe to call before or after
// Start: Start may be called while dormant, and SetRunner wakes the watcher
// via runnerSet, so the two are order-independent. Policy is FIRST WINS: the
// first SetRunner attaches; a second SetRunner with a DIFFERENT runner is
// refused with a warning and no state change; re-setting the same pointer is
// a no-op. R11F calls this at Runner construction.
func (rt *BatchRuntime) SetRunner(r batchRunner) {
	rt.mu.Lock()
	if rt.runner != nil {
		prev := rt.runner
		rt.mu.Unlock()
		if prev != r {
			slog.Warn("Batch runtime: refusing second SetRunner with a different runner (first wins)")
		}
		return
	}
	rt.runner = r
	rt.mu.Unlock()
	select {
	case rt.runnerSet <- struct{}{}:
	default:
	}
}

// Runner returns the attached Runner, nil while dormant.
func (rt *BatchRuntime) Runner() batchRunner {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.runner
}

// CommitHook returns the held commit hook (for R11F Runner construction).
func (rt *BatchRuntime) CommitHook() func(context.Context, jobbatch.Transition) error {
	return rt.cfg.CommitHook
}

// Fenced reports whether a fault has been observed: either the watcher set
// the fence, or the attached Runner's Faulted channel is already closed —
// the latter independently of the watcher goroutine and of Start having
// been called. While fenced, the drain gate refuses every release: the host
// lock and the writer are retained until the process exits with code 70.
func (rt *BatchRuntime) Fenced() bool {
	if rt.fenced.Load() {
		return true
	}
	r := rt.Runner()
	if r == nil {
		return false
	}
	select {
	case <-r.Faulted():
		return true
	default:
		return false
	}
}

// Start arms the fault watcher goroutine and — R11F P1-6 — constructs the
// Runner through cfg.RunnerFactory when one is wired, so no factory-built
// runtime starts engine goroutines before this point. It is idempotent; the
// watcher exits on Stop or on handling a fault. Dormant (no Runner yet) it
// waits.
func (rt *BatchRuntime) Start() {
	rt.watch.Do(func() {
		if rt.cfg.RunnerFactory != nil {
			r, err := rt.cfg.RunnerFactory()
			if err != nil {
				// Fail closed: no Runner attached, the ingress refuses
				// every Invoke; the watcher still runs for Stop/Drain.
				slog.Error("batch runtime: runner construction failed; runtime stays runnerless", "err", err)
			} else if r != nil {
				rt.SetRunner(r)
			}
		}
		go rt.watchFaults()
		if rt.cfg.TimeoutCheckin != nil {
			rt.cfg.TimeoutCheckin.Start()
		}
	})
}

// Stop disarms the fault watcher. It does NOT drain the Runner and does NOT
// release anything — that is DrainAndRelease's job.
func (rt *BatchRuntime) Stop() {
	rt.mu.Lock()
	select {
	case <-rt.stop:
	default:
		close(rt.stop)
	}
	rt.mu.Unlock()
	if rt.cfg.TimeoutCheckin != nil {
		rt.cfg.TimeoutCheckin.Stop()
	}
}

func (rt *BatchRuntime) watchFaults() {
	for {
		rt.mu.Lock()
		runner := rt.runner
		stop := rt.stop
		rt.mu.Unlock()
		if runner != nil {
			select {
			case <-runner.Faulted():
				rt.handleFault(runner.FaultErr())
				return
			case <-stop:
				return
			}
		}
		// Dormant: wait for a Runner to be set (or shutdown); then re-read.
		if rt.testParked != nil {
			select {
			case rt.testParked <- struct{}{}:
			default:
			}
		}
		select {
		case <-rt.runnerSet:
		case <-stop:
			return
		}
	}
}
