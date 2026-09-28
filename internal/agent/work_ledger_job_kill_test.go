// Unit tests for task #1023 (wakes stage 2): job_kill marking a job
// stop-requested before the caller kills it (§2.2), and run_command's own
// control path (§3, no BackgroundShellManager entry to operate on).
package agent

import (
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// fakeLiveOutputBuffer is a minimal tools.LiveOutputBuffer for ledger tests
// that never need a real run_command process.
type fakeLiveOutputBuffer struct {
	mu   sync.Mutex
	data string
}

func (f *fakeLiveOutputBuffer) Read(cursor int64) (string, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := int64(len(f.data))
	if cursor < 0 || cursor > total {
		cursor = 0
	}
	return f.data[cursor:], total
}

func (f *fakeLiveOutputBuffer) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeLiveOutputBuffer) write(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data += s
}

var _ tools.LiveOutputBuffer = (*fakeLiveOutputBuffer)(nil)

// TestWorkLedger_MarkJobStopped_BashProducesDistinctCancelledOutcome pins
// §2.2: a job marked stopped BEFORE finish() reaches a distinct phaseCancelled
// outcome (Stopped=true), not phaseFailed/phaseCompleted from whatever the
// killed process's own exit looked like -- and FormatAsyncCompletion renders
// the contract's exact "stopped (job_kill)" wording, not a double-wrapped
// "finished"/"failed" text around it.
//
// Revert-check performed: removed finish()'s `if job.stopRequested` branch
// (state always derived from result.isError) -- this test FAILED (state was
// phaseFailed, Stopped was false, and the rendered text was the generic
// "failed" wording instead of "was stopped (job_kill)"). Restored the fix;
// re-ran, passed.
func TestWorkLedger_MarkJobStopped_BashProducesDistinctCancelledOutcome(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "call")

	l.MarkJobStopped("owner", "call")
	// The executor's own finish() call still happens exactly once, exactly
	// as if the killed process had reported a plain error -- MarkJobStopped
	// only changes what THIS call records, not whether it is called.
	l.finish("owner", "call", jobResult{content: "killed: exit status 1", isError: true})

	got := drainCompletions(delivered)
	require.Len(t, got, 1)
	require.True(t, got[0].Stopped, "stopped-on-request outcome must be marked Stopped")
	require.False(t, got[0].IsError, "a job_kill stop is not a failure")
	require.False(t, got[0].TimedOut)
	require.Equal(t, "killed: exit status 1", got[0].Content, "raw partial content, unformatted -- FormatAsyncCompletion formats it")

	text := FormatAsyncCompletion(got[0])
	require.Contains(t, text, "was stopped (job_kill)")
	require.Contains(t, text, "killed: exit status 1")
	require.NotContains(t, text, "finished", "must not double-wrap with the generic finished/failed wording")
	require.NotContains(t, text, "failed")

	l.mu.Lock()
	job := l.bySession["owner"]
	l.mu.Unlock()
	require.Nil(t, job.jobs["call"], "delivered job must be dropped from the ledger, not retained (task #1023 item 5)")
}

// TestWorkLedger_MarkJobStopped_RunCommandUsesLiveBufferForPartialOutput
// pins §3: a run_command job has no result.content worth trusting (its own
// ctx-cancellation path returns a bare "context canceled" error, not the
// process's actual output) -- the stopped notice must instead quote the live
// output buffer's snapshot.
func TestWorkLedger_MarkJobStopped_RunCommandUsesLiveBufferForPartialOutput(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	_, _, err := l.Start("owner", "call", "", "run_command", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "call")

	buf := &fakeLiveOutputBuffer{}
	buf.write("line 1\nline 2\n")
	l.setRunCommandBuffer("owner", "call", buf)

	require.NoError(t, l.StopRunCommandJob("owner", "call"))
	// Simulates run_command.go's own ctx-cancellation branch, which discards
	// its own partial buffer content and returns a bare error (see
	// run_command.go's `case ctx.Err() == context.Canceled`).
	l.finish("owner", "call", jobResult{content: "context canceled", isError: true})

	got := drainCompletions(delivered)
	require.Len(t, got, 1)
	require.True(t, got[0].Stopped)
	require.Equal(t, "line 1\nline 2\n", got[0].Content, "must use the live buffer, not the executor's own (discarded) partial content")
}

// TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome extends ASYNC-03's
// race coverage with MarkJobStopped racing finish -- a job_kill call landing
// at (almost) the same moment the job finishes on its own. Whichever wins,
// transitionToTerminal's CAS must still yield exactly one delivered outcome.
func TestWorkLedger_JobKillRaceAgainstFinishYieldsOneOutcome(t *testing.T) {
	t.Parallel()
	for i := 0; i < 30; i++ {
		delivered := make(chan AsyncCompletion, 8)
		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
		_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
		require.NoError(t, err)
		l.acknowledged("owner", "call")

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			l.MarkJobStopped("owner", "call")
		}()
		go func() {
			defer wg.Done()
			<-start
			l.finish("owner", "call", jobResult{content: "ok"})
		}()
		close(start)
		wg.Wait()

		got := drainCompletions(delivered)
		require.Len(t, got, 1, "exactly one terminal outcome must be delivered per race (iteration %d)", i)
	}
}

