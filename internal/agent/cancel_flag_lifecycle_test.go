// R8A-2 / R8C-2 (agent part): a `sessions cancel` flag is a one-shot request.
// A human-initiated turn (web Run, a delegation resuming a child) starts with
// it cleared; an in-turn abort that honours it clears it; a turn of a session
// the `rush run` loop drives never clears it (the loop reads it between turns
// and clears it when IT honours it).
package agent

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// toolStepResponse serves one step that calls probe_tool.
func toolStepResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_p","type":"function","function":{"name":"probe_tool","arguments":"{}"}}]},"finish_reason":null}]}`)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl != nil {
		fl.Flush()
	}
}

// twoStepFixture is a session whose every turn is two provider steps (a tool
// call, then text); steps counts the provider requests.
type twoStepFixture struct {
	*attemptFixture
	steps atomic.Int32
}

func newTwoStepFixture(t *testing.T, opts attemptFixtureOpts) *twoStepFixture {
	t.Helper()
	probe := fantasy.NewAgentTool("probe_tool", "a probe", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	tf := &twoStepFixture{}
	opts.tools = []fantasy.AgentTool{probe}
	opts.handler = func(w http.ResponseWriter, _ *http.Request) {
		if tf.steps.Add(1)%2 == 1 {
			toolStepResponse(w)
			return
		}
		textFinishResponse(w, "done")
	}
	tf.attemptFixture = newAttemptFixture(t, "cancel-flag", opts)
	// History first, so no title generation adds requests.
	_, err := tf.env.messages.Create(context.Background(), tf.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier"}},
	})
	require.NoError(t, err)
	return tf
}

func (f *attemptFixture) cancelRequested() bool {
	f.t.Helper()
	ok, err := f.env.sessions.IsCancelRequested(context.Background(), f.sessID)
	require.NoError(f.t, err)
	return ok
}

// webCoordinator is the fixture's coordinator wired to its provider through a
// real config, so coord.Run is the web path end to end.
func (f *attemptFixture) webCoordinator() *coordinator {
	f.t.Helper()
	cfg, err := config.Init(f.env.workingDir, "", false)
	require.NoError(f.t, err)
	cfg.Config().Providers.Set("probe-provider", config.ProviderConfig{
		ID: "probe-provider", Type: openaicompat.Name, APIKey: "probe", BaseURL: f.srv.URL,
		Models: []catwalk.Model{{ID: "probe", DefaultMaxTokens: 1000, ContextWindow: 200000}},
	})
	sel := config.SelectedModel{Provider: "probe-provider", Model: "probe"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel
	c := f.coord
	c.cfg, c.sessions, c.messages = cfg, f.env.sessions, f.env.messages
	c.modelCache = csync.NewMap[string, cachedModelPair]()
	c.currentAgent = f.sa
	return c
}

// TestCancelFlag_StaleFlagDoesNotAbortAWebTurn: `sessions cancel` on an idle web
// session leaves the flag; the user's next prompt runs both its steps (today it
// is aborted after step 1: "session ... cancelled by user") and the flag is
// gone afterwards.
//
// Revert-check: dropping the clear at the start of runInternal's human turn
// aborts the turn after step 1 (one request, an error).
func TestCancelFlag_StaleFlagDoesNotAbortAWebTurn(t *testing.T) {
	ctx := context.Background()
	f := newTwoStepFixture(t, attemptFixtureOpts{noIdle: true, noDriver: true})
	coord := f.webCoordinator()
	require.NoError(t, f.env.sessions.RequestCancel(ctx, f.sessID))

	_, err := coord.Run(ctx, f.sessID, "new prompt")

	require.NoError(t, err)
	require.EqualValues(t, 2, f.steps.Load(), "both steps of the turn ran")
	require.False(t, f.cancelRequested())
}

// TestCancelFlag_LoopOwnedTurnsKeepTheFlag: the flag is the `rush run` loop's to
// honour. A turn started with per-call options (ExecuteRun: the loop's first and
// reviewer turns; it clears by itself where it is meant to) and a Drain both
// leave a pending request alone when the turn starts.
//
// Revert-check: clearing for every runInternal call turns both cases red.
func TestCancelFlag_LoopOwnedTurnsKeepTheFlag(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(context.Context) context.Context{
		"per-call options (ExecuteRun)": func(c context.Context) context.Context { return WithCallOptions(c, &CallOptions{}) },
		"Drain":                         WithDrainCall,
	}
	for name, mark := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTwoStepFixture(t, attemptFixtureOpts{noIdle: true, noDriver: true})
			coord := f.webCoordinator()
			require.NoError(t, f.env.sessions.RequestCancel(ctx, f.sessID))
			seen := make(chan bool, 1)
			coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				ok, err := f.env.sessions.IsCancelRequested(ctx, f.sessID)
				require.NoError(t, err)
				seen <- ok
				return nil, nil
			}}

			_, _ = coord.Run(mark(ctx), f.sessID, "")

			require.True(t, <-seen, "the flag is still set when the agent starts")
		})
	}
}

