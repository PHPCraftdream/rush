package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchAgentReactionScope_OwedDebtKeepsScopeOpen(t *testing.T) {
	t.Parallel()
	// PREMATURE CLOSURE: reverting DescendantsLive to the HasLive-only read
	// closes the delegation while the child still owes its selected
	// completion reaction.
	facts := pscChildReactionFacts{
		scope: func(context.Context, string) (CLIScopeState, error) {
			return CLIScopeState{Drain: DrainOwed}, nil
		},
	}
	live, complete := pscCombineDescendantFacts(context.Background(), facts, "child", false, true)
	require.True(t, live, "an owed completion reaction must keep the delegation scope open")
	require.True(t, complete, "a readable owed-debt read must not degrade completeness")
}

func TestBatchAgentReactionScope_PacedReactionKeepsScopeOpen(t *testing.T) {
	t.Parallel()
	// Catches: closure while the child's reaction is queued/retrying/paced
	// after an injected failure.
	facts := pscChildReactionFacts{
		scope: func(context.Context, string) (CLIScopeState, error) {
			return CLIScopeState{Drain: DrainPaced}, nil
		},
	}
	live, complete := pscCombineDescendantFacts(context.Background(), facts, "child", false, true)
	require.True(t, live, "a paced reaction must keep the delegation scope open")
	require.True(t, complete)
}

func TestBatchAgentReactionScope_RunningChildWorkKeepsScopeOpen(t *testing.T) {
	t.Parallel()
	facts := pscChildReactionFacts{
		scope: func(context.Context, string) (CLIScopeState, error) {
			return CLIScopeState{WorkOpen: true}, nil
		},
	}
	live, complete := pscCombineDescendantFacts(context.Background(), facts, "child", false, true)
	require.True(t, live, "running work on a live host must keep the delegation scope open")
	require.True(t, complete)
}

func TestBatchAgentReactionScope_IncompleteChildReadStaysOpen(t *testing.T) {
	t.Parallel()
	// Catches: an unreadable child scope read closing the scope
	// optimistically (must fail conservative).
	facts := pscChildReactionFacts{
		scope: func(context.Context, string) (CLIScopeState, error) {
			return CLIScopeState{}, errors.New("boom")
		},
	}
	live, complete := pscCombineDescendantFacts(context.Background(), facts, "child", false, true)
	require.True(t, live, "a failed child scope read must keep the scope open")
	require.False(t, complete, "a failed child scope read must be reported incomplete")
}

func TestBatchAgentReactionScope_NonActionableDebtClosesScope(t *testing.T) {
	t.Parallel()
	// Deferred (held question, Stop, policy) and stuck debt must NOT hang
	// the delegation forever.
	for _, drain := range []DrainState{DrainNone, DrainDeferred, DrainStuck} {
		facts := pscChildReactionFacts{
			scope: func(context.Context, string) (CLIScopeState, error) {
				return CLIScopeState{Drain: drain}, nil
			},
		}
		live, complete := pscCombineDescendantFacts(context.Background(), facts, "child", false, true)
		require.False(t, live, "drain %d: non-actionable debt must not keep the scope open", drain)
		require.True(t, complete, "drain %d: non-actionable debt must report a complete read", drain)
	}
}

func TestBatchAgentReactionScope_RefreshReplacesStaleYieldWithFinalOutcome(t *testing.T) {
	t.Parallel()
	// STALE RESULT CAPTURE: settling with the initial pre-reaction yield
	// text instead of the child's final outcome.
	facts := pscChildReactionFacts{
		outcome: func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool) {
			return AsyncCompletion{Content: "child done: build output received"}, true
		},
	}
	got := pscRefreshedTurnResult(facts,
		batchAgentRunRequest{ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent"},
		batchAgentTurnResult{Response: fantasy.ToolResponse{Content: "root waiting"}},
		nil)
	require.Equal(t, "child done: build output received", got.Response.Content)
	require.False(t, got.Response.IsError)
}

