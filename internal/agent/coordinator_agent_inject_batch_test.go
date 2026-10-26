package agent

// Inject_agent held-question bridge (design D1) oracles. Dual to the
// held-question fixtures (batch_held_question_inject_test.go /
// batch_disk_provider_question_test.go): the happy path runs the REAL
// parked child, the REAL published marker, the REAL fenced store read and
// the REAL Runner.Inject; the fence/rejection cases run the REAL sink over
// store-reserved batch rows (seedDelegation/commitMarkerNotice — the same
// constraint-valid seeding the cli_active_work batch-question fixtures use)
// with a recording fake runner.
//
// Production mutation each test catches:
//   - HappyPath: deleting the bridge call in InjectAgent leaves the child
//     parked — the tool returns the legacy "Message saved..." text, the Ask
//     never returns, the node never settles completed and the leaf never
//     runs its second op. Answering through a NEW generation instead of the
//     SAME incarnation shows as runCount > 1 or a changed node token.
//   - ForeignChild: routing delivery before (or without) controlChild would
//     let a non-owner answer another session's delegation; the fake sink's
//     call counter stays at zero only while the ownership check runs first.
//   - Fence: dropping the token/live-state fence (or the child filter)
//     would deliver into a STALE or TERMINAL incarnation; each fenced-out
//     case must answer ErrBatchAgentStaleGeneration with zero runner calls.
//   - RunnerRejections: swallowing (or remapping) the deterministic
//     Inject errors would lose the answer silently or revive a stopped
//     incarnation; each mapped error must propagate with the node named. A
//     closed sink must answer jobbatch.ErrReleased-shaped, never None.
//   - NoHeldQuestion: treating "no committed held question" as a delivery
//     (or as an error) would break the byte-identical legacy fallback; the
//     exact legacy texts, EXACTLY ONE InjectMessage delivery and zero
//     runner calls are asserted through the counting driver spy.
//   - InjectAgentSurfacesRejection: a deterministic sink error surfaced
//     through InjectAgent must reach the caller naming the reason — falling
//     back to the mailbox instead shows as a nil error plus one counted
//     InjectMessage.
//   - SinkAbsent: a flag-OFF coordinator (no sink) or a shutdown-cleared
//     sink must take the legacy path; a success-shaped sink cleared from
//     the registry must stop answering.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// bridgeFakeRunner records every Inject and answers with a fixed error.
type bridgeFakeRunner struct {
	mu      sync.Mutex
	injects []bridgeInjectRecord
	err     error
}

type bridgeInjectRecord struct {
	id  jobbatch.ID
	msg json.RawMessage
}

func (r *bridgeFakeRunner) Inject(_ context.Context, id jobbatch.ID, msg json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injects = append(r.injects, bridgeInjectRecord{id: id, msg: append(json.RawMessage(nil), msg...)})
	return r.err
}

func (r *bridgeFakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.injects)
}

// bridgeFakeSink counts DeliverHeldAnswer calls (ownership-check oracle).
type bridgeFakeSink struct {
	mu    sync.Mutex
	calls int
}

func (s *bridgeFakeSink) DeliverHeldAnswer(context.Context, string, string, string) (BatchHeldQuestionFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return BatchHeldQuestionFact{}, ErrBatchHeldAnswerNone
}

func (s *bridgeFakeSink) Close() {}

func (s *bridgeFakeSink) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// bridgeSuccessSink always answers with a fixed fact (clear-on-shutdown oracle).
type bridgeSuccessSink struct{ fact BatchHeldQuestionFact }

func (s bridgeSuccessSink) DeliverHeldAnswer(context.Context, string, string, string) (BatchHeldQuestionFact, error) {
	return s.fact, nil
}

func (s bridgeSuccessSink) Close() {}

// bridgeErrorSink always fails with a fixed error (InjectAgent surfacing oracle).
type bridgeErrorSink struct{ err error }

func (s bridgeErrorSink) DeliverHeldAnswer(context.Context, string, string, string) (BatchHeldQuestionFact, error) {
	return BatchHeldQuestionFact{}, s.err
}

func (s bridgeErrorSink) Close() {}

// bridgeCountingAgent counts exactly how many InjectMessage deliveries the
// coordinator made into its child mailbox (the legacy-path "exactly once"
// oracle — the result string alone does not prove the delivery).
type bridgeCountingAgent struct {
	controlStubAgent
	mu  sync.Mutex
	inj int
}

func (a *bridgeCountingAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	a.mu.Lock()
	a.inj++
	a.mu.Unlock()
	return a.controlStubAgent.InjectMessage(ctx, call)
}

