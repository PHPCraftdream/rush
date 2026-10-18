package jobbatch

// NewTestBatch builds a detached Batch handle for cross-package tests (e.g.
// agent ingress runner fakes that must return (root, *Batch) from
// SubmitBound). The handle reports the given id and a fresh, never-closing
// Done channel; it is wired to no Runner and never settles on its own.
//
// There is deliberately no production constructor equivalent: a real Batch
// only ever comes from Runner.Submit, and tests cannot reach the unexported
// fields from outside this package.
func NewTestBatch(id ID) *Batch {
	return &Batch{id: id, done: make(chan struct{})}
}
