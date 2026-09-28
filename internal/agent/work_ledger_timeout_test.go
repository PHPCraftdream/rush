package agent

// §5.7: explicit per-call timeouts -- the single timer service, the
// timeout event handler, CAS interaction with finish/cancel, and
// parseTimeoutParam's validation table.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// TestTimeoutService_SingleGoroutineForManyJobs: arming N jobs must not
// spawn N goroutines -- exactly one timer goroutine services the whole
// process regardless of how many deadlines are armed.
// Revert-check performed: temporarily changed arm() to spawn
// `go time.AfterFunc(...)`-per-job (see inline note) -- goroutine count grew
// with N. Restored the heap-based single-goroutine arm; re-ran, passed.
func TestTimeoutService_SingleGoroutineForManyJobs(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()
	l := newWorkLedger(nil)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	const n = 50
	for i := 0; i < n; i++ {
		_, _, err := l.Start("owner", fmt.Sprintf("call-%d", i), "", "bash", "", false, false, &TimeoutSpec{
			Deadline: time.Now().Add(time.Hour), Kind: timeoutTerminateAndWake, Seconds: 3600,
		}, func() {})
		require.NoError(t, err)
	}

	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	require.LessOrEqual(t, after-before, 3,
		"arming %d jobs must not spawn %d goroutines -- expected at most the ONE timer goroutine (plus scheduler slack), got a delta of %d", n, n, after-before)
}

// TestTimeoutService_FiresNearestFirst: two deadlines armed in FAR-then-NEAR
// order must still fire NEAR first -- the timer must recompute its sleep to
// the nearest deadline whenever a new job is armed, not just sleep for
// whichever job it saw first.
func TestTimeoutService_FiresNearestFirst(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	l := newWorkLedger(func(c AsyncCompletion) {
		mu.Lock()
		order = append(order, c.ToolCallID)
		mu.Unlock()
	})
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	now := time.Now()
	_, _, err := l.Start("owner", "far", "", "bash", "", false, false, &TimeoutSpec{
		Deadline: now.Add(200 * time.Millisecond), Kind: timeoutTerminateAndWake, Seconds: 1,
	}, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "far")

	_, _, err = l.Start("owner", "near", "", "bash", "", false, false, &TimeoutSpec{
		Deadline: now.Add(50 * time.Millisecond), Kind: timeoutTerminateAndWake, Seconds: 1,
	}, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "near")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"near", "far"}, order)
}

// TestWorkLedger_TerminateAndWakeTransitionsToTimedOut: a terminate_and_wake
// job past its deadline transitions to phaseTimedOut, invokes job.cancel,
// and is delivered with TimedOut=true.
func TestWorkLedger_TerminateAndWakeTransitionsToTimedOut(t *testing.T) {
	t.Parallel()
	var delivered []AsyncCompletion
	l := newWorkLedger(func(c AsyncCompletion) { delivered = append(delivered, c) })
	var cancelled bool
	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, &TimeoutSpec{
		Deadline: time.Now().Add(-time.Hour), Kind: timeoutTerminateAndWake, Seconds: 30,
	}, func() { cancelled = true })
	require.NoError(t, err)
	l.acknowledged("owner", "call")

	l.mu.Lock()
	job := l.bySession["owner"].jobs["call"]
	l.mu.Unlock()
	require.NotNil(t, job)

	l.handleTimeout(job)

	require.True(t, cancelled, "job.cancel must be invoked")
	require.Equal(t, phaseTimedOut, job.state)
	require.Len(t, delivered, 1)
	require.True(t, delivered[0].TimedOut)
	require.Equal(t, 30, delivered[0].TimeoutSeconds)
}