func (a *bridgeCountingAgent) injectCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inj
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// TestBatchInjectBridge_HappyPathReleasesHeldQuestion runs the REAL parked
// child: inject_agent must release the held Ask through Runner.Inject, the
// SAME incarnation answers and completes, and exactly ONE launch happened.
func TestBatchInjectBridge_HappyPathReleasesHeldQuestion(t *testing.T) {
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()

	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the held question is published")
	node, token, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)

	// The fixture's child session becomes a real child of the batch owner,
	// so controlChild's ownership check passes for InjectAgent. The marker's
	// child field must name exactly this session.
	var childID string
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT id FROM sessions WHERE title = 'q8 child' LIMIT 1`).Scan(&childID))
	require.Equal(t, batchQuestionMarkerChild(text), childID,
		"the persisted question names THIS child session")
	_, err := env.conn.ExecContext(context.Background(),
		`UPDATE sessions SET parent_session_id = ?1 WHERE id = ?2`, env.sessionID, childID)
	require.NoError(t, err)

	coord, _ := newControlCoordinator(t)
	coord.sessions = env.sessions
	coord.messages = env.messages
	sink, serr := NewBatchHeldAnswerSink(env.conn, env.install.Runner)
	require.NoError(t, serr)
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })

	out, ierr := coord.InjectAgent(context.Background(), env.sessionID, childID, "yes, proceed", false)
	require.NoError(t, ierr, "the bridge delivers the answer")
	require.Contains(t, out, "SAME batch execution", "the tool names the continuing execution")
	require.Contains(t, out, node, "the tool names the answered batch node")
	require.Contains(t, out, itoa64(token), "the tool names the answered incarnation token")
	require.NotContains(t, out, "Message saved", "the legacy mailbox path was not taken")

	// The SAME incarnation wakes, runs op2 and settles completed — the held
	// CHILD node (the marker's node) settles with an UNCHANGED token, and
	// the root settles completed (no replacement incarnation anywhere).
	var state string
	var doneToken int64
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT state, token FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, env.sessionID).Scan(&state, &doneToken) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")
	require.Equal(t, token, doneToken, "the incarnation token is unchanged")
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			string(root), env.sessionID).Scan(&state) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the root settles completed")
	_, _, _, _, _, rawAnswer := env.leaf.snapshot()
	// The recorder stores the Ask tool's response CONTENT, i.e. the bridge's
	// JSON-encoded answer already decoded back to plain text.
	require.Equal(t, "yes, proceed", string(rawAnswer), "the injected answer reached the asker")
	require.Equal(t, 1, env.leaf.runCount(), "exactly ONE launch: no new generation")

	// Release hygiene, as in TestHeldQuestionInject.
	require.NoError(t, env.install.Runner.Release(context.Background(), root))
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond, "no provider owner remains")
}

// TestBatchInjectBridge_ForeignChildRejectedBeforeDelivery: controlChild
// rejects before the sink is ever consulted.
func TestBatchInjectBridge_ForeignChildRejectedBeforeDelivery(t *testing.T) {
	coord, env := newControlCoordinator(t)
	_, otherChild := controlChild(t, env, "bridge-foreign")
	parent, _ := controlChild(t, env, "bridge-caller")
	sink := &bridgeFakeSink{}
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })

	_, err := coord.InjectAgent(t.Context(), parent, otherChild, "hi", false)
	require.ErrorContains(t, err, "is not a child of this session; refusing to inject")
	require.Zero(t, sink.callCount(), "the ownership check runs before any delivery")
}

// TestBatchInjectBridge_FenceRejectsStaleTokenTerminalOrMissingRow: the
// sink reuses LiveHeldQuestions' incarnation fence — a candidate notice
// whose batch_nodes row is missing, stale-tokened or terminal is a
// deterministic stale-generation rejection with zero runner calls. Rows are
// seeded through the REAL store Reserve + production publisher, so every
// row is constraint-valid by construction.
func TestBatchInjectBridge_FenceRejectsStaleTokenTerminalOrMissingRow(t *testing.T) {
	_, conn, _ := newTestBatchStore(t)
	q := db.New(conn)
	ctx := context.Background()
	runner := &bridgeFakeRunner{}
	sink, serr := NewBatchHeldAnswerSink(conn, runner)
	require.NoError(t, serr)
	const owner = "bridge-owner-1"
	const child = "bridge-child-1"
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)

	// Stale token: the notice names token 7, the node row moved to 8.
	// The publisher itself refuses a mismatched token, so publish with the
	// live token and then move the node to a newer incarnation.
	root1, node1 := seedDelegation(t, conn, owner, "call-bbridge-stale", child, 7)
	commitMarkerNotice(t, conn, owner, node1, child, 7)
	_, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET token = 8 WHERE node_id = ?1`, node1)
	require.NoError(t, err)
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "hi")
	require.ErrorIs(t, err, ErrBatchAgentStaleGeneration, "a stale-token notice never delivers")
	require.Zero(t, runner.count(), "no delivery through a stale fence")
	require.NotEmpty(t, root1)

	// Terminal state: token matches but the incarnation already settled.
	// Single-column state UPDATE on a store-reserved row: 'completed' is a
	// CHECK-valid state, so the row stays constraint-valid.
	_, node2 := seedDelegation(t, conn, owner, "call-bbridge-done", child, 9)
	commitMarkerNotice(t, conn, owner, node2, child, 9)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?1`, node2)
	require.NoError(t, err)
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "hi")
	require.ErrorIs(t, err, ErrBatchAgentStaleGeneration, "a terminal incarnation never delivers")
	require.Zero(t, runner.count())

	// Marker naming a node that has NO batch_nodes row at all.
	// The publisher requires a persisted node, so publish for a real leaf
	// and then remove its row (foreign keys are off in this fixture).
	_, ghostNode := seedDelegation(t, conn, owner, "call-bbridge-ghost", child, 11)
	commitMarkerNotice(t, conn, owner, ghostNode, child, 11)
	_, err = conn.ExecContext(ctx, `DELETE FROM batch_nodes WHERE node_id = ?1`, ghostNode)
	require.NoError(t, err)
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "hi")
	require.ErrorIs(t, err, ErrBatchAgentStaleGeneration, "an unknown node never delivers")
	require.Zero(t, runner.count())

	// A notice naming a DIFFERENT child is not a candidate for another
	// child: the sink answers "none" (the coordinator's legacy fallback),
	// never an error.
	createSessionRow(t, q, ctx, "bridge-child-other")
	_, nodeOther := seedDelegation(t, conn, owner, "call-bbridge-other", "bridge-child-other", 12)
	commitMarkerNotice(t, conn, owner, nodeOther, "bridge-child-other", 12)
	_, err = sink.DeliverHeldAnswer(ctx, owner, "nobody-else", "hi")
	require.ErrorIs(t, err, ErrBatchHeldAnswerNone)
	require.Zero(t, runner.count())
}

// TestBatchInjectBridge_DeterministicRunnerRejections: a fenced-in
// candidate delivers depth+text in the production wire shape, every
// deterministic Inject rejection propagates named with the node, and a
// CLOSED sink answers jobbatch.ErrReleased-shaped — never a legacy miss.
func TestBatchInjectBridge_DeterministicRunnerRejections(t *testing.T) {
	_, conn, _ := newTestBatchStore(t)
	q := db.New(conn)
	ctx := context.Background()
	const owner = "bridge-owner-2"
	const child = "bridge-child-2"
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	// Live fenced-in candidate: token 42 on both the row and the marker.
	_, node := seedDelegation(t, conn, owner, "call-bbridge-live", child, 42)
	commitMarkerNotice(t, conn, owner, node, child, 42)
	runner := &bridgeFakeRunner{}
	sink, serr := NewBatchHeldAnswerSink(conn, runner)
	require.NoError(t, serr)

	// Success: the wire message carries the marker's depth and the
	// JSON-encoded answer text (batchAgentAskQuestionTool unmarshals it).
	fact, err := sink.DeliverHeldAnswer(ctx, owner, child, "yes, proceed")
	require.NoError(t, err)
	require.Equal(t, BatchHeldQuestionFact{Owner: owner, NodeID: node, Token: 42, Depth: 1}, fact)
	require.Equal(t, 1, runner.count())
	var msg batchAgentInjectMessage
	require.NoError(t, json.Unmarshal(runner.injects[0].msg, &msg))
	require.Equal(t, 1, msg.Depth, "the answer addresses the held depth from the marker")
	require.Equal(t, jobbatch.ID(node), runner.injects[0].id)
	var answered string
	require.NoError(t, json.Unmarshal(msg.Text, &answered))
	require.Equal(t, "yes, proceed", answered, "the answer text survives the wire shape")

	// Deterministic rejections propagate through the existing error values.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"stopped", ErrBatchAgentStopped},
		{"not-held", ErrBatchAgentNotHeld},
		{"stale", ErrBatchAgentStaleGeneration},
		{"released", jobbatch.ErrReleased},
	} {
		runner.mu.Lock()
		runner.err = tc.err
		runner.mu.Unlock()
		_, err := sink.DeliverHeldAnswer(ctx, owner, child, "late")
		require.ErrorIs(t, err, tc.err, "%s must surface deterministically", tc.name)
		require.ErrorContains(t, err, node, "%s names the node", tc.name)
	}

	// A closed sink (the shutdown path) refuses deterministically: the
	// ErrReleased shape, NOT the legacy "none" miss.
	sink.Close()
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "late")
	require.ErrorIs(t, err, jobbatch.ErrReleased, "a closed sink fails like a released execution")
	require.NotErrorIs(t, err, ErrBatchHeldAnswerNone)
	runner.mu.Lock()
	runner.err = nil
	runner.mu.Unlock()
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "late")
	require.ErrorIs(t, err, jobbatch.ErrReleased, "close wins even with a healthy runner")
	require.Equal(t, 5, runner.count(), "no Inject after Close")
}

// TestBatchInjectBridge_NoHeldQuestionLegacyPath: no committed held
// question is a bridge MISS — the legacy InjectMessage texts are returned
// unchanged, InjectMessage delivered EXACTLY ONCE, and Runner.Inject is
// never called.
func TestBatchInjectBridge_NoHeldQuestionLegacyPath(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "bridge-legacy")
	_, conn, _ := newTestBatchStore(t)
	runner := &bridgeFakeRunner{}
	sink, serr := NewBatchHeldAnswerSink(conn, runner)
	require.NoError(t, serr)
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })
	// The counting spy rides the child's driver slot, so BOTH the busy
	// check and the legacy InjectMessage route through it (task #1054
	// routing) — its counter is the exactly-once delivery oracle.
	spy := &bridgeCountingAgent{}
	coord.subAgentDrivers.register(child, subAgentDriver{agent: spy, parentSessionID: parent})

	out, err := coord.InjectAgent(t.Context(), parent, child, "hello", false)
	require.NoError(t, err)
	require.Equal(t, "Message saved to the sub-agent's session; it is idle, so nothing runs until you call agent with resume_session_id=\""+child+"\".", out,
		"the legacy idle text is byte-identical")
	require.Equal(t, 1, spy.injectCount(), "InjectMessage ran EXACTLY ONCE")
	require.Zero(t, runner.count(), "Runner.Inject is never called on a miss")

	// Busy child: the legacy running-turn text, still exactly one delivery.
	spy.setBusy(true)
	out, err = coord.InjectAgent(t.Context(), parent, child, "hello again", false)
	require.NoError(t, err)
	require.Equal(t, "Message delivered into the sub-agent's running turn; it merges at the next step boundary.", out,
		"the legacy busy text is byte-identical")
	require.Equal(t, 2, spy.injectCount(), "the busy inject delivered exactly once more")
	require.Zero(t, runner.count())
}

// TestBatchInjectBridge_InjectAgentSurfacesRejection: a deterministic sink
// error routed through coordinator.InjectAgent reaches the CALLER naming
// the reason — the mailbox is never used as a fallback for it.
func TestBatchInjectBridge_InjectAgentSurfacesRejection(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "bridge-reject")
	spy := &bridgeCountingAgent{}
	coord.subAgentDrivers.register(child, subAgentDriver{agent: spy, parentSessionID: parent})
	sink := bridgeErrorSink{err: fmt.Errorf("batch node bridge-n-x: %w", ErrBatchAgentStopped)}
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })

	_, err := coord.InjectAgent(t.Context(), parent, child, "late answer", false)
	require.ErrorIs(t, err, ErrBatchAgentStopped,
		"the deterministic rejection reaches the caller")
	require.ErrorContains(t, err, "bridge-n-x", "the rejection names the node")
	require.ErrorContains(t, err, "batch agent: incarnation is stopped",
		"the rejection names the reason")
	require.Zero(t, spy.injectCount(), "the mailbox is never a fallback for a bridge error")
}

// TestBatchInjectBridge_SinkAbsentLegacyPath: no sink (flag-OFF shape) and
// a shutdown-cleared sink both take the legacy path.
func TestBatchInjectBridge_SinkAbsentLegacyPath(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parent, child := controlChild(t, env, "bridge-absent")

	out, err := coord.InjectAgent(t.Context(), parent, child, "hello", false)
	require.NoError(t, err)
	require.Contains(t, out, "Message saved to the sub-agent's session",
		"a coordinator without a sink keeps the legacy path")

	// A bound success-shaped sink answers through the bridge...
	SetCoordinatorBatchHeldAnswerSink(coord, bridgeSuccessSink{fact: BatchHeldQuestionFact{
		Owner: parent, NodeID: "bridge-n-x", Token: 3, Depth: 1,
	}})
	out, err = coord.InjectAgent(t.Context(), parent, child, "hello", false)
	require.NoError(t, err)
	require.Contains(t, out, "SAME batch execution", "a bound sink delivers")
	require.Contains(t, out, "bridge-n-x")

	// ...and clearing it (the shutdown path) restores the legacy path.
	SetCoordinatorBatchHeldAnswerSink(coord, nil)
	out, err = coord.InjectAgent(t.Context(), parent, child, "hello", false)
	require.NoError(t, err)
	require.Contains(t, out, "Message saved to the sub-agent's session",
		"a cleared sink keeps the legacy path")
}