// TestCancelFlag_InTurnAbortHonoursAndClearsIt: a cancel that lands while a turn
// runs aborts it at the next step boundary (the intent) and is then spent: a
// later turn of the same session is not aborted by it.
//
// Revert-check: not clearing in enforceRunawayCaps leaves the flag set and the
// following turn aborts after step 1 too.
func TestCancelFlag_InTurnAbortHonoursAndClearsIt(t *testing.T) {
	ctx := context.Background()
	f := newTwoStepFixture(t, attemptFixtureOpts{noIdle: true, noDriver: true})
	require.NoError(t, f.env.sessions.RequestCancel(ctx, f.sessID))

	_, err := f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "go", MaxOutputTokens: 100})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "the abort cancels the turn")
	require.EqualValues(t, 1, f.steps.Load(), "aborted at the first step boundary")
	require.False(t, f.cancelRequested(), "honoured: the request is spent")

	f.steps.Store(0)
	_, err = f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "again", MaxOutputTokens: 100})
	require.NoError(t, err)
	require.EqualValues(t, 2, f.steps.Load(), "the next turn runs its two steps")
}

// TestCancelFlag_LoopDrivenSessionKeepsItAfterTheAbort: the session a `rush run`
// loop in THIS process drives (external driver) is not cleared by the turn: the
// loop reads the flag after the turn and ends the run canceled, then clears it.
// A Drain of such a session is aborted and leaves the flag; once no loop drives
// it, the same Drain honours the flag once and clears it.
//
// Revert-check: clearing without the external-driver guard empties the flag.
func TestCancelFlag_LoopDrivenSessionKeepsItAfterTheAbort(t *testing.T) {
	ctx := context.Background()
	f := newTwoStepFixture(t, attemptFixtureOpts{noIdle: true, noDriver: true})
	f.ledger.claimExternalDriver(f.sessID)
	f.seedDebt(ctx, "call-1", false)
	require.NoError(t, f.env.sessions.RequestCancel(ctx, f.sessID))

	_, err := f.sa.Run(ctx, newDrainCall(SessionAgentCall{SessionID: f.sessID, MaxOutputTokens: 100}))
	require.Error(t, err)
	require.EqualValues(t, 1, f.steps.Load())
	require.True(t, f.cancelRequested(), "the loop's flag survives the Drain that honoured it")

	f.ledger.releaseExternalDriver(f.sessID)
	f.steps.Store(0)
	f.seedDebt(ctx, "call-2", false) // the aborted turn's first step already reacted to call-1
	_, err = f.sa.Run(ctx, newDrainCall(SessionAgentCall{SessionID: f.sessID, MaxOutputTokens: 100}))
	require.Error(t, err, "without a loop the Drain honours the flag once")
	require.False(t, f.cancelRequested(), "and clears it")
}

// TestRunSubAgent_ResumeClearsAStaleCancelFlag: `sessions cancel C` left the flag
// on a child; a later delegation that resumes C must not be aborted after one
// step ("cancelled by user") every time.
//
// Revert-check: dropping the clear from runSubAgent's resume path leaves the
// flag set when the child's agent starts.
func TestRunSubAgent_ResumeClearsAStaleCancelFlag(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), env.sessions.CreateAgentToolSessionID("msg-1", "call-1"), parent.ID, "Child")
	require.NoError(t, err)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), child.ID))

	var flagAtStart bool
	agent := newMockAgent(providerID, 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		flagAtStart, err = env.sessions.IsCancelRequested(ctx, child.ID)
		return agentResultWithText("resumed"), nil
	})
	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-2", ToolCallID: "call-2",
		Prompt: "continue", SessionTitle: "Child", ResumeSessionID: child.ID,
	})
	require.NoError(t, err)
	require.False(t, flagAtStart, "the resumed child starts with the request spent")
}
