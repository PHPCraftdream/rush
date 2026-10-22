package session

// PD11C (c): the fused batch transactions write the history message through
// the binder-backed cached INSERT/UPDATE when the service supports
// CreateTxQ/UpdateTxQ, and the legacy bare-tx path stays byte-identical.

import (
	"context"
	"database/sql"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// pdcUUIDRe scrubs the random message UUIDs the two scenario runs mint; the
// batch node UUIDs are already scrubbed via pdaEquivState.scrubList.
var pdcUUIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// pdcPublishProbe counts CreatedEvent publications and asserts the "publish
// only after commit" contract: when the event goes out, the row must already
// be visible to a fresh reader. Single-test-goroutine only (BatchAnnounceStarted
// publishes synchronously), so holding *testing.T is safe.
type pdcPublishProbe struct {
	message.Service
	t       *testing.T
	conn    *sql.DB
	created atomic.Int32
}

func (p *pdcPublishProbe) PublishCreated(msg message.Message) {
	_, err := db.New(p.conn).GetMessage(context.Background(), msg.ID)
	if err != nil {
		p.t.Errorf("published before commit: message %s not visible: %v", msg.ID, err)
		return
	}
	p.created.Add(1)
	p.Service.PublishCreated(msg)
}

// pdcHideTxQuerier wraps a message.Service and deliberately does NOT
// implement pdcTxQuerierMessages: the embedded interface's method set has no
// CreateTxQ/UpdateTxQ, so the batch call sites fall back to the legacy
// bare-tx CreateTx/UpdateTx.
type pdcHideTxQuerier struct {
	message.Service
}

// pdcPublishProbeQ adds the optional CreateTxQ/UpdateTxQ capability on top of
// the publish probe by forwarding to the real service, and counts how often
// the batch call site really took the cached-Querier path. The embedded probe
// alone has no such methods, so only this type satisfies pdcTxQuerierMessages.
type pdcPublishProbeQ struct {
	*pdcPublishProbe
	inner  pdcTxQuerierMessages
	qCalls atomic.Int32
}

func (p *pdcPublishProbeQ) CreateTxQ(ctx context.Context, q db.Querier, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	p.qCalls.Add(1)
	return p.inner.CreateTxQ(ctx, q, sessionID, params)
}

func (p *pdcPublishProbeQ) UpdateTxQ(ctx context.Context, q db.Querier, msg message.Message) (func(), error) {
	p.qCalls.Add(1)
	return p.inner.UpdateTxQ(ctx, q, msg)
}

// pdcRunBatchAnnounceScenario runs one BatchAnnounceStarted on a fresh
// database, with (cachedMessageWrite) or without (legacy bare-tx) the
// CreateTxQ capability, and returns the scrubbed messages dump, the
// publication count and the number of CreateTxQ/UpdateTxQ calls observed.
func pdcRunBatchAnnounceScenario(t *testing.T, cachedMessageWrite bool) ([]string, int32, int32) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	s := NewBatchStore(conn)
	snap := pdaEquivSettleOne(t, ctx, s, conn)
	require.NoError(t, db.PdaBatchEnsure(ctx, conn))
	base := message.NewService(db.New(conn))
	probe := &pdcPublishProbe{t: t, conn: conn}
	var messages message.Service
	var qProbe *pdcPublishProbeQ
	if cachedMessageWrite {
		inner, ok := base.(pdcTxQuerierMessages)
		require.True(t, ok, "message.NewService must expose CreateTxQ/UpdateTxQ")
		probe.Service = base
		qProbe = &pdcPublishProbeQ{pdcPublishProbe: probe, inner: inner}
		messages = qProbe
	} else {
		probe.Service = pdcHideTxQuerier{Service: base}
		messages = probe
	}
	_, err = s.BatchAnnounceStarted(ctx, messages, "owner", snap.root, batchOutboxText("started"))
	require.NoError(t, err)
	require.Equal(t, int32(1), probe.created.Load(), "exactly one CreatedEvent, published after commit")
	var qCalls int32
	if qProbe != nil {
		qCalls = qProbe.qCalls.Load()
	}
	var dump []string
	for _, line := range pdaEquivDump(t, ctx, conn, pdaEquivMessagesDump, nil) {
		dump = append(dump, pdaEquivScrub(pdcUUIDRe.ReplaceAllString(line, "MSGID"), nil))
	}
	return dump, probe.created.Load(), qCalls
}

func TestPdcBatchTxMessagePathEquivalence(t *testing.T) {
	t.Parallel()
	cachedDumps, cachedCreated, cachedQCalls := pdcRunBatchAnnounceScenario(t, true)
	legacyDumps, legacyCreated, legacyQCalls := pdcRunBatchAnnounceScenario(t, false)
	require.Equal(t, int32(1), cachedQCalls, "the cached scenario must really take the CreateTxQ path")
	require.Equal(t, int32(0), legacyQCalls, "the legacy scenario must stay on the bare-tx CreateTx")
	require.Equal(t, legacyDumps, cachedDumps, "cached message write must leave identical message rows")
	require.Equal(t, legacyCreated, cachedCreated)
}

// PD11C test 4: the binder performs at most ONE tx.StmtContext binding per
// distinct statement per transaction — counted through the StmtContext seam
// (PdbBatchTxBindCallCount), not by distinct map keys, so a per-use re-bind
// would show here.
func TestPdbBatchTxBindCallCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	s := NewBatchStore(conn)
	st := pdaEquivReserve(t, ctx, s)
	require.NoError(t, db.PdaBatchEnsure(ctx, conn))
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	q := db.PdaBatchTxQueries(ctx, conn, tx)
	require.Equal(t, 0, db.PdbBatchTxBindCallCount(q))
	// The registered fence statement, executed twice: one binding.
	var fenced int
	require.NoError(t, q.PdbBatchRawQueryRow(ctx, pdcBatchRootFenceSQL, "owner", "host", st.root, st.root, "").Scan(&fenced))
	require.Equal(t, 1, db.PdbBatchTxBindCallCount(q))
	require.NoError(t, q.PdbBatchRawQueryRow(ctx, pdcBatchRootFenceSQL, "owner", "host", st.root, st.root, "").Scan(&fenced))
	require.Equal(t, 1, db.PdbBatchTxBindCallCount(q), "second execution must reuse the memoised binding")
	// A generated message statement executed twice: one binding.
	_, err = q.CreateMessage(ctx, db.CreateMessageParams{ID: "b1", SessionID: "owner", Role: "assistant", Parts: "[]", Model: batchStoreTestString("m")})
	require.NoError(t, err)
	_, err = q.CreateMessage(ctx, db.CreateMessageParams{ID: "b2", SessionID: "owner", Role: "assistant", Parts: "[]", Model: batchStoreTestString("m")})
	require.NoError(t, err)
	require.Equal(t, 2, db.PdbBatchTxBindCallCount(q), "message INSERT must bind once, not per execution")
	// An unregistered query never binds (raw fallback).
	var answer int
	require.NoError(t, q.PdbBatchRawQueryRow(ctx, "SELECT 41+1").Scan(&answer))
	require.Equal(t, 42, answer)
	require.Equal(t, 2, db.PdbBatchTxBindCallCount(q), "unregistered query must fall through without binding")
	require.NoError(t, tx.Rollback())
	// A NEW transaction starts from zero bindings (per-tx lifecycle).
	tx2, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx2.Rollback() //nolint:errcheck
	q2 := db.PdaBatchTxQueries(ctx, conn, tx2)
	require.Equal(t, 0, db.PdbBatchTxBindCallCount(q2))
}
