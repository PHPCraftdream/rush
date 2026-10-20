// LiveHeldQuestions' additive identity: the fenced rows must name their
// marker's node and child session, because the CLI active-work exclusion
// binds the commit decision to the CURRENT incarnation through them.
// Flag-independent: nodeBatchCutoverFacts.LiveHeldQuestions never reads
// RUSH_BATCH_RUNTIME.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchLiveHeldQuestionsCarryNodeAndChildIdentity(t *testing.T) {
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(conn), batchFixtureLiveReader(conn, dataDir), conn)
	ctx := context.Background()
	const owner = "sess-bq-held-owner"
	const child = "sess-bq-held-child"
	_, nodeID := seedDelegation(t, conn, owner, "call-bq-id", child, 21)
	commitMarkerNotice(t, conn, owner, nodeID, child, 21)

	held, err := facts.LiveHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Len(t, held, 1)
	// Revert-check: dropping the NodeID/ChildSessionID population reverts
	// LiveHeldQuestions to id+kind and both identity assertions fail.
	require.Equal(t, nodeID, held[0].NodeID)
	require.Equal(t, child, held[0].ChildSessionID)

	// A legacy-frame, marker-less child_question notice is never counted as a
	// held batch question, even when it names some other session.
	require.NoError(t, store.InsertSessionNotice(ctx, owner, session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session someone-else): q", true, ""))
	held, err = facts.LiveHeldQuestions(ctx, owner)
	require.NoError(t, err)
	// Revert-check: counting marker-less rows breaks the incarnation fence
	// and the final Len assertion fails.
	require.Len(t, held, 1)
}
