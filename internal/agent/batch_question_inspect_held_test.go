// inspect_agent must not mask a held batch question as work (task 36):
// a batch ask_question parks the child's run INSIDE a tool call, so the
// driver-aware busy check alone reported "running" and the parent never
// learned it had to answer. Only the fenced actionable route (CURRENT live
// incarnation, exact token, exact child) promotes the status to
// awaiting_answer together with the fenced held_question identity; an
// answered/stale/terminal/foreign-scope notice, a capability-absent
// coordinator, and an ordinary executing child keep the legacy statuses;
// the legacy between-turns question stop keeps HeldQuestion nil (the tools
// contract documents held_question as batch-only).
//
// Mutations caught:
//   - restoring the OLD priority (busy first): subtest "held and busy"
//     fails (a confirmed held ask reads "running" again — the masking
//     defect);
//   - any reacted filter in the route reader: subtest "reacted but
//     unanswered" fails (the production pull+reaction state must stay
//     actionable);
//   - losing the close-on-acceptance interplay: subtest "answered clears"
//     fails (the voided notice must stop routing);
//   - losing the incarnation fence: subtest "stale generation" fails;
//   - losing the child filter: subtest "foreign-scope notice" fails;
//   - fabricating awaiting_answer without a route: subtests "ordinary
//     busy without notices" and "capability absent" fail;
//   - reporting held_question on the legacy question stop: subtest
//     "legacy between-turns question stop" fails.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// heldInspectFixture builds a fresh coordinator with a parent/child pair
// (child.ParentSessionID == parent) and, when busy, registers a busy driver
// spy so IsSessionBusy(child) reports a live turn. The returned conn is the
// batch facts' backing store (delegation and notice seeds go through it).
func heldInspectFixture(t *testing.T, busy bool) (coord *coordinator, owner, child string, conn *sql.DB) {
	t.Helper()
	coord, env := newControlCoordinator(t)
	owner, child = controlChild(t, env, "held-inspect")
	if busy {
		spy := &bridgeCountingAgent{}
		spy.setBusy(true)
		coord.subAgentDrivers.register(child, subAgentDriver{agent: spy, parentSessionID: owner})
	}
	return coord, owner, child, env.conn
}

// inspectHeld runs InspectAgent with the shared sanity gate: no error and
// the correct child id echoed back.
func inspectHeld(t *testing.T, coord *coordinator, owner, child string) (status string, held bool) {
	t.Helper()
	insp, err := coord.InspectAgent(context.Background(), owner, child)
	require.NoError(t, err)
	require.Equal(t, child, insp.ChildSessionID, "the inspection names the inspected child")
	return insp.Status, insp.HeldQuestion != nil
}

