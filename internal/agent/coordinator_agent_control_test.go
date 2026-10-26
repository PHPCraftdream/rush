package agent

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// controlStubAgent is a driver stub for the control-tool tests: its busy
// flag mirrors a live generation and is cleared by Cancel (like a real
// mailbox), and InterruptAndReplace reports "nothing to interrupt" when
// idle — mockSessionAgent's unconditional true would mask §4.2's fallback.
type controlStubAgent struct {
	mockSessionAgent
	mu   sync.Mutex
	busy bool
}

func (s *controlStubAgent) IsSessionBusy(string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *controlStubAgent) setBusy(busy bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

func (s *controlStubAgent) Cancel(sessionID string) {
	s.mockSessionAgent.Cancel(sessionID)
	s.setBusy(false)
}

func (s *controlStubAgent) InterruptAndReplace(sessionID string, call SessionAgentCall) bool {
	s.mu.Lock()
	busy := s.busy
	s.mu.Unlock()
	if !busy {
		return false
	}
	s.mockSessionAgent.InterruptAndReplace(sessionID, call)
	return true
}

// newControlRealAgent builds a REAL sessionAgent backed by the SSE stand-in,
// so inject paths genuinely persist messages and report real idle/busy.
func newControlRealAgent(t *testing.T, env fakeEnv, srvURL string) SessionAgent {
	t.Helper()
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srvURL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	return NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{
			Model:      lm,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
			ModelCfg:   config.SelectedModel{Provider: "control-probe", Model: "probe"},
		},
		FastModel:            Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        t.TempDir(),
	})
}

// newControlCoordinator builds the minimal coordinator the sub-agent control
// tools (coordinator_agent_control.go) need: a real session/message store, a
// resolvable model config pointing at a never-called SSE stand-in, a REAL
// root agent (inject paths must persist), and the batch node facts bound
// (delegation state and ListDelegations read them).
func newControlCoordinator(t *testing.T) (*coordinator, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	srv := singleTurnSSEServer(nil)
	t.Cleanup(srv.Close)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	const providerID = "control-probe"
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Type:    "openai",
		BaseURL: srv.URL,
		Models:  []catwalk.Model{{ID: "probe", Name: "Probe", DefaultMaxTokens: 1000}},
	})
	cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: providerID, Model: "probe"}
	cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: providerID, Model: "probe"}

	coord := &coordinator{
		cfg:          cfg,
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: newControlRealAgent(t, env, srv.URL),
		modelCache:   csync.NewMap[string, cachedModelPair](),
	}
	coord.subAgentDrivers = newSubAgentDriverRegistry()
	bindBatchNodeFacts(t, coord, env.conn, env.workingDir)
	return coord, env
}

// controlChild creates a parent and a delegated child session pair
// (child.ParentSessionID == parent.ID).
func controlChild(t *testing.T, env fakeEnv, suffix string) (parentID, childID string) {
	t.Helper()
	parent, err := env.sessions.Create(t.Context(), "control parent "+suffix)
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), parent.ID+"$$c-"+suffix, parent.ID, "child "+suffix)
	require.NoError(t, err)
	return parent.ID, child.ID
}

// seedControlDelegation seeds the LIVE delegation child-link parent->child in the
// batch store (a running agent task node naming childID) — the durable state
// childDelegationState/ListDelegations/StopAgent read. Callers pass any
// coordinator; the seed goes through the shared batch helper.
func seedControlDelegation(t *testing.T, env fakeEnv, coord *coordinator, parentID, childID string) {
	t.Helper()
	_ = coord
	seedBatchDelegation(t, env.conn, parentID, "call-"+childID, childID, "agent", "running", false)
}

// releaseDelegation projects the delegation root AND child terminal — the
// delivered shape the history fallback then answers from.
func releaseDelegation(t *testing.T, conn *sql.DB, owner, childID, state string) {
	t.Helper()
	ctx := context.Background()
	var rootID string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`,
		owner, "call-"+childID).Scan(&rootID))
	var nodeID string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes WHERE root_id=? AND node_id<>root_id LIMIT 1`, rootID).Scan(&nodeID))
	_, err := conn.ExecContext(ctx,
		`UPDATE batch_nodes SET state=?, outcome_state=? WHERE node_id IN (?, ?)`, state, state, rootID, nodeID)
	require.NoError(t, err)
}

