// Dormant app-owned batch runtime owner (R11E). Construction has no side
// effects: no goroutine, no registration, no Runner. R11F will install the
// Runner and wire the gate into app_lifecycle.go.

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

	// RetainHostIdentity forgets the in-process host-lock handle without
	// releasing the OS lock (the CloseKeepLock convention of the forced
	// shutdown path in app_lifecycle.go). Nil means no host identity.
	RetainHostIdentity func()

	// ReleaseHostIdentity releases the host identity (AsyncJobStore.Close
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
}

// DefaultBatchFaultContainment is the bounded containment wait the fault
// handler runs before process exit (decision D4).
const DefaultBatchFaultContainment = 10 * time.Second

// BatchRuntime is the dormant app-owned owner of the batch Runner, the
// commit hook, the fault watcher and the drain gate.
//
// After R11F it will own the lifecycle responsibilities that today live in
// app_lifecycle.go's releaseResources: the host identity release
// (asyncJobStore.CloseKeepLock/Close) and the DB writer release
// (db.ReleaseConn/Release on agent-idle), replacing them with the drain
// gate DrainAndRelease. The App fields it will own: asyncJobStore,
// dbConns/dbReleasesNeeded/dataDir release calls, and the fault-exit
// responsibility for the CLI/server/SDK host kinds.
//
// Construction has NO side effects: no goroutine, no signal handler, no
// Runner registration. Start arms the fault watcher; Stop disarms it.
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
	})
}

// Stop disarms the fault watcher. It does NOT drain the Runner and does NOT
// release anything — that is DrainAndRelease's job.
func (rt *BatchRuntime) Stop() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	select {
	case <-rt.stop:
	default:
		close(rt.stop)
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
		select {
		case <-rt.runnerSet:
		case <-stop:
			return
		}
	}
}
