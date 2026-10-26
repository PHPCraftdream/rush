// Package jobbatch is a standalone engine and runtime for batches of tasks.
//
// Every invocation is an ordinary batch; a one-task call uses OneItem. The
// design is docs/plans/2026-10-07-job-batches-design.md (revision 4); this
// package depends only on the standard library.
//
// # Frozen contract (phase A0)
//
// types.go, events.go, executor.go, runtime_types.go and errors.go hold the
// shared data declarations and a few pure helpers every slice must answer
// identically (State.Terminal/Active/Reportable, ValidCode, ReservedCode,
// ValidRef). Only the integration owner changes them. A slice that needs a
// missing field reports it instead of editing a contract file.
//
// The function and method signatures below are frozen by the design (section
// 2) and declared exactly once, with a real body, by the owning slice:
//
//	Core     (engine.go, tree.go, tree_apply.go, admission.go, view.go, notice.go)
//	  NewEngine(Limits) (*Engine, error)
//	  (*Engine) Apply(Event) (Transition, error)
//	  (*Engine) View(ID) (NodeView, bool)
//	  (*Engine) Snapshot(ID) (BatchView, bool)
//
//	Runtime  (runner.go, runner_dispatch.go, runner_control.go,
//	          runner_observe.go, runner_requests.go)
//	  OneItem(TaskSpec) Spec
//	  NewRunner(context.Context, RunnerConfig) (*Runner, error)
//	  (*Runner) Submit, Add, Stop, CapacityAvailable, View, Snapshot,
//	            Events, Output, Inject, Release, Close
//	  (*Batch) ID, Done, Wait
//
// Slice-owned package-level test helpers carry the slice prefix: coreTest*
// (testdata/core/) and runtimeTest* (testdata/runtime/). The laboratory
// (lab/) and cmd/batchlab consume this package; it never imports them.
package jobbatch
