package session

// Regression coverage for a defect the coordinator caught in review of task
// #607/P0's own fix: verdictOnCtxDone's first version called
// l.recordUnattributed(ctx.Err()) UNCONDITIONALLY, as its first line --
// before checking whether this call had executed (or observed) anything at
// all. That set l.anyExecuted = true and added an entry to l.failed even
// when NOTHING had run, so a ctx that died before this call ever touched a
// row started reporting (DrainFailed, ctx.Err()) instead of the
// pre-existing, correct (DrainNoWork, ctx.Err()) -- overstating failure in
// the mirror image of the five prior task reviews' defects (which all
// overstated SUCCESS). See run_queue_drain_session.go's verdictOnCtxDone
// doc for the full explanation and why this distinction matters even though
// app_run.go currently exits non-zero for both results today: DrainNoWork
// and DrainFailed are different claims about what this call actually did,
// and DrainResult exists specifically so callers do not have to guess which
// one is true from a single boolean.
//
// This file pins the fix at the rowLedger level (package-internal, white-box
// -- verdictOnCtxDone and anyExecuted are both unexported) rather than
// driving a full DrainSessionNow call, because the defect and its fix live
// entirely inside rowLedger's own bookkeeping and are fastest and most
// precisely pinned there directly. See p607_ctx_dies_after_success_test.go
// and p607_observed_admission_ctx_dies_test.go (package session_test) for
// the integration-level coverage of DrainSessionNow's two call sites
// themselves, which this file does not duplicate.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerdictOnCtxDone_NothingExecuted_ReportsNoWork pins the exact scenario
// the coordinator's review reproduced: a freshly-constructed ledger (nothing
// has executed or been observed yet) whose ctx is already dead. This MUST
// report (DrainNoWork, ctx.Err()) -- the pre-existing "nothing happened in
// this call" contract every DrainSessionNow caller already depends on --
// NOT (DrainFailed, ctx.Err()), which would falsely claim a row ran and
// failed.
//
// Revert-check (see task report): reverting JUST the `if l.anyExecuted`
// guard in verdictOnCtxDone back to an unconditional
// `l.recordUnattributed(ctx.Err())` reproduces (DrainFailed, ctx.Err()) here
// and fails this test, while leaving p607_ctx_dies_after_success_test.go and
// p607_observed_admission_ctx_dies_test.go (which both start from a ledger
// that already HAS something executed) both still green -- proving this
// test covers the anyExecuted=false case specifically, not the two
// call-site tests' already-covered anyExecuted=true case.
func TestVerdictOnCtxDone_NothingExecuted_ReportsNoWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := newRowLedger()
	result, err := l.verdictOnCtxDone(ctx)

	require.Equal(t, DrainNoWork, result, "nothing executed before ctx died -- must report DrainNoWork, not DrainFailed")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "the error must be ctx.Err() itself, not a fabricated ledger failure")

	// The ledger itself must remain untouched -- recordUnattributed must
	// never have run for this case (asserted directly, not just inferred
	// from the returned DrainResult, to pin the GUARD itself rather than
	// merely its externally-visible effect).
	require.False(t, l.anyExecuted, "verdictOnCtxDone must not mark anyExecuted when nothing had executed before it was called")
	require.Empty(t, l.failed, "verdictOnCtxDone must not add a failure entry when nothing had executed before it was called")
}

// TestVerdictOnCtxDone_SomethingExecuted_ReportsFailed is the sibling
// control: once ANYTHING has genuinely executed (a real recordSuccess, in
// this minimal reproduction), a subsequent verdictOnCtxDone call for a dead
// ctx MUST still report DrainFailed -- the anyExecuted guard must gate
// recordUnattributed, not skip it whenever ctx is merely dead. This is the
// task #607 scenario itself (one row succeeded, ctx then died with more
// work outstanding), now pinned directly at the ledger level as well as
// through the two DrainSessionNow integration tests.
func TestVerdictOnCtxDone_SomethingExecuted_ReportsFailed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := newRowLedger()
	l.recordSuccess("row-A")
	result, err := l.verdictOnCtxDone(ctx)

	require.Equal(t, DrainFailed, result, "row A succeeded but ctx died before further work could be accounted for -- must NOT report DrainComplete or DrainNoWork")
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "the representative error must trace back to ctx's own cancellation")
}
