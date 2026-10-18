// R12-C instrumentation, flag ON, round 4 (P2-1/P2-2): the LIST annotation
// handler's flag-ON facts must keep their honest derived cost over N listed
// sessions — handleListSessions → annotateSessionActivity →
// App.SessionActivityBatch (internal/server/handlers_sessions.go) reads, per
// poll:
//
//   - sessionDriverFacts via activityConn() (app_session_activity.go:96,
//     :292-300): ONE ListSessionDrivers statement;
//   - batchCutoverDebtOwners (app_session_activity.go:103-104 →
//     app_batch_cutover_facts.go:63-69 → session/batch_reaction.go:
//     BatchReactionDebtOwners): ONE shared read-only tx, then PER OWNER a
//     batchDebtSnapshot = 2 statements (batchOutboxDebtNodesVoidAware raw
//     QueryContext at batch_reaction.go:45-50 + BatchOutboxDebtNotices).
//     The per-owner loop is PRODUCTION DESIGN (batch_reaction.go:163-165),
//     so the honest relation is LINEAR in N with slope 2, not a constant;
//   - batchCutoverLiveWork (app_batch_cutover_facts.go:78-107 →
//     session/batch_read_live.go:49-79): BFS-chunked — one
//     BatchLiveNodesForOwners chunk for all N owners at depth 0 (chunk size
//     500), plus one chunk per further depth of delegation children;
//   - sessionEndReasons (app_session_activity.go:153-154 →
//     ListSessionEndReasonsForIDs): ONE statement;
//   - wake schedules: OpenOnceWakeSchedulesForOwners is NOT called — the
//     hand-built App has no wake-schedule store (WakeScheduleStore() reads
//     app.wakeScheduleStore, nil on a zero-value App, and
//     app_session_activity.go:141-144 guards on nil);
//   - Sessions.List (handlers_sessions.go → session_read.go ListSessions):
//     ONE statement, same pool;
//   - lock facts and host probes are FILE reads (InspectSessionLockFact,
//     ProbeHostShared) — never SQL.
//
// The seam: a full app.New App cannot be used because readDB/dbConns
// (unexported, set by app.New) shadow the exported DB accessor, but a
// HAND-BUILT &app.App{} literal leaves both nil, so activityConn() falls
// back to app.DB() (app_session_activity.go:293-295), batchCutoverWriter
// falls back to app.DB() (app_batch_cutover_facts.go:38-44) and
// batchCutoverReader falls back to the writer (:53-58). Every annotation-path
// statement therefore crosses the ONE counting pool wired below. This is a
// DIFFERENT surface from the flat snapshot reader instrumented by
// descendant_work_batch_on_test.go (the batchSourceFor seam).
//
// Mutations this test catches:
//   - the annotation replaced by per-session SessionActivity reads → the
//     live-walk chunks and driver statements multiply by N → the exact
//     constant C fails;
//   - a per-owner transaction instead of the ONE shared tx → txs == N fails;
//   - lost BFS chunking → the live-walk statements become N chunks → C fails;
//   - broken instrumentation (the counting driver never installed, or reads
//     bypassing the pool) → the >0 precondition fails at 0.
package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

// annotationCounters is the shared statement/transaction counter pair.
type annotationCounters struct {
	stmts, txs atomic.Int64
}

// countingDriver wraps the real modernc sqlite driver (sqlite.Driver, the
// type the package registers under "sqlite"; a zero value is safe — its Open
// ranges over the (here nil) UDF/collation maps, which iterate zero times)
// and counts every statement execution and transaction begun on its
// connections.
type countingDriver struct {
	inner driver.Driver
	c     *annotationCounters
}

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{inner: conn, c: d.c}, nil
}

// countingConnector implements driver.Connector so sql.OpenDB can build a
// *sql.DB straight over the counting driver (no global registration).
type countingConnector struct {
	d   *countingDriver
	dsn string
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	return c.d.Open(c.dsn)
}

func (c countingConnector) Driver() driver.Driver { return c.d }

// countingConn forwards the context-carrying interfaces the modernc conn
// implements (ConnPrepareContext, ConnBeginTx, ExecerContext,
// QueryerContext - conn.go:1150-1184) and ALSO SessionResetter and
// Validator (conn.go:810, :820), each only when the inner conn has it, so
// pool behaviour matches the unwrapped driver. Rows are returned unwrapped:
// counting is per-statement-call, not per-row.
type countingConn struct {
	inner driver.Conn
	c     *annotationCounters
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	st, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return countingStmt{inner: st, c: c.c}, nil
}

func (c countingConn) Close() error { return c.inner.Close() }

// Begin is the deprecated legacy entry; route it through BeginTx so the inner
// conn is only ever begun via driver.ConnBeginTx.
func (c countingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	st, err := c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return countingStmt{inner: st, c: c.c}, nil
}

func (c countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.c.txs.Add(1)
	return countingTx{inner: tx}, nil
}