// TestNotifyAsyncCompletion_TimedOutUsesContractTextAndNoticeKind:
// FormatAsyncCompletion's timeout wording matches the contract verbatim, and
// notifyAsyncCompletion's wake carries noticeKind="timeout_terminated" for a
// TimedOut completion.
// Revert-check performed: reverted FormatAsyncCompletion to the
// unconditional "finished/failed" text -- the text assertion below FAILED
// (got the generic "finished" wording instead of "timed out after...").
// Restored the TimedOut branch; re-ran, passed.
func TestNotifyAsyncCompletion_TimedOutUsesContractTextAndNoticeKind(t *testing.T) {
	t.Parallel()
	text := FormatAsyncCompletion(AsyncCompletion{
		SessionID: "s", ToolCallID: "call-1", ToolName: "bash",
		Content: "partial output", TimedOut: true, TimeoutSeconds: 900,
	})
	require.Equal(t, "Async job call-1 (bash) timed out after 900s and was stopped. Partial output:\n\npartial output", text)

	received := make(chan SessionAgentCall, 1)
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			received <- call
			return agentResultWithText("ok"), nil
		},
	}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: agent})
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.coord = coord

	coord.notifyAsyncCompletion(AsyncCompletion{
		SessionID: "child-1", ToolCallID: "call-1", ToolName: "bash",
		Content: "partial output", TimedOut: true, TimeoutSeconds: 900,
	})

	select {
	case call := <-received:
		require.Equal(t, "timeout_terminated", call.NoticeKind)
		require.Contains(t, call.Prompt, "timed out after 900s")
	case <-time.After(2 * time.Second):
		t.Fatal("notifyAsyncCompletion's wake never reached agent.Run")
	}
}

// TestWorkLedger_WakeOnlyUsesTimeoutWakeOnlyNoticeKind: handleTimeout's
// wake_only branch calls wakeSession directly with
// noticeKind="timeout_wake_only", and does NOT transition the job to
// terminal (it stays phaseRunning).
func TestWorkLedger_WakeOnlyUsesTimeoutWakeOnlyNoticeKind(t *testing.T) {
	t.Parallel()
	received := make(chan SessionAgentCall, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		received <- call
		return agentResultWithText("ok"), nil
	}}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("owner", subAgentDriver{agent: agent})
	l := newWorkLedger(nil)
	l.coord = coord
	coord.asyncJobs = l

	_, _, err := l.Start("owner", "call-1", "", "bash", "", false, false, &TimeoutSpec{
		Deadline: time.Now().Add(-time.Hour), Kind: timeoutWakeOnly, Seconds: 30,
	}, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "call-1")

	l.mu.Lock()
	job := l.bySession["owner"].jobs["call-1"]
	l.mu.Unlock()

	l.handleTimeout(job)

	select {
	case call := <-received:
		require.Equal(t, "timeout_wake_only", call.NoticeKind)
		require.Contains(t, call.Prompt, "still running")
	case <-time.After(2 * time.Second):
		t.Fatal("handleTimeout's wake_only branch never reached agent.Run")
	}
	require.Equal(t, phaseRunning, job.state, "wake_only must not transition the job to terminal")
}

// TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning: calling
// handleTimeout twice for the same wake_only job must fire the wake exactly
// once (the timeoutNotified one-shot guard), not twice.
// Revert-check performed: removed the `if job.timeoutNotified {return}`
// guard -- this test FAILED (calls.Load() was 2). Restored the guard;
// re-ran, passed.
func TestWorkLedger_WakeOnlyFiresExactlyOnceThenStaysRunning(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		calls.Add(1)
		return agentResultWithText("ok"), nil
	}}
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	coord.subAgentDrivers.register("owner", subAgentDriver{agent: agent})
	l := newWorkLedger(nil)
	l.coord = coord
	coord.asyncJobs = l

	_, _, err := l.Start("owner", "call-1", "", "bash", "", false, false, &TimeoutSpec{
		Deadline: time.Now().Add(-time.Hour), Kind: timeoutWakeOnly, Seconds: 30,
	}, func() {})
	require.NoError(t, err)
	l.acknowledged("owner", "call-1")

	l.mu.Lock()
	job := l.bySession["owner"].jobs["call-1"]
	l.mu.Unlock()

	l.handleTimeout(job)
	l.handleTimeout(job)

	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, phaseRunning, job.state)
}

// TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome extends ASYNC-03's
// phase-1 race test (TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome)
// with handleTimeout as a fourth competing terminal-transition source.
func TestWorkLedger_TimeoutRaceAgainstFinishYieldsOneOutcome(t *testing.T) {
	t.Parallel()
	for i := 0; i < 30; i++ {
		delivered := make(chan AsyncCompletion, 8)
		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
		_, _, err := l.Start("owner", "call", "", "bash", "", false, false, &TimeoutSpec{
			Deadline: time.Now().Add(-time.Hour), Kind: timeoutTerminateAndWake, Seconds: 30,
		}, func() {})
		require.NoError(t, err)
		l.acknowledged("owner", "call")

		l.mu.Lock()
		job := l.bySession["owner"].jobs["call"]
		l.mu.Unlock()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			l.finish("owner", "call", jobResult{content: "ok"})
		}()
		go func() {
			defer wg.Done()
			<-start
			l.handleTimeout(job)
		}()
		close(start)
		wg.Wait()

		got := drainCompletions(delivered)
		require.Len(t, got, 1, "exactly one terminal outcome must be delivered per race (iteration %d)", i)
	}
}