func TestAgentControl_MissingChildRefusedAllThree(t *testing.T) {
	t.Parallel()
	coord, _ := newControlCoordinator(t)
	ctx := t.Context()

	_, err := coord.InspectAgent(ctx, "p", "missing-1")
	require.ErrorContains(t, err, `child_session_id "missing-1" not found`)
	_, err = coord.InjectAgent(ctx, "p", "missing-1", "hi", false)
	require.ErrorContains(t, err, `child_session_id "missing-1" not found`)
	_, err = coord.StopAgent(ctx, "p", "missing-1")
	require.ErrorContains(t, err, `child_session_id "missing-1" not found`)
}

func TestAgentControl_ForeignChildRefusedAllThree(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	_, otherChild := controlChild(t, env, "foreign")
	parent, _ := controlChild(t, env, "caller")
	ctx := t.Context()

	_, err := coord.InspectAgent(ctx, parent, otherChild)
	require.ErrorContains(t, err, "is not a child of this session; refusing to inspect")

	_, err = coord.InjectAgent(ctx, parent, otherChild, "hi", false)
	require.ErrorContains(t, err, "is not a child of this session; refusing to inject")

	_, err = coord.StopAgent(ctx, parent, otherChild)
	require.ErrorContains(t, err, "is not a child of this session; refusing to stop")
}

func TestInspectAgent_Statuses(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "insp")
	ctx := t.Context()

	// Never-run child: idle.
	insp, err := coord.InspectAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Equal(t, "idle", insp.Status)
	require.Zero(t, insp.QueuedMessages)

	// Busy child via its registered DRIVER (task #1054 routing): running —
	// revert-check: routing InspectAgent's busy check through c.currentAgent
	// instead of agentFor makes this report idle and the test fail.
	stub := &controlStubAgent{}
	stub.setBusy(true)
	coord.subAgentDrivers.register(child, subAgentDriver{agent: stub, parentSessionID: parent})
	insp, err = coord.InspectAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Equal(t, "running", insp.Status)

	// awaiting_answer: the exact question-stop finish literal recorded on
	// the child's last assistant message.
	_, err = env.messages.Create(ctx, child, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.Finish{Reason: message.FinishReasonError, Message: awaitingAnswerStoppedTitle}},
	})
	require.NoError(t, err)
	stub.setBusy(false)
	insp, err = coord.InspectAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Equal(t, "awaiting_answer", insp.Status)

	// Armed-but-not-running and terminal states on a SECOND child (the
	// awaiting literal legitimately outranks per §4.1's priority, so it must
	// not sit in this child's history here). A LIVE batch delegation with an
	// idle driver is §4.1's idle case: the delegation is still live, the
	// child not executing a turn.
	parent2, child2 := controlChild(t, env, "insp2")
	stub2 := &controlStubAgent{}
	coord.subAgentDrivers.register(child2, subAgentDriver{agent: stub2, parentSessionID: parent2})
	seedControlDelegation(t, env, coord, parent2, child2)
	insp, err = coord.InspectAgent(ctx, parent2, child2)
	require.NoError(t, err)
	require.Equal(t, "idle", insp.Status)

	// Terminal: release the delegation, then let the child's own finished
	// assistant message (a cancelled finish, what a stopped delegation's
	// history records) answer through the history fallback.
	releaseDelegation(t, env.conn, parent2, child2, "cancelled")
	_, err = env.messages.Create(ctx, child2, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.Finish{Reason: message.FinishReasonCanceled}},
	})
	require.NoError(t, err)
	insp, err = coord.InspectAgent(ctx, parent2, child2)
	require.NoError(t, err)
	require.Equal(t, "cancelled", insp.Status)
}

