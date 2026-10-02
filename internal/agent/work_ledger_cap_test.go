// Cap admission for workLedger.Start (ASYNC-12, work_ledger_cap.go): the
// per-session limit of maxAsyncJobsPerSession counts only NON-terminal jobs.
// A terminal job still parked in the owner's map -- delivered-on-ack, waiting
// for its "started" ack or an inline Tx2 -- holds no slot, so neither a lost
// ack nor a burst of instant jobs can wedge a session's bash jobs forever;
// genuinely running jobs (acknowledged or not) are still capped, and the
// refusal names the timers holding the slots.
package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// capStart is the one Start call shape this file's loops repeat dozens of
// times: a web-origin bash job with no child session, no timeout and no
// cancel. cli=true keeps the origin honest (Start records it verbatim).
func capStart(t *testing.T, l *workLedger, owner, toolCallID, input string) {
	t.Helper()
	_, _, err := l.Start(owner, toolCallID, input, "bash", "", true, false, nil, nil)
	require.NoError(t, err)
}

// capOwnerJobCount reads the owner's in-memory job count under l.mu. A
// sessionJobs entry survives with an empty map after its last delivery, so
// the nil check is part of the read, not defensive noise.
func capOwnerJobCount(l *workLedger, owner string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.bySession[owner]; s != nil {
		return len(s.jobs)
	}
	return 0
}

// TestWorkLedgerCap_AckedInstantJobsNeverAccumulate pins the ack-gate's
// happy path under the cap: 120 jobs that are all started, finished and
// delivered inside the loop, in both orderings (finish-then-ack and
// ack-then-finish), never accumulate in the owner's map and never block a
// later Start. Every one of the 120 Starts succeeds and every row ends up
// announced with a pending delivery -- an undelivered row never holds a
// slot. Green before and after the ASYNC-12 fix: it pins the property the
// fix must not break, not the fix itself.
func TestWorkLedgerCap_AckedInstantJobsNeverAccumulate(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 120)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = newTestAsyncJobStore(t)
	const owner = "cap-owner"
	const perGroup = 60

	// Group A: finished before the ack -- the "fast job" that races its own
	// "started" tool result (work_ledger.go's finishAcknowledgeLocally).
	for i := range perGroup {
		id := fmt.Sprintf("call-%d", i)
		capStart(t, l, owner, id, "")
		l.finish(jobOf(l, owner, id), jobResult{})
		l.acknowledged(jobOf(l, owner, id))
	}
	// Group B: acked while still running, finished after -- the ordinary
	// long-job shape compressed into the loop.
	for i := range perGroup {
		id := fmt.Sprintf("call-ack-%d", i)
		capStart(t, l, owner, id, "")
		l.acknowledged(jobOf(l, owner, id))
		l.finish(jobOf(l, owner, id), jobResult{})
	}

	require.Zero(t, capOwnerJobCount(l, owner),
		"every delivered job must leave the owner's map")
	require.False(t, l.running(owner))

	rows, err := l.store.ListAsyncJobsForOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, rows, 2*perGroup)
	for _, r := range rows {
		require.EqualValues(t, 1, r.Announced, "row %s must be announced", r.ToolCallID)
		require.Equal(t, "pending", r.Delivery, "row %s must await its notice", r.ToolCallID)
		require.Equal(t, "completed", r.State, "row %s must be terminal", r.ToolCallID)
	}
	require.Len(t, delivered, 2*perGroup, "every job must have been delivered once")
}