func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.c.stmts.Add(1)
	return c.inner.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.c.stmts.Add(1)
	return c.inner.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// countingStmt counts every execution; NumInput/Close forward untouched.
type countingStmt struct {
	inner driver.Stmt
	c     *annotationCounters
}

func (s countingStmt) Close() error  { return s.inner.Close() }
func (s countingStmt) NumInput() int { return s.inner.NumInput() }
func (s countingStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.c.stmts.Add(1)
	return s.inner.Exec(args)
}

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.c.stmts.Add(1)
	return s.inner.Query(args)
}

func (s countingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.c.stmts.Add(1)
	return s.inner.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.c.stmts.Add(1)
	return s.inner.(driver.StmtQueryContext).QueryContext(ctx, args)
}

// countingTx forwards Commit/Rollback verbatim (the tx itself is counted at
// BeginTx/Begin time).
type countingTx struct{ inner driver.Tx }

func (t countingTx) Commit() error   { return t.inner.Commit() }
func (t countingTx) Rollback() error { return t.inner.Rollback() }

// annotationCountingPool opens the counting pool over the same rush.db file
// db.Connect migrated, with the writer DSN's pragmas mirrored from
// internal/db/connect_modernc.go openDB (the pragma values come from
// internal/db/connect.go:15-24). The PRAGMAs themselves run on the INNER
// conn during connect (modernc runs them before handing the conn back), so
// they never pass through the counting wrapper and never inflate the counts.
func annotationCountingPool(t *testing.T, c *annotationCounters, dataDir string) *sql.DB {
	t.Helper()
	params := url.Values{}
	for name, value := range map[string]string{
		"foreign_keys": "ON", "journal_mode": "WAL", "page_size": "4096",
		"temp_store": "MEMORY", "cache_size": "-8000", "synchronous": "NORMAL",
		"secure_delete": "ON", "busy_timeout": "30000",
	} {
		params.Add("_pragma", fmt.Sprintf("%s(%s)", name, value))
	}
	params.Set("_txlock", "immediate")
	dsn := fmt.Sprintf("file:%s?%s", storepath.DataDirPath(dataDir, storepath.ActiveFilename), params.Encode())
	pool := sql.OpenDB(countingConnector{d: &countingDriver{inner: new(sqlite.Driver), c: c}, dsn: dsn})
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

// reserveAnnotationRoot reserves one live batch root (kind=batch, parallel,
// running) for owner, optionally with one running agent-task child whose
// ChildSessionID names childID — the production Reserve shape.
func reserveAnnotationRoot(t *testing.T, ctx context.Context, pool *sql.DB, owner, toolCallID, childID string) {
	t.Helper()
	children := []session.BatchReservationNode{}
	if childID != "" {
		children = append(children, session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
			State: "running", ControlState: "none",
			ChildSessionID: sql.NullString{String: childID, Valid: true},
		}})
	}
	_, err := session.NewBatchStore(pool).Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "on-annotation-host", RunnerID: "on-annotation-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "running", ControlState: "none",
		}, Children: children},
	})
	require.NoError(t, err)
}