func TestStopAgent_IdempotentRefusalWhenNothingToStop(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "stop-idle")
	ctx := t.Context()

	_, err := coord.StopAgent(ctx, parent, child)
	require.ErrorContains(t, err, `child_session_id "`+child+`" has no active turn or pending delegation to stop`)

	// A live turn + armed delegation: stop succeeds...
	stub := &controlStubAgent{}
	stub.setBusy(true)
	coord.subAgentDrivers.register(child, subAgentDriver{agent: stub, parentSessionID: parent})
	seedControlDelegation(t, env, coord, parent, child)
	t.Cleanup(func() { releaseDelegation(t, env.conn, parent, child, "cancelled") })
	text, err := coord.StopAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Contains(t, text, "Sub-agent session "+child+" stopped")
	require.Contains(t, text, "resume_session_id")
	require.Contains(t, stub.cancelled, child, "the child's driver generation must be cancelled")

	// ...and the SECOND call is the same idempotent refusal (§4.3/§1.5),
	// not a disguised success.
	_, err = coord.StopAgent(ctx, parent, child)
	require.ErrorContains(t, err, "has no active turn or pending delegation to stop")
}

func TestStopAgent_StopsOnlyThisChild_DeliversExactlyOneEvent(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "stop-hot")
	siblingParent, siblingChild := controlChild(t, env, "sibling")
	ctx := t.Context()

	// The child's driver must report busy, or StopAgent refuses a child with
	// nothing active.
	stub := &controlStubAgent{}
	stub.setBusy(true)
	coord.subAgentDrivers.register(child, subAgentDriver{agent: stub, parentSessionID: parent})
	seedControlDelegation(t, env, coord, parent, child)
	sib := &controlStubAgent{}
	sib.setBusy(true)
	coord.subAgentDrivers.register(siblingChild, subAgentDriver{agent: sib, parentSessionID: siblingParent})
	seedControlDelegation(t, env, coord, siblingParent, siblingChild)

	text, err := coord.StopAgent(ctx, parent, child)
	require.NoError(t, err)
	require.Contains(t, text, "stopped")
	require.Contains(t, stub.cancelled, child, "this child's driver generation must be cancelled")
	require.Empty(t, sib.cancelled, "the sibling's driver must be untouched by stop_agent on another child")
	require.False(t, stub.IsSessionBusy(child), "the stopped child's turn must be cancelled")

	// The sibling delegation is untouched and still running.
	sibState, ok := coord.childDelegationState(ctx, siblingChild)
	require.True(t, ok)
	require.Equal(t, "running", sibState, "a sibling delegation must be untouched by stop_agent on another child")
}

func TestInjectAgent_QueuesForIdleChildWithoutStartingTurn(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "inj-idle")
	ctx := t.Context()

	text, err := coord.InjectAgent(ctx, parent, child, "answer is 42", false)
	require.NoError(t, err)
	require.Contains(t, text, "nothing runs until you call agent with resume_session_id")
	require.Contains(t, text, child)

	// The message is persisted into the child's history...
	msgs, err := env.messages.List(ctx, child)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.Equal(t, "answer is 42", msgs[len(msgs)-1].FullText())

	// ...but NO turn was started (contract §4.2: idle inject only saves).
	require.False(t, coord.agentFor(child).IsSessionBusy(child))
}

func TestInjectAgent_InterruptOnIdleChildFallsBackToQueue(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "inj-idle-int")
	ctx := t.Context()

	// InterruptAndReplace on an idle session returns false — the exact
	// fallback §4.2 requires, with the exact text.
	text, err := coord.InjectAgent(ctx, parent, child, "new instruction", true)
	require.NoError(t, err)
	require.Equal(t, "child session was idle, nothing to interrupt — message queued instead; call agent with resume_session_id to resume it", text)

	msgs, err := env.messages.List(ctx, child)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "the fallback must still persist the message")
}