// TestWorkLedger_StopRunCommandJob_CancelsAndIsIdempotent pins §1.5's
// idempotency rule applied to run_command's own stop path: a second call on
// an already-stopping job is refused with the same "not found" shape, not a
// disguised second success, and does not call cancel twice.
func TestWorkLedger_StopRunCommandJob_CancelsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	var cancelCalls int
	_, _, err := l.Start("owner", "call", "", "run_command", "", false, false, nil, func() { cancelCalls++ })
	require.NoError(t, err)

	require.NoError(t, l.StopRunCommandJob("owner", "call"))
	require.Equal(t, 1, cancelCalls)

	err = l.StopRunCommandJob("owner", "call")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
	require.Equal(t, 1, cancelCalls, "a repeat stop must not cancel a second time")
}

// TestWorkLedger_StopRunCommandJob_RejectsNonRunCommandJob pins the kind
// boundary: StopRunCommandJob must refuse a bash/delegation job_id, the same
// way ResolveJobShellID refuses a run_command job_id going the other way.
func TestWorkLedger_StopRunCommandJob_RejectsNonRunCommandJob(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)

	err = l.StopRunCommandJob("owner", "call")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestWorkLedger_RunCommandOutput_CursorSemantics pins §2.1's cursor rules
// for run_command (no BackgroundShellManager, so the live buffer is the only
// source): a valid cursor returns only new bytes, negative/out-of-range is
// treated as 0, and done reflects the job's own terminal state.
func TestWorkLedger_RunCommandOutput_CursorSemantics(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("owner", "call", "", "run_command", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "call")

	buf := &fakeLiveOutputBuffer{}
	buf.write("hello ")
	l.setRunCommandBuffer("owner", "call", buf)

	data, done, next, err := l.RunCommandOutput("owner", "call", 0)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, "hello ", data)
	require.EqualValues(t, 6, next)

	buf.write("world")
	data, done, next, err = l.RunCommandOutput("owner", "call", next)
	require.NoError(t, err)
	require.False(t, done)
	require.Equal(t, "world", data, "must return only bytes written since the previous cursor")
	require.EqualValues(t, 11, next)

	// Stale/negative cursor: treated as 0, not an error.
	data, _, _, err = l.RunCommandOutput("owner", "call", -5)
	require.NoError(t, err)
	require.Equal(t, "hello world", data)
	data, _, _, err = l.RunCommandOutput("owner", "call", 999)
	require.NoError(t, err)
	require.Equal(t, "hello world", data)

	l.finish("owner", "call", jobResult{content: "hello world"})
	// Terminal but not yet delivered would show done=true; here finish's own
	// deliverLocked already removed the job (task #1023 item 5's retention
	// bound), so a subsequent call correctly reports not-found.
	_, _, _, err = l.RunCommandOutput("owner", "call", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestWorkLedger_RunCommandOutput_StillStartingRaceIsNotAnError covers the
// narrow window before run_command.go's process has registered its output
// sink (setRunCommandBuffer race, §3): job_output must not error, just
// report no output yet.
func TestWorkLedger_RunCommandOutput_StillStartingRaceIsNotAnError(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("owner", "call", "", "run_command", "", false, false, nil, func() {})
	require.NoError(t, err)

	data, done, _, err := l.RunCommandOutput("owner", "call", 0)
	require.NoError(t, err)
	require.False(t, done)
	require.Empty(t, data)
}

// TestWorkLedger_ResolveJobShellID_TypedErrors pins the typed-error routing
// job_kill/job_output depend on (task #1023): errors.As must find
// *tools.RunCommandJobError / *tools.DelegationJobError, and the delegation
// error must carry the child session id for the refusal text.
func TestWorkLedger_ResolveJobShellID_TypedErrors(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("owner", "rc-call", "", "run_command", "", false, false, nil, nil)
	require.NoError(t, err)
	_, err = l.ResolveJobShellID("owner", "rc-call")
	var rcErr *tools.RunCommandJobError
	require.ErrorAs(t, err, &rcErr)
	require.Equal(t, "rc-call", rcErr.JobID)

	_, _, err = l.Start("owner", "agent-call", "", AgentToolName, "child-session", false, false, nil, nil)
	require.NoError(t, err)
	_, err = l.ResolveJobShellID("owner", "agent-call")
	var delErr *tools.DelegationJobError
	require.ErrorAs(t, err, &delErr)
	require.Equal(t, "agent-call", delErr.JobID)
	require.Equal(t, "child-session", delErr.ChildSessionID)
	require.Contains(t, delErr.Error(), "child session child-session")
	require.NotContains(t, delErr.Error(), "stop_agent", "stop_agent does not exist yet (stage 3); must not tell the model to call it")
}
