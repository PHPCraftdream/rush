package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchDeliveryTestSettle projects a completed settled root with a persisted
// envelope, mimicking the R11B commit hook's settled write.
func batchDeliveryTestSettle(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot, seq int64) {
	t.Helper()
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	v := batchStoreTestUpdate(snap.Root.RootID, "completed")
	v.OutcomeState = "completed"
	v.ResponseEnvelope = batchStoreTestString(`{"state":"completed","code":"","summary":"done","details":{"answer":42}}`)
	v.ResponseEnvelopeRef = batchStoreTestString(snap.Root.RootID + "#7")
	p.Updates = append(p.Updates, v)
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: seq, Roots: []BatchRootProjection{p}}))
}

// Envelope loading for sync consumption: the settled jobbatch envelope is
// readable from the store; unsettled roots report ok=false, never an error.
func TestBatchDeliveryRootEnvelope(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	_, ok, err := s.BatchDeliveryRootEnvelope(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.False(t, ok, "unsettled root has no envelope")
	batchDeliveryTestSettle(t, s, ctx, snap, 2)
	env, ok, err := s.BatchDeliveryRootEnvelope(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "completed", env.State)
	require.Equal(t, "done", env.Summary)
	require.JSONEq(t, `{"answer":42}`, string(env.Details))
	require.Equal(t, snap.Root.RootID+"#7", env.Ref)
}

// Inline election via the typed wrapper: one winner, the loser gets won=false
// and inserts no history.
func TestBatchDeliveryElectInlineOnce(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchOutboxReserve(t, s, ctx, 1)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	params := batchOutboxText("inline-answer")
	a, won, err := s.BatchDeliveryElectInline(ctx, m, "owner", snap.Root.RootID, params)
	require.NoError(t, err)
	require.True(t, won)
	require.NotEmpty(t, a.Msg.ID)
	// A second consumer loses: no duplicate delivery, no extra history.
	_, won, err = s.BatchDeliveryElectInline(ctx, m, "owner", snap.Root.RootID, params)
	require.NoError(t, err)
	require.False(t, won, "the second inline election must lose")
}

// Started election via the typed wrapper: one winner, losers get won=false.
func TestBatchDeliveryElectStartedOnce(t *testing.T) {
	t.Parallel()
	s, _, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(s.writer))
	snap := batchOutboxReserve(t, s, ctx, 1)
	_, won, err := s.BatchDeliveryElectStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("started"))
	require.NoError(t, err)
	require.True(t, won)
	_, won, err = s.BatchDeliveryElectStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("started"))
	require.NoError(t, err)
	require.False(t, won)
}