func TestInjectAgent_InterruptOnBusyChildRoutesToItsDriver(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "inj-busy")
	ctx := t.Context()

	// A busy child's generation runs on its registered driver; interrupt
	// must land THERE (task #1054), not on c.currentAgent.
	// Revert-check: routing through c.currentAgent (the mock at the root)
	// leaves this driver's interruptAndReplaced empty and the test fails.
	driverBusy := &controlStubAgent{}
	driverBusy.setBusy(true)
	coord.subAgentDrivers.register(child, subAgentDriver{agent: driverBusy, parentSessionID: parent})

	text, err := coord.InjectAgent(ctx, parent, child, "redirect now", true)
	require.NoError(t, err)
	require.Contains(t, text, "Interrupted the sub-agent's current turn")
	replaced := driverBusy.interruptAndReplacedSnapshot()
	require.Len(t, replaced, 1, "the interrupt must reach the child's driver, not c.currentAgent")
	require.Equal(t, "redirect now", replaced[0].Prompt)
	require.Equal(t, child, replaced[0].SessionID)
	// The root agent (a real sessionAgent here) must not be the interrupt's
	// target: the child is idle from ITS point of view, so its InterruptAndReplace
	// would have returned false and the fallback text would differ.
	msgs, err := env.messages.List(ctx, child)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, "redirect now", m.FullText(),
			"the interrupt message must reach the driver, not be re-persisted by the root path")
	}
}

func TestInjectAgent_InterruptErrorSurfaced(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "inj-err")
	ctx := t.Context()
	// No resolvable session (deleted between ownership check and model
	// resolution is not reachable here) — instead prove a buildCall failure
	// surfaces as a model-safe error, not a Go error: point the coordinator
	// at an unresolvable provider by clearing models.
	coord.cfg.Config().Models = map[config.SelectedModelType]config.SelectedModel{}
	driver := newBusyStubAgent()
	driver.setBusy(true)
	coord.subAgentDrivers.register(child, subAgentDriver{agent: driver, parentSessionID: parent})
	_, err := coord.InjectAgent(ctx, parent, child, "x", true)
	require.Error(t, err)
}

func TestListDelegations_ListsOnlyCallersLiveChildren(t *testing.T) {
	t.Parallel()
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "list")
	otherParent, otherChild := controlChild(t, env, "list-other")
	// A SECOND child of the same parent: controlChild makes one (parent,
	// child) pair, so the second child is created by the same pattern.
	second, err := env.sessions.CreateTaskSession(t.Context(), parent+"$$c-list2b", parent, "child list2b")
	require.NoError(t, err)
	secondChild := second.ID
	ctx := t.Context()

	// A plain batch command root (no child session) owned by the same
	// caller: NOT a delegation. It must never appear in ListDelegations'
	// output or its count -- only childSession-carrying task nodes are
	// delegations. Revert-check: dropping the ChildSessionID filter in
	// ListDelegations makes the assertions below fail on this root.
	seedBatchDelegation(t, env.conn, parent, "bash-call-list", "", "command", "running", false)

	got, err := coord.ListDelegations(ctx, parent)
	require.NoError(t, err)
	require.Empty(t, got)

	// Two live delegations for parent (second child of the same caller) and
	// one for the sibling caller.
	for _, tc := range []struct{ owner, driver string }{{parent, child}, {parent, secondChild}, {otherParent, otherChild}} {
		stub := &controlStubAgent{}
		coord.subAgentDrivers.register(tc.driver, subAgentDriver{agent: stub, parentSessionID: tc.owner})
		seedControlDelegation(t, env, coord, tc.owner, tc.driver)
	}
	t.Cleanup(func() {
		releaseDelegation(t, env.conn, parent, child, "cancelled")
		releaseDelegation(t, env.conn, parent, secondChild, "cancelled")
		releaseDelegation(t, env.conn, otherParent, otherChild, "cancelled")
	})

	// AgeSeconds is a whole-second truncation of time.Since(startedAt), so
	// the listing right after arming would legitimately read 0 -- wait out a
	// second before re-reading it as a live teenager.
	time.Sleep(time.Second + 100*time.Millisecond)

	got, err = coord.ListDelegations(ctx, parent)
	require.NoError(t, err)
	require.Len(t, got, 2, "only the caller's own live delegations, not the sibling's")
	require.Equal(t, child, got[0].ChildSessionID)
	require.Equal(t, secondChild, got[1].ChildSessionID)
	require.Equal(t, "agent", got[0].ToolName)
	require.NotEmpty(t, got[0].StartedAt)
	require.Positive(t, got[0].AgeSeconds)

	for _, s := range got {
		require.NotEmpty(t, s.ChildSessionID, "a plain async job is not a delegation and must not be listed")
		require.Equal(t, "agent", s.ToolName)
	}
}
