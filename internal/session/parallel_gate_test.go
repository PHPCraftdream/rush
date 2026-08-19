package session

import (
	"runtime"
	"sync"
	"testing"
)

// parallelSlots bounds how many of this package's t.Parallel() subtests
// actually execute their body at the same time, independent of whatever
// -parallel value (default GOMAXPROCS) the test binary was invoked with.
//
// Found by measurement (task #592): this package has ~80 t.Parallel()
// subtests split across two Go test packages compiled into the same test
// binary (whitebox `package session`, this file, and blackbox `package
// session_test`, see run_queue_round2_test.go's identically-named,
// independently-scoped copy of this same helper — the two packages cannot
// share unexported identifiers, so each needs its own semaphore; the net
// effect still meaningfully bounds total concurrency since both apply at
// once). Most of these subtests drive a real RunQueuePump or SessionLock
// against real (SetMaxOpenConns(1), production-faithful) SQLite DBs or real
// OS file locks, with real, accelerated-but-genuine wall-clock TTLs,
// backoffs, and heartbeat intervals. A single `go test -race` invocation
// already runs all ~80 concurrently (bounded only by GOMAXPROCS); eight
// such invocations at once (a stress mode used to shake out real
// concurrency defects in this package, not a CI shape) multiply that to
// several hundred goroutines contending for CPU and disk across only a
// handful of real cores. Under that contention, some of those real
// wall-clock budgets — themselves already widened repeatedly in prior
// sessions to absorb ordinary CI slowness — are missed not because the
// production code is wrong, but because the test process cannot schedule a
// goroutine or complete a disk write in time.
//
// This does NOT touch any TTL, timeout, or require.Eventually window — it
// reduces how many of this package's own subtests self-inflict contention
// on each other and on the disk, which is the actual variable this task's
// own hypothesis section asked to be tested first (and which the measured
// before/after tallies confirmed: same failures, far less often, with zero
// source changes to any duration constant).
//
// Sized as a quarter of GOMAXPROCS (floor 2, so a single test process can
// always make forward progress even on a 1-2 core CI runner) rather than a
// fixed number, so it scales with whatever machine runs the suite instead
// of encoding this machine's 16 logical CPUs as a magic constant.
var (
	parallelSlotsOnce sync.Once
	parallelSlotsCh   chan struct{}
)

func parallelSlots() chan struct{} {
	parallelSlotsOnce.Do(func() {
		n := runtime.GOMAXPROCS(0) / 4
		if n < 2 {
			n = 2
		}
		parallelSlotsCh = make(chan struct{}, n)
	})
	return parallelSlotsCh
}

// limitParallel bounds this test's actual execution (not its eligibility to
// be scheduled) to parallelSlots() concurrent bodies package-wide. Call
// immediately after t.Parallel() in every parallel test in this package --
// see parallelSlots' doc for why.
func limitParallel(t *testing.T) {
	t.Helper()
	sem := parallelSlots()
	sem <- struct{}{}
	t.Cleanup(func() { <-sem })
}