func TestBatchAgentReactionScope_RefreshDeliversReactionFailureNotStaleSuccess(t *testing.T) {
	t.Parallel()
	// Catches: a permanently failed reaction settlement being reported as
	// the child's stale success text.
	facts := pscChildReactionFacts{
		outcome: func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool) {
			return AsyncCompletion{Content: "wake_failed: child reaction failed", IsError: true}, true
		},
	}
	got := pscRefreshedTurnResult(facts,
		batchAgentRunRequest{ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent"},
		batchAgentTurnResult{Response: fantasy.ToolResponse{Content: "root waiting"}},
		nil)
	require.True(t, got.Response.IsError)
	require.Contains(t, got.Response.Content, "wake_failed")
}

func TestBatchAgentReactionScope_RefreshKeepsInitialWhenOutcomeUnavailable(t *testing.T) {
	t.Parallel()
	facts := pscChildReactionFacts{
		outcome: func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool) {
			return AsyncCompletion{}, false
		},
	}
	got := pscRefreshedTurnResult(facts,
		batchAgentRunRequest{ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent"},
		batchAgentTurnResult{Response: fantasy.ToolResponse{Content: "root waiting", IsError: false}},
		nil)
	require.Equal(t, "root waiting", got.Response.Content)
	require.False(t, got.Response.IsError)
}

func TestBatchAgentReactionScope_RefreshSkippedForTurnErrorNoChildAndUnselected(t *testing.T) {
	t.Parallel()
	initial := batchAgentTurnResult{Response: fantasy.ToolResponse{Content: "root waiting"}}
	cases := []struct {
		name string
		req  batchAgentRunRequest
		err  error
	}{
		{"turn error", batchAgentRunRequest{ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent"}, errors.New("leaf failed")},
		{"no child session", batchAgentRunRequest{ChildSessionID: "", ToolCallID: "call-agent", Tool: "agent"}, nil},
		{"no child selected", batchAgentRunRequest{ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent", NoChildSelected: true}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var called atomic.Bool
			facts := pscChildReactionFacts{
				outcome: func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool) {
					called.Store(true)
					return AsyncCompletion{Content: "unrelated"}, true
				},
			}
			got := pscRefreshedTurnResult(facts, tc.req, initial, tc.err)
			require.False(t, called.Load(), "the outcome refresh must not run")
			require.Equal(t, initial, got)
		})
	}
}

func TestBatchAgentReactionScope_WatchSettlesRefreshedOutcome(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	turn := &pscFakeTurn{done: closed, exit: closed, resp: fantasy.ToolResponse{Content: "child waiting for build"}}
	e := &batchAgentExecution{
		nodeID: "n1", rootID: "r1",
		deps: batchAgentDeps{
			Descendants: pscTerminalDescendants{},
			ChildFacts: pscChildReactionFacts{
				outcome: func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool) {
					return AsyncCompletion{Content: "child done: build output received"}, true
				},
			},
		},
		req:        batchAgentRunRequest{SessionID: "s", ChildSessionID: "child", ToolCallID: "call-agent", Tool: "agent"},
		scope:      newBatchAgentScope(),
		driverDone: make(chan struct{}), watcherDone: make(chan struct{}), finished: make(chan struct{}),
	}
	var got jobbatch.Result
	e.watch(context.Background(), turn, func(r jobbatch.Result) { got = r })
	require.Equal(t, jobbatch.StateCompleted, got.State)
	require.Equal(t, "child done: build output received", got.Summary)
}

type pscTerminalDescendants struct{}

func (pscTerminalDescendants) DescendantsLive(context.Context, batchAgentRunRequest) (bool, bool) {
	return false, true
}

type pscFakeTurn struct {
	done, exit chan struct{}
	resp       fantasy.ToolResponse
}

func (t *pscFakeTurn) Done() <-chan struct{}        { return t.done }
func (t *pscFakeTurn) Exit() <-chan struct{}        { return t.exit }
func (t *pscFakeTurn) Result() batchAgentTurnResult { return batchAgentTurnResult{Response: t.resp} }
func (t *pscFakeTurn) Err() error                   { return nil }
func (t *pscFakeTurn) Stop(context.Context) error   { return nil }