// TestWorkLedgerCap_UnacknowledgedTerminalDoesNotHoldSlot pins ASYNC-12's
// core: 60 jobs that reach terminal WITHOUT ever being acked (their "started"
// tool result was lost) stay parked in the owner's map as undelivered rows,
// yet the 51st through 60th Starts all succeed -- a terminal entry holds no
// slot. Red before the fix (the map-length cap refused at 51), green after.
// The trailing ack of all 60 then empties the map and delivers every job.
func TestWorkLedgerCap_UnacknowledgedTerminalDoesNotHoldSlot(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 120)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	l.store = newTestAsyncJobStore(t)
	const owner = "cap-owner2"
	const total = 60

	for i := range total {
		id := fmt.Sprintf("call-%d", i)
		capStart(t, l, owner, id, "")
		l.finish(jobOf(l, owner, id), jobResult{})
	}
	// All 60 Starts above succeeded; pin that the entries really are parked
	// (terminal, unacknowledged), i.e. the cap was not dodged by the jobs
	// disappearing from the map.
	require.Equal(t, total, capOwnerJobCount(l, owner),
		"terminal, unacknowledged jobs stay in the map until delivered")

	for i := range total {
		id := fmt.Sprintf("call-%d", i)
		l.acknowledged(jobOf(l, owner, id))
	}

	require.Zero(t, capOwnerJobCount(l, owner), "acking delivers every parked job")
	require.False(t, l.running(owner))

	seen := make(map[string]bool, total)
	for range total {
		select {
		case c := <-delivered:
			require.Equal(t, owner, c.SessionID)
			seen[c.ToolCallID] = true
		default:
			t.Fatal("a parked job was never delivered after its ack")
		}
	}
	require.Len(t, seen, total, "each job must be delivered exactly once")

	rows, err := l.store.ListAsyncJobsForOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, rows, total)
	for _, r := range rows {
		require.EqualValues(t, 1, r.Announced)
		require.Equal(t, "pending", r.Delivery)
		require.Equal(t, "completed", r.State)
	}
}

// TestWorkLedgerCap_RunningJobsStillCapped pins that the ASYNC-12 fix did not
// weaken the real cap: 50 genuinely running jobs -- half acked, half not, so
// an unacked running job counts too -- still refuse the 51st Start with a
// typed *asyncCapError carrying Running/Limit 50, and the refusal happens
// before store.Claim, so no row exists for the refused tool call. Freeing
// one slot (finish+ack of one job) makes the very same Start succeed.
func TestWorkLedgerCap_RunningJobsStillCapped(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(func(AsyncCompletion) {})
	l.store = newTestAsyncJobStore(t)
	ctx := context.Background()
	const owner = "cap-owner3"
	const running = 50
	const refusedID = "call-51"

	for i := range running / 2 {
		id := fmt.Sprintf("call-%d", i)
		capStart(t, l, owner, id, "sleep 600; echo t")
		l.acknowledged(jobOf(l, owner, id))
	}
	for i := range running / 2 {
		capStart(t, l, owner, fmt.Sprintf("call-b-%d", i), "go test ./...")
	}
	require.True(t, l.running(owner))

	job, existing, err := l.Start(owner, refusedID, "echo hi", "bash", "", true, false, nil, nil)
	require.Error(t, err)
	require.Nil(t, job)
	require.False(t, existing)
	var capErr *asyncCapError
	require.True(t, errors.As(err, &capErr), "refusal must be a typed *asyncCapError")
	require.Equal(t, running, capErr.Running)
	require.Equal(t, maxAsyncJobsPerSession, capErr.Limit)

	// The refusal precedes Claim: the refused tool call has no row at all.
	_, getErr := l.store.Get(ctx, owner, refusedID)
	require.ErrorIs(t, getErr, sql.ErrNoRows)
	rows, err := l.store.ListAsyncJobsForOwner(ctx, owner)
	require.NoError(t, err)
	require.Len(t, rows, running)
	for _, r := range rows {
		require.NotEqual(t, refusedID, r.ToolCallID)
	}

	// Free exactly one slot: the first acked job finishes and is delivered.
	l.finish(jobOf(l, owner, "call-0"), jobResult{})
	l.acknowledged(jobOf(l, owner, "call-0"))
	require.Equal(t, running-1, capOwnerJobCount(l, owner))

	job, existing, err = l.Start(owner, refusedID, "echo hi", "bash", "", true, false, nil, nil)
	require.NoError(t, err, "the same Start must succeed once a slot is free")
	require.NotNil(t, job)
	require.False(t, existing)
	row, err := l.store.Get(ctx, owner, refusedID)
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
}

