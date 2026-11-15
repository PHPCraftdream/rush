package jobbatch

import "sync"

// testBatchOnce tracks the settlement gate of every Batch created by
// NewTestBatch, so Settle closes Done exactly once (a real Batch closes its
// done channel exactly once when the run settles — runner.go publishes
// SettledRoots with close(b.done)).
var testBatchOnce sync.Map // *Batch -> *sync.Once

// NewTestBatch builds a detached Batch handle for cross-package tests (e.g.
// agent ingress runner fakes that must return (root, *Batch) from
// SubmitBound). The handle reports the given id and mimics the real Batch's
// Done semantics: the channel is open while the batch runs and closes exactly
// once when Settle is called. It is wired to no Runner and never settles on
// its own.
//
// There is deliberately no production constructor equivalent: a real Batch
// only ever comes from Runner.Submit, and tests cannot reach the unexported
// fields from outside this package.
func NewTestBatch(id ID) *Batch {
	b := &Batch{id: id, done: make(chan struct{})}
	testBatchOnce.Store(b, &sync.Once{})
	return b
}

// Settle closes the test batch's Done channel exactly once (sync.Once),
// mimicking a real Batch's close-on-completion: every watcher — including the
// agent root-lease adoption watcher — observes settlement and releases.
// Settle is IDEMPOTENT: the map entry is retained for the process lifetime
// (the Once makes every call after the first a no-op), so a second Settle on
// the same Batch is harmless. Test-only API. It panics on a Batch not
// produced by NewTestBatch.
func (b *Batch) Settle() {
	v, ok := testBatchOnce.Load(b)
	if !ok {
		panic("jobbatch: Settle called on a non-test Batch")
	}
	once := v.(*sync.Once)
	once.Do(func() {
		close(b.done)
	})
}