func TestInspectAgentHeldQuestionBeatsBusyAndClearsOnAnswer(t *testing.T) {
	ctx := context.Background()

	t.Run("held and busy", func(t *testing.T) {
		coord, owner, child, conn := heldInspectFixture(t, true)
		_, nodeID := seedDelegation(t, conn, owner, "call-insp-held", child, 7)
		commitMarkerNotice(t, conn, owner, nodeID, child, 7)
		insp, err := coord.InspectAgent(ctx, owner, child)
		require.NoError(t, err)
		require.Equal(t, child, insp.ChildSessionID)
		require.Equal(t, "awaiting_answer", insp.Status,
			"a confirmed held ask outranks the generic busy state")
		require.NotNil(t, insp.HeldQuestion, "the fenced identity is reported")
		require.Equal(t, nodeID, insp.HeldQuestion.NodeID)
		require.Equal(t, int64(7), insp.HeldQuestion.Token)
		require.Equal(t, 1, insp.HeldQuestion.Depth)
		require.Equal(t, int64(1), insp.HeldQuestion.Sequence)
	})

	t.Run("reacted but unanswered", func(t *testing.T) {
		coord, owner, child, conn := heldInspectFixture(t, true)
		_, nodeID := seedDelegation(t, conn, owner, "call-insp-reacted", child, 7)
		commitMarkerNotice(t, conn, owner, nodeID, child, 7)
		// The incident's pull+reaction state: reacted is NOT answered.
		_, err := conn.ExecContext(ctx,
			`UPDATE session_notices SET delivery = 'done', reacted = 1 WHERE owner = ?1 AND kind = ?2`,
			owner, session.NoticeKindChildQuestion)
		require.NoError(t, err)
		insp, err := coord.InspectAgent(ctx, owner, child)
		require.NoError(t, err)
		require.Equal(t, child, insp.ChildSessionID)
		require.Equal(t, "awaiting_answer", insp.Status,
			"reacted must never make an unanswered held ask invisible")
		require.NotNil(t, insp.HeldQuestion)
		require.Equal(t, nodeID, insp.HeldQuestion.NodeID)
	})

	t.Run("answered clears", func(t *testing.T) {
		coord, owner, child, conn := heldInspectFixture(t, true)
		_, nodeID := seedDelegation(t, conn, owner, "call-insp-answered", child, 7)
		commitMarkerNotice(t, conn, owner, nodeID, child, 7)
		// The close-on-acceptance shape: the sink voids the notice rows.
		_, err := conn.ExecContext(ctx,
			`UPDATE session_notices SET delivery = 'void', wake = 0 WHERE owner = ?1 AND kind = ?2`,
			owner, session.NoticeKindChildQuestion)
		require.NoError(t, err)
		status, held := inspectHeld(t, coord, owner, child)
		require.Equal(t, "running", status, "an answered question no longer promotes the busy child")
		require.False(t, held, "an answered question reports no held identity")
	})

	t.Run("stale generation", func(t *testing.T) {
		coord, owner, child, conn := heldInspectFixture(t, true)
		_, nodeID := seedDelegation(t, conn, owner, "call-insp-stale", child, 7)
		commitMarkerNotice(t, conn, owner, nodeID, child, 7)
		// A new incarnation: the node token moved, the marker's is stale.
		_, err := conn.ExecContext(ctx,
			`UPDATE batch_nodes SET token = token + 1 WHERE node_id = ?1 AND owner_session_id = ?2`,
			nodeID, owner)
		require.NoError(t, err)
		status, held := inspectHeld(t, coord, owner, child)
		require.Equal(t, "running", status, "a stale marker token never promotes the busy child")
		require.False(t, held)
	})

	t.Run("foreign-scope notice", func(t *testing.T) {
		coord, env := newControlCoordinator(t)
		owner, child := controlChild(t, env, "held-inspect-foreign")
		otherChild, err := env.sessions.CreateTaskSession(ctx, owner+"$$c-other", owner, "other child")
		require.NoError(t, err)
		spy := &bridgeCountingAgent{}
		spy.setBusy(true)
		coord.subAgentDrivers.register(child, subAgentDriver{agent: spy, parentSessionID: owner})
		conn := env.conn
		_, otherNode := seedDelegation(t, conn, owner, "call-insp-foreign", otherChild.ID, 7)
		commitMarkerNotice(t, conn, owner, otherNode, otherChild.ID, 7)
		status, held := inspectHeld(t, coord, owner, child)
		require.Equal(t, "running", status, "another child's question never promotes this child")
		require.False(t, held)
	})

	t.Run("ordinary busy without notices", func(t *testing.T) {
		coord, owner, child, _ := heldInspectFixture(t, true)
		status, held := inspectHeld(t, coord, owner, child)
		require.Equal(t, "running", status, "the check never fabricates awaiting_answer")
		require.False(t, held)
	})

	t.Run("legacy between-turns question stop", func(t *testing.T) {
		coord, owner, child, _ := heldInspectFixture(t, false)
		seedQuestionStop(t, coord, child)
		insp, err := coord.InspectAgent(ctx, owner, child)
		require.NoError(t, err)
		require.Equal(t, child, insp.ChildSessionID)
		require.Equal(t, "awaiting_answer", insp.Status, "the legacy question stop still reports awaiting_answer")
		require.Nil(t, insp.HeldQuestion, "held_question is batch-only; the legacy stop stays identity-less")
	})

	t.Run("capability absent", func(t *testing.T) {
		// A coordinator whose facts never bind: coordinatorBatchCutoverFacts
		// resolves unknownBatchCutoverFacts, which lacks the
		// BatchHeldQuestionSource capability, so the check is inert and the
		// legacy behaviour is byte-identical. Drop the binding newControlCoordinator
		// installed.
		coord, env := newControlCoordinator(t)
		coordinatorBatchCutoverFactsBindings.Delete(coord)
		owner, child := controlChild(t, env, "held-inspect-legacy")
		spy := &bridgeCountingAgent{}
		spy.setBusy(true)
		coord.subAgentDrivers.register(child, subAgentDriver{agent: spy, parentSessionID: owner})
		insp, err := coord.InspectAgent(ctx, owner, child)
		require.NoError(t, err)
		require.Equal(t, child, insp.ChildSessionID)
		require.Equal(t, "running", insp.Status)
		require.Nil(t, insp.HeldQuestion)
	})
}