// TestWorkLedgerCap_RefusalNamesTimers pins the refusal's guidance: with 30
// wait-only timers (sleep/echo no-ops, recognized by shell.IsNoOpCommand)
// and 20 real commands running, the 51st Start is refused with a listing
// that names timers first (at most asyncCapListingMax of them), text that
// tells the model to free slots with job_kill and end its turn instead of
// waiting or escalating, and metadata carrying running/limit/wait_timers.
func TestWorkLedgerCap_RefusalNamesTimers(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(func(AsyncCompletion) {})
	l.store = newTestAsyncJobStore(t)
	ctx := context.Background()
	const owner = "cap-owner4"
	const timers = 30
	const commands = 20
	const refusedID = "call-51"

	timerIDs := make(map[string]bool, timers)
	for i := range timers {
		id := fmt.Sprintf("timer-%02d", i)
		timerIDs[id] = true
		capStart(t, l, owner, id, fmt.Sprintf("sleep 600; echo w%d", i+1))
	}
	for i := range commands {
		capStart(t, l, owner, fmt.Sprintf("test-%02d", i), fmt.Sprintf("go test ./... %d", i))
	}

	_, _, err := l.Start(owner, refusedID, "echo hi", "bash", "", true, false, nil, nil)
	var capErr *asyncCapError
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, maxAsyncJobsPerSession, capErr.Running)
	require.Equal(t, maxAsyncJobsPerSession, capErr.Limit)
	require.Equal(t, timers, capErr.Timers, "all 30 sleep/echo jobs are wait-only timers")
	require.Len(t, capErr.Jobs, asyncCapListingMax,
		"the listing quotes at most asyncCapListingMax jobs")
	require.Contains(t, capErr.Jobs[0].ID, "timer",
		"the listing leads with the timers, which are the cheapest slots to free")
	for _, j := range capErr.Jobs {
		require.True(t, timerIDs[j.ID], "listing must name timers first, got %s", j.ID)
	}

	msg := capErr.Error()
	for _, want := range []string{"50 running", "30 of them", "job_kill", "NO tool call", "ask_question"} {
		require.Contains(t, msg, want)
	}
	require.NotContains(t, msg, "wait for", "the refusal must never tell the model to wait")

	// The listing section's own "- <id> ..." lines agree with the struct.
	// Cut at the listing header first: the guidance bullets above it also
	// start with "- " and must not be counted as quoted jobs.
	_, listing, ok := strings.Cut(msg, "Running (timers first")
	require.True(t, ok, "the refusal must carry a running-jobs listing")
	var listed []string
	for line := range strings.SplitSeq(listing, "\n") {
		if after, ok := strings.CutPrefix(line, "- "); ok {
			listed = append(listed, strings.Fields(after)[0])
		}
	}
	require.Len(t, listed, len(capErr.Jobs))
	require.LessOrEqual(t, len(listed), asyncCapListingMax)
	for _, id := range listed {
		require.True(t, timerIDs[id], "listing line must name a timer, got %s", id)
	}

	var meta asyncCapMetadata
	require.NoError(t, json.Unmarshal([]byte(capErr.Metadata()), &meta))
	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Running)
	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Limit)
	require.Equal(t, timers, meta.AsyncCap.WaitTimers)

	_, getErr := l.store.Get(ctx, owner, refusedID)
	require.ErrorIs(t, getErr, sql.ErrNoRows, "the refusal must not create a row")
}

// TestAsyncTool_CapRefusalMetadata pins asyncTool.Run's cap branch end to
// end: with the owner's 50 jobs running, the tool answers an error response
// whose text is the capErr's guidance and whose metadata is the
// {"async_cap":...} tag -- and never a claim_id, because the refusal
// happened before store.Claim, so there is no row to ack.
func TestAsyncTool_CapRefusalMetadata(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(func(AsyncCompletion) {})
	l.store = newTestAsyncJobStore(t)
	const owner = "cap-owner5"

	for i := range 30 {
		capStart(t, l, owner, fmt.Sprintf("timer-%02d", i), fmt.Sprintf("sleep 600; echo w%d", i+1))
	}
	for i := range 20 {
		capStart(t, l, owner, fmt.Sprintf("test-%02d", i), fmt.Sprintf("go test ./... %d", i))
	}

	var innerRuns bool
	inner := fantasy.NewAgentTool("run_command", "Run a program",
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			innerRuns = true
			return fantasy.NewTextResponse("should never run"), nil
		})
	wrapped := &asyncTool{inner: inner, coordinator: &coordinator{asyncJobs: l}, name: "run_command"}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, owner)
	ctx = WithCallOrigin(ctx, message.OriginCLI)

	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call-51", Name: "run_command", Input: `{}`})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.False(t, innerRuns, "the inner tool must never run past a cap refusal")
	require.Contains(t, resp.Content, "50 running")
	require.Contains(t, resp.Content, "job_kill")
	require.NotContains(t, resp.Metadata, "claim_id",
		"a refused job has no claim, so no result may carry one")

	var meta asyncCapMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Running)
	require.Equal(t, maxAsyncJobsPerSession, meta.AsyncCap.Limit)
	require.Equal(t, 30, meta.AsyncCap.WaitTimers)
}
