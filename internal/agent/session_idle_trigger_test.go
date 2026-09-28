package agent

// Phase 3's by-construction "session became idle" trigger (docs/plans/
// 2026-09-28-async-phase3-spec.md, orchestrator decision 2026-09-28 item 1):
// sessionAgent.onSessionIdle, fired from abandonOwnershipWithHandoff --
// the one function every Run()/RunWithReservedOwnership/ReleaseExclusive
// exit funnels through, regardless of caller.
//
// Every OTHER delegation test in this package (work_ledger_delegation_test.go)
// drives re-check trigger (iv) by calling coord.noteSubAgentChildRunEnded
// directly, simulating the manual per-caller trigger -- none of them
// exercise a REAL *sessionAgent's mailbox lifecycle at all. This file is the
// one test that does: it builds an actual driver via NewSessionAgent, runs
// its turn for real through Run(), and asserts the parked delegation is
// released WITHOUT this test ever calling noteSubAgentChildRunEnded/
// recheckChild itself -- proving the release came from the mailbox's own
// release path (onSessionIdle), not a manual call site. Written and passing
// BEFORE the safety-net ticker was deleted (with the ticker interval forced
// to an hour, so a pass could not be attributed to it) -- this was the
// orchestrator-mandated proof required before that deletion; the ticker
// itself, and this test's dependency on it, are gone as of the commit that
// added this comment.

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// blockUntilReleasedModel is a fantasy.LanguageModel whose Stream call blocks
// until release is closed, then yields a plain finish (no tool call, no
// text) -- enough for the turn to complete normally. Used to hold a REAL
// sessionAgent's mailbox at mbOwned (IsSessionBusy() == true) for a
// controlled window, so a delegation armed during that window cannot be
// released until this test explicitly lets the turn finish.
type blockUntilReleasedModel struct {
	release chan struct{}
}

func newBlockUntilReleasedModel() *blockUntilReleasedModel {
	return &blockUntilReleasedModel{release: make(chan struct{})}
}

func (m *blockUntilReleasedModel) unblock() { close(m.release) }

func (m *blockUntilReleasedModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &fantasy.Response{
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *blockUntilReleasedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

func (m *blockUntilReleasedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *blockUntilReleasedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*blockUntilReleasedModel) Provider() string { return "test" }
func (*blockUntilReleasedModel) Model() string    { return "block-until-released" }

// TestRecheckChild_FiresOnRealDriverRunEndWithoutManualTrigger is the
// orchestrator-mandated proof: a child turn run through its REAL driver
// (bypassing coordinator.runInternal entirely, exactly like wakeSession's
// production agent.Run call does) delivers the parent's parked delegation
// once it ends, with no ticker running and no test-side manual trigger call.
//
// Revert-check performed: removed the `a.onSessionIdle(sessionID)` call from
// abandonOwnershipWithHandoff -- this test hung until its own 5s timeout and
// failed with "the parked delegation must be delivered once the driver's
// real turn ends". Restored the call; re-ran, passed.
func TestRecheckChild_FiresOnRealDriverRunEndWithoutManualTrigger(t *testing.T) {
	env := testEnv(t)

	const parentSession = "trigger-parent"
	const parentCall = "call-trigger"

	child, err := env.sessions.Create(t.Context(), "trigger-child")
	require.NoError(t, err)
	childSession := child.ID

	delivered := make(chan AsyncCompletion, 4)
	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	model := newBlockUntilReleasedModel()
	driver := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:  Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
		AsyncJobs:  coord.asyncJobs,
		// The exact wiring buildAgent does for every driver it builds
		// (coordinator_tools.go) -- this test wires it directly since it
		// builds the driver without going through buildAgent.
		OnSessionIdle: coord.noteSubAgentChildRunEnded,
	})
	coord.subAgentDrivers.register(childSession, subAgentDriver{agent: driver, parentSessionID: parentSession})

	// Start the child's real turn -- AutoResumed+BackgroundJobNotice skip
	// title generation (agent_turn.go's needsTitle), so the blocking model
	// is only ever invoked once, for the turn itself.
	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = driver.Run(context.Background(), SessionAgentCall{
			SessionID:           childSession,
			Prompt:              "resume",
			AutoResumed:         true,
			BackgroundJobNotice: true,
		})
	}()

	require.Eventually(t, func() bool {
		select {
		case <-runDone:
			t.Fatalf("driver.Run returned early, err=%v", runErr)
		default:
		}
		return driver.IsSessionBusy(childSession)
	}, 2*time.Second, 5*time.Millisecond, "the child's real turn must be mid-flight before the delegation arms")

	// Park a delegation while the driver is genuinely busy: armDelegation's
	// own synchronous trigger (i) must NOT deliver it, since childScopeDrained
	// reads the driver's real (busy) IsSessionBusy.
	_, _, err = coord.asyncJobs.Start(parentSession, parentCall, "", AgentToolName, childSession, false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged(parentSession, parentCall)
	coord.asyncJobs.armDelegation(parentSession, parentCall, jobResult{content: "child yielded: still working"})
	require.True(t, coord.asyncJobs.hasParked())
	require.Empty(t, drainCompletions(delivered),
		"the delegation must not be delivered while the real driver is still busy")

	// Let the child's turn actually finish -- this is the ONLY thing that
	// makes the mailbox mbIdle; no test code calls recheckChild/
	// noteSubAgentChildRunEnded anywhere in this test.
	model.unblock()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver's real turn did not finish")
	}

	var got []AsyncCompletion
	require.Eventually(t, func() bool {
		got = append(got, drainCompletions(delivered)...)
		return len(got) > 0
	}, 5*time.Second, 5*time.Millisecond,
		"the parked delegation must be delivered once the driver's real turn ends")
	require.Len(t, got, 1)
	require.Equal(t, parentCall, got[0].ToolCallID)
	require.Contains(t, got[0].Content, "child yielded")
	require.False(t, coord.asyncJobs.hasParked())
	require.False(t, driver.IsSessionBusy(childSession))
}
