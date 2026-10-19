// X2-slot helpers (delegation/scope residuals of the flag-ON test port,
// stage 3): dual-mode fixture helpers owned by the X2 test files
// (turn_arbiter_shadow_test.go, work_ledger_delegation_test.go). Everything
// here selects its authority through batchFlagEnabled() and the foundation
// dual-mode helpers (batch_fixture_port_test.go) and never touches
// RUSH_BATCH_RUNTIME.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// x2ReleaseBatchDelegation projects the LIVE parent->child delegation that
// childDelegationBase already seeded in batch_nodes (b1SeedDelegationChildLink
// -> seedBatchDelegation, state "running") to its terminal "completed" state
// -- the ON twin of the OFF branch's store.Transition(..., "completed", ...).
// It must be used instead of a SECOND seedBatchDelegation call for the same
// (owner, tool call id): a matching live reservation attaches to the
// existing root, and re-announcing that already-announced root loses the
// announcement election ("batch announcement election lost"). Batch mode
// only: a call with the flag OFF is a fixture bug.
func x2ReleaseBatchDelegation(t *testing.T, conn *sql.DB, owner, callID string) {
	t.Helper()
	ctx := context.Background()
	var rootID string
	err := conn.QueryRowContext(ctx, `SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`, owner, callID).Scan(&rootID)
	require.NoError(t, err)
	store := session.NewBatchStore(conn)
	snap, err := store.Read(ctx, owner, rootID)
	require.NoError(t, err)
	updates := make([]db.ProjectBatchNodeParams, 0, len(snap.Nodes))
	for _, node := range snap.Nodes {
		if node.NodeID != rootID && node.RootID != rootID {
			continue
		}
		updates = append(updates, db.ProjectBatchNodeParams{
			NodeID: node.NodeID, State: "completed", OutcomeState: "completed",
			OutcomeSummary: "fixture delegation released", ControlState: "none",
		})
	}
	require.NotEmpty(t, updates, "the seeded delegation root must have nodes to release")
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: rootID, Updates: updates}},
	}))
}

// x2WriteParkedChildTurn records a finished assistant turn on a session
// through an EXPLICIT message service -- the writeParkedChildTurn variant
// for fixtures whose refresh reads (coordinator.messages) and whose batch
// reaction writes must see the SAME database (the coordinator's bound batch
// conn), not the testEnv one.
func x2WriteParkedChildTurn(t *testing.T, messages message.Service, sessionID, text string, reason message.FinishReason) {
	t.Helper()
	ctx := t.Context()
	row, err := messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	require.NoError(t, err)
	row.AddFinish(reason, "", "")
	require.NoError(t, messages.Update(ctx, row))
}