// TestParseTimeoutParam_ValidationTable covers §5.2/§5.3's validation rules.
func TestParseTimeoutParam_ValidationTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		toolName    string
		input       string
		wantErr     string
		wantNil     bool
		wantKind    timeoutKind
		wantSeconds int
	}{
		{name: "nothing set", toolName: tools.BashToolName, input: `{}`, wantNil: true},
		{name: "seconds too low", toolName: tools.BashToolName, input: `{"timeout":{"seconds":4,"kind":"wake_only"}}`, wantErr: "must be between"},
		{name: "seconds too high", toolName: tools.BashToolName, input: `{"timeout":{"seconds":604801,"kind":"wake_only"}}`, wantErr: "must be between"},
		{name: "kind empty", toolName: tools.BashToolName, input: `{"timeout":{"seconds":10,"kind":""}}`, wantErr: "must be"},
		{name: "kind unknown", toolName: tools.BashToolName, input: `{"timeout":{"seconds":10,"kind":"bogus"}}`, wantErr: "must be"},
		{name: "valid wake_only", toolName: tools.BashToolName, input: `{"timeout":{"seconds":10,"kind":"wake_only"}}`, wantKind: timeoutWakeOnly, wantSeconds: 10},
		{name: "valid terminate_and_wake", toolName: tools.BashToolName, input: `{"timeout":{"seconds":10,"kind":"terminate_and_wake"}}`, wantKind: timeoutTerminateAndWake, wantSeconds: 10},
		{name: "legacy timeout_seconds alias", toolName: tools.RunCommandToolName, input: `{"timeout_seconds":90}`, wantKind: timeoutTerminateAndWake, wantSeconds: 90},
		{name: "both set is an error", toolName: tools.RunCommandToolName, input: `{"timeout":{"seconds":10,"kind":"wake_only"},"timeout_seconds":90}`, wantErr: "at most one"},
		{name: "agentic_fetch ignored", toolName: tools.AgenticFetchToolName, input: `{"timeout":{"seconds":10,"kind":"wake_only"}}`, wantNil: true},
		{name: "agent tool respects timeout", toolName: AgentToolName, input: `{"timeout":{"seconds":600,"kind":"terminate_and_wake"}}`, wantKind: timeoutTerminateAndWake, wantSeconds: 600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := parseTimeoutParam(tc.toolName, tc.input)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, spec)
				return
			}
			require.NotNil(t, spec)
			require.Equal(t, tc.wantKind, spec.Kind)
			require.Equal(t, tc.wantSeconds, spec.Seconds)
		})
	}
}

// TestStopGuidanceFor_ToolSpecificAccuracy pins the orchestrator review's
// finding: the wake_only check-in text used to unconditionally offer both
// job_kill (bash/run_command) and stop_agent (agent) regardless of toolName.
// job_kill only actually controls a running bash job (run_command is
// explicitly refused by ResolveJobShellID), and stop_agent does not exist as
// a tool yet -- naming either for the wrong tool tells the model to do
// something that will fail or does not exist.
//
// Revert-check performed: reverted stopGuidanceFor to unconditionally return
// the old text naming both job_kill and stop_agent -- this test FAILED (the
// run_command and agent cases both wrongly contained "job_kill"/"stop_agent").
// Restored the fix; re-ran, passed.
func TestStopGuidanceFor_ToolSpecificAccuracy(t *testing.T) {
	t.Parallel()

	bashText := stopGuidanceFor(tools.BashToolName)
	require.Contains(t, bashText, "job_kill", "bash is the one tool job_kill actually controls")
	require.NotContains(t, bashText, "stop_agent")

	runCommandText := stopGuidanceFor(tools.RunCommandToolName)
	require.NotContains(t, runCommandText, "job_kill", "ResolveJobShellID explicitly refuses run_command jobs")
	require.NotContains(t, runCommandText, "stop_agent")
	require.Contains(t, runCommandText, "cannot be stopped")

	agentText := stopGuidanceFor(AgentToolName)
	require.NotContains(t, agentText, "stop_agent", "stop_agent does not exist as a tool yet")
	require.NotContains(t, agentText, "job_kill")
	require.Contains(t, agentText, "cannot be stopped")
}