// TestHandleListSessions_AnnotationReadsBoundedInOwners pins the annotation
// path's derived relation over N listed sessions, measured through a
// counting database/sql driver wired under the &app.App{} literal seam (see
// the file doc). The hand-built App has no wake-schedule store, so the wake
// fact read is skipped (app_session_activity.go:141-144); every OTHER
// annotation-path statement crosses the counting pool.
//
// Derived constant C (flag ON, all counts = 1 statement each):
//
//	C = 1 ListSessions (db/sql/sessions.sql:94-98)
//	  + 1 ListSessionDrivers (db/sql/session_drivers.sql:34)
//	  + 2 BatchLiveNodesForOwners chunks (depth 0: all N owners, ≤500;
//	    depth 1: the delegation children — batch_read_live.go:49-79)
//	  + 1 ListSessionEndReasonsForIDs (db/sql/sessions.sql:196)
//	  = 5
//
// so the exact relation is stmts == 5 + 2*N and txs == 1 (the ONE shared
// read-only tx of BatchReactionDebtOwners, batch_reaction.go:156-165), with
// the debt slope +2/owner coming from the per-owner batchDebtSnapshot pair
// (batchOutboxDebtNodesVoidAware + BatchOutboxDebtNotices,
// batch_reaction.go:163-165 / :45-50 / :72). Host "on-annotation-host" is
// unregistered, so ProbeHostShared reads Dead; host death never settles
// batch nodes (batch_read_live doc, and batchLiveReplay only downgrades on
// HostStatusUnknown, batch_read_live.go:126-131), so the roots stay live and
// the walk stays Complete.
//
// Static trace: entry → t.Setenv flag ON → migrate via the real db.Connect
// → counting pool over the same file → hand-built App literal (activityConn
// and batchCutoverWriter/Reader all fall back to app.DB() = the counting
// pool) → two rounds, counters reset right before each handleListSessions →
// first assertion require.Greater(stmts, 0) (broken instrumentation fails
// here) → the exact relations C + 2N / txs == 1 (per-session N-way
// annotation, per-owner tx, or lost chunking fail there) → the parent
// annotations pinned on one delegation session.
func TestHandleListSessions_AnnotationReadsBoundedInOwners(t *testing.T) {
	// Non-parallel: t.Setenv pins the flag (Go 1.26 Setenv rule).
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, batchRuntimeEnabled())
	ctx := t.Context()

	// Migrations must come from the real path; the counting pool then opens
	// the same file.
	dataDir := t.TempDir()
	_, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	c := &annotationCounters{}
	pool := annotationCountingPool(t, c, dataDir)

	// NewService(q, conn) with no reader aliases qRead/readDB to the writer
	// (internal/session/session.go NewServiceWithWorkspace), so List reads
	// ListSessions through the counting pool.
	s := session.NewService(db.New(pool), pool)
	a := &app.App{Sessions: s, DB: func() *sql.DB { return pool }}
	a.SetDataDirForTest(dataDir)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client

	// Round A: 3 top-level owners; two of them carry one delegation child.
	addOwner := func(name string, withChild bool) (string, string) {
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		childID := ""
		if withChild {
			child, err := a.Sessions.CreateTaskSession(ctx, name+"-child", sess.ID, "delegation child")
			require.NoError(t, err)
			childID = child.ID
		}
		reserveAnnotationRoot(t, ctx, pool, sess.ID, "tc-"+name, childID)
		return sess.ID, childID
	}
	firstDelegator, firstChild := addOwner("ann-owner-0", true)
	addOwner("ann-owner-1", true)
	addOwner("ann-owner-2", false)

	c.stmts.Store(0)
	c.txs.Store(0)
	handleListSessions(ctx, a, client, WSMessage{ID: "req-a", Type: CmdListSessions})
	replies := drainSessionsListReplies(t, client)
	require.NotEmpty(t, replies)
	rows := replies[len(replies)-1]
	require.Len(t, rows, 3, "the three top-level sessions, task children excluded")

	// Precondition FIRST: a broken instrumentation reads zero and must fail
	// here, never inside the exact bound.
	require.Greater(t, c.stmts.Load(), int64(0),
		"precondition: the counting pool must have served the reads — zero means the instrumentation is broken")

	// Round A exact relation: C + 2*3.
	require.Equal(t, int64(5+2*3), c.stmts.Load(),
		"stmts(N=3) == C + 2*N with C = 1 ListSessions + 1 ListSessionDrivers + 2 live-walk chunks + 1 sessionEndReasons (wake skipped: no store); slope 2 = the per-owner debt pair")
	require.Equal(t, int64(1), c.txs.Load(),
		"ONE shared read-only tx for the whole owners batch (BatchReactionDebtOwners), never one per owner")

	// Parent annotations on one delegation session: the exact single-child
	// slice (cf. handlers_sessions_batch_cutover_test.go:113), and the
	// by-design flag-ON divergence that a depth-0 live batch root is the
	// session's OWN work (batch_read_live.go:133-138 →
	// app_batch_cutover_facts.go:86-90 → handlers_sessions.go:276).
	row := findSessionRow(t, rows, firstDelegator)
	require.True(t, row.HasLiveOwnWork, "the depth-0 live batch root is the session's own live work")
	require.True(t, row.HasLiveDescendantWork, "the running delegation child is descendant work")
	require.Equal(t, []string{firstChild}, row.LiveDescendantIDs,
		"single seeded child: the slice is exactly that child id")

	// Round B: 9 more owners the same way (12 total) → the slope, not the
	// whole count, grows: +9 owners × 2 debt statements = +18, while the
	// chunked live walk stays 2 statements (both depths stay ≤500 owners,
	// batch_read_live.go:11, :57).
	for i := 3; i < 12; i++ {
		addOwner(fmt.Sprintf("ann-owner-%d", i), false)
	}
	c.stmts.Store(0)
	c.txs.Store(0)
	handleListSessions(ctx, a, client, WSMessage{ID: "req-b", Type: CmdListSessions})
	replies = drainSessionsListReplies(t, client)
	require.NotEmpty(t, replies)
	rows = replies[len(replies)-1]
	require.Len(t, rows, 12, "the twelve top-level sessions")
	require.Greater(t, c.stmts.Load(), int64(0), "precondition (round B): instrumentation still live")
	require.Equal(t, int64(5+2*12), c.stmts.Load(),
		"stmts(N=12) == C + 2*N: the growth is the LINEAR debt slope (batch_reaction.go:163-165), NOT ~4x the whole count; the live walk stays chunked at 2")
	require.Equal(t, int64(1), c.txs.Load(),
		"still ONE shared tx at N=12 (batch_reaction.go:156-165)")
}

func (c countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c countingConn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}
