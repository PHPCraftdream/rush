package server

// R12-C terminal-root completion delivery (handlers_batch.go): each
// finished root's outcome — state + result-summary columns — is carried in
// the structured snapshot keyed by root_id, exactly once, and a reload
// re-reads it from the structured store. No prose is parsed anywhere.
//
// Revert-check: keying completions by tool_call_id instead of root_id
// collapses the two reused-id roots and fails
// TestBatchWorkSnapshot_RootCompletionKeyedByRootID; dropping the history
// read (or deriving the summary from message prose) fails
// TestBatchWorkSnapshot_ReloadReconstructsRootCompletion; dropping the
// failed-history downgrade without keeping the read entries fails
// TestBatchWorkSnapshot_FailedHistoryKeepsCompletions.

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func batchHistoryRoot(rootID, state, toolCall, code, summary, ref string) db.BatchNode {
	n := batchNode(rootID, rootID, "s1", state, toolCall, 0)
	n.Kind = "batch"
	n.OutcomeCode = code
	n.OutcomeSummary = summary
	n.ResponseEnvelopeRef = sql.NullString{String: ref, Valid: ref != ""}
	n.CreatedAt = time.Unix(1700000000, 0).Unix()
	return n
}

func TestBatchWorkSnapshot_RootCompletionKeyedByRootID(t *testing.T) {
	live := batchNode("r-live", "r-live", "s1", "running", "tc-live", 0)
	done := batchHistoryRoot("r-done", "completed", "tc-shared", "ok", "3 tasks finished", "env-1")
	failed := batchHistoryRoot("r-fail", "failed", "tc-shared", "err", "1 task failed", "env-2")
	duplicate := done // the same root read twice must still deliver once
	src := &fakeBatchSource{rows: []db.BatchNode{live}, history: []db.BatchNode{done, failed, duplicate}}
	withBatchSeams(t, src, probeAlive, nil)

	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.True(t, snap.Complete)
	var rootComps []BatchCompletionWire
	for _, c := range snap.Completions {
		if c.NodeID == c.RootID {
			rootComps = append(rootComps, c)
		}
	}
	// Two reused tool_call_ids are two distinct incarnations: keyed by
	// root_id, each delivered exactly once.
	require.Len(t, rootComps, 2, "two reused tool_call_ids must stay distinct root completions")
	byRoot := map[string]BatchCompletionWire{}
	for _, c := range rootComps {
		byRoot[c.RootID] = c
	}
	require.Equal(t, BatchCompletionWire{
		RootID: "r-done", NodeID: "r-done", OutcomeCode: "ok", OutcomeRef: "env-1",
		State: "completed", OutcomeSummary: "3 tasks finished",
	}, byRoot["r-done"])
	require.Equal(t, "failed", byRoot["r-fail"].State)
	require.Equal(t, "env-2", byRoot["r-fail"].OutcomeRef)
}

func TestBatchWorkSnapshot_ReloadReconstructsRootCompletion(t *testing.T) {
	done := batchHistoryRoot("r-done", "completed", "tc-root", "ok", "structured summary", "env-7")
	src := &fakeBatchSource{history: []db.BatchNode{done}}
	withBatchSeams(t, src, probeAlive, nil)

	// A browser reload answers get_session_batch_work with a FRESH read of
	// the structured store: the completion must reconstruct identically,
	// straight from the columns — no message prose involved.
	first := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	second := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.Len(t, first.Completions, 1)
	require.Equal(t, first.Completions, second.Completions)
	require.Equal(t, "structured summary", second.Completions[0].OutcomeSummary,
		"the summary must be the node column, not parsed prose")
	require.Equal(t, "completed", second.Completions[0].State)
	raw, err := json.Marshal(second.Completions)
	require.NoError(t, err)
	require.Contains(t, string(raw), "structured summary")
}

func TestBatchWorkSnapshot_FailedHistoryKeepsCompletions(t *testing.T) {
	live := batchNode("r-live", "r-live", "s1", "running", "tc-live", 0)
	done := batchNode("r-live-d", "r-live", "s1", "completed", "tc-d", 1)
	done.OutcomeCode = "ok"
	src := &fakeBatchSource{rows: []db.BatchNode{live, done}, failHistory: true}
	withBatchSeams(t, src, probeAlive, nil)

	// A failed root-history read downgrades the scope but must not drop the
	// completions that WERE read (terminal nodes inside the live root).
	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.False(t, snap.Complete, "a failed history read must make the scope incomplete")
	require.NotEmpty(t, snap.Completions, "read completions are kept, never dropped")
	require.Equal(t, "r-live-d", snap.Completions[0].NodeID)
}
