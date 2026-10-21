package session

// PD11A Phase B: behavior-equivalence and concurrency tests for the batch
// prepared-statement cache. For every one of the 19 cached queries, the same
// operation is executed through BOTH paths on two fresh databases:
//
//	path P (prepared): db.PdaBatchEnsure, BeginTx, db.PdaBatchTxQueries
//	path R (raw):      BeginTx, db.New(tx)
//
// and the resulting table dumps (plus the query result fingerprints) must be
// identical after scrubbing per-database variable identifiers (UUIDs) and
// wall-clock epoch values.
//
// Mutation caught by the equivalence table: binding the wrong prepared
// statement to a Queries field, or binding a statement whose SQL text differs
// from the const, makes path P produce different rows/rowcounts than path R
// and the require.Equal on the dumps fails.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// pdaEquivState carries per-database variable identifiers produced by the
// seed (UUIDs) so they can be scrubbed before comparing the two databases.
type pdaEquivState struct {
	ids   []string // scrubbed positionally to ID0, ID1, ...
	root  string
	leaf  string
	nodes []string // every node id of the seeded tree (random UUIDs)
}

func (st pdaEquivState) scrubList() []string {
	out := append([]string{}, st.ids...)
	out = append(out, st.root, st.leaf)
	return append(out, st.nodes...)
}

func pdaEquivNodeIDs(snap BatchSnapshot) []string {
	ids := make([]string, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		ids = append(ids, n.NodeID)
	}
	return ids
}

var pdaEquivTSRe = regexp.MustCompile(`\b\d{10,15}\b`)

func pdaEquivScrub(s string, ids []string) string {
	for i, id := range ids {
		if id != "" {
			s = strings.ReplaceAll(s, id, fmt.Sprintf("ID%d", i))
		}
	}
	return pdaEquivTSRe.ReplaceAllString(s, "TS")
}

// pdaEquivDump reads a whole query result fully into scrubbed strings. The
// writer pool has ONE connection; rows are always closed.
func pdaEquivDump(t *testing.T, ctx context.Context, conn *sql.DB, query string, ids []string) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	out = append(out, strings.Join(cols, "|"))
	for rows.Next() {
		raw := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		require.NoError(t, rows.Scan(dest...))
		cells := make([]string, len(cols))
		for i, v := range raw {
			if !v.Valid {
				cells[i] = "∅"
			} else {
				cells[i] = pdaEquivScrub(v.String, ids)
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
	require.NoError(t, rows.Err())
	return out
}

const (
	pdaEquivBatchNodesDump     = "SELECT * FROM batch_nodes ORDER BY ordinal, kind"
	pdaEquivCheckpointsDump    = "SELECT * FROM batch_runner_checkpoints ORDER BY host_id, runner_id"
	pdaEquivSessionNoticesDump = "SELECT * FROM session_notices ORDER BY id"
	pdaEquivMessagesDump       = "SELECT * FROM messages ORDER BY id"
)

type pdaEquivCase struct {
	name string
	// seed runs through the REAL store/service fixtures on each fresh DB;
	// its first require.NoError is the first production-facing assertion.
	seed  func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState
	act   func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string)
	dumps []string
}

// pdaEquivRun executes one path on a fresh database and returns the scrubbed
// table dumps plus the scrubbed query-result fingerprints.
func pdaEquivRun(t *testing.T, c pdaEquivCase, prepared bool) ([]string, []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	s := NewBatchStore(conn)
	st := c.seed(t, ctx, s, conn)
	if prepared {
		require.NoError(t, db.PdaBatchEnsure(ctx, conn))
	}
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	var q *db.Queries
	if prepared {
		q = db.PdaBatchTxQueries(ctx, conn, tx)
	} else {
		q = db.New(tx)
	}
	fingerprints := []string{}
	c.act(t, ctx, q, st, &fingerprints)
	require.NoError(t, tx.Commit())
	var dumps []string
	for _, d := range c.dumps {
		dumps = append(dumps, pdaEquivDump(t, ctx, conn, d, st.scrubList())...)
	}
	for i, f := range fingerprints {
		fingerprints[i] = pdaEquivScrub(f, st.scrubList())
	}
	return dumps, fingerprints
}

func pdaEquivReserve(t *testing.T, ctx context.Context, s *BatchStore) pdaEquivState {
	t.Helper()
	snap := batchStoreTestReserve(t, s, ctx)
	return pdaEquivState{root: snap.Root.RootID, leaf: snap.Nodes[1].NodeID, nodes: pdaEquivNodeIDs(snap)}
}

// pdaEquivSettleOne reserves a single-task root and settles its ROOT row
// through the store: state completed + notice selected, still unannounced
// with delivery='pending' — the precondition of the outbox election queries.
func pdaEquivSettleOne(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
	t.Helper()
	snap := batchOutboxReserve(t, s, ctx, 1)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	return pdaEquivState{root: snap.Root.RootID, leaf: snap.Nodes[1].NodeID, nodes: pdaEquivNodeIDs(snap)}
}

func pdaEquivSeedNotice(t *testing.T, ctx context.Context, conn *sql.DB, extraUpdate string) {
	t.Helper()
	n, err := db.New(conn).InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: "owner", Kind: NoticeKindChildQuestion, Text: "question", Wake: 1, CreatedAt: 1, UpdatedAt: 1})
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE session_notices SET "+extraUpdate+" WHERE id=?", n.ID)
	require.NoError(t, err)
}

func pdaEquivCases() []pdaEquivCase {
	return []pdaEquivCase{
		{
			name: "getLiveBatchRoot",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				n, err := q.GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{OwnerSessionID: "owner", ToolCallID: batchStoreTestString("call")})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("root=%s state=%s seq=%d", n.RootID, n.State, n.RootSequence))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Direct reserve with FIXED ids: the full params struct is
			// typed-passed, mirroring batchInsertTree in batch_store.go.
			name: "reserveBatchNode",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivState{}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				v := db.ReserveBatchNodeParams{
					NodeID: "R", RootID: "R", OwnerSessionID: "owner", HostID: "host",
					Ordinal: 0, Kind: "batch",
					ToolCallID: batchStoreTestString("call"), InputHash: batchStoreTestString("hash"),
					Mode: batchStoreTestString("parallel"), TaskGroup: "",
					State: "pending", Cause: "", ControlState: "none",
					RootSequence: 1, ProjectionSequence: 1, CreatedAt: 5, UpdatedAt: 5,
				}
				got, err := q.ReserveBatchNode(ctx, v)
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("node=%s state=%s", got.NodeID, got.State))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			name: "getBatchRoot",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				n, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: st.root, OwnerSessionID: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("root=%s kind=%s state=%s", n.RootID, n.Kind, n.State))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			name: "listBatchNodes",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: st.root, OwnerSessionID: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("count=%d first=%s last=%s", len(nodes), nodes[0].NodeID, nodes[len(nodes)-1].NodeID))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			name: "getBatchNode",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				n, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{RootID: st.root, NodeID: st.leaf, OwnerSessionID: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("node=%s kind=%s state=%s", n.NodeID, n.Kind, n.State))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Mirrors the Project call shape in batch_store.go.
			name: "advanceBatchRootSequence",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: st.root, HostID: "host", RunnerID: "", OwnerSessionID: "owner", Sequence: 2, UpdatedAt: 7})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			name: "projectBatchNode",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				v := batchStoreTestUpdate(st.leaf, "starting")
				v.RootID, v.HostID, v.OwnerSessionID, v.Sequence, v.UpdatedAt, v.RunnerID = st.root, "host", "owner", 2, 7, ""
				rows, err := q.ProjectBatchNode(ctx, v)
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// leaf1 is settled WITH its notice through the store (the Project
			// notices path); the query under test then selects the notice on
			// leaf2, settled to completed in the same transition with
			// projection_sequence=2 — a normal (non-void) root.
			name: "selectBatchNodeNotice",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				snap := batchStoreTestReserve(t, s, ctx)
				require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{
					Owner: "owner", Root: snap.Root.RootID,
					Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Nodes[1].NodeID, "completed"), batchStoreTestUpdate(snap.Nodes[2].NodeID, "completed")},
					Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(snap.Nodes[1].NodeID)},
				}}}))
				return pdaEquivState{root: snap.Root.RootID, leaf: snap.Nodes[2].NodeID, nodes: pdaEquivNodeIDs(snap)}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				v := batchStoreTestNotice(st.leaf)
				v.NoticeSequence = sql.NullInt64{Int64: 2, Valid: true}
				v.RootID, v.HostID, v.OwnerSessionID, v.RunnerID, v.UpdatedAt = st.root, "host", "owner", "", 7
				rows, err := q.SelectBatchNodeNotice(ctx, v)
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Mirrors the batch_commit.go UpdateBatchRunnerCheckpoint call
			// sites (insert, then upsert with a higher sequence).
			name: "updateBatchRunnerCheckpoint",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivState{}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.UpdateBatchRunnerCheckpoint(ctx, db.UpdateBatchRunnerCheckpointParams{HostID: "host", RunnerID: "run", Sequence: 1, EventType: "started", Final: 0, UpdatedAt: 7})
				require.NoError(t, err)
				rows2, err := q.UpdateBatchRunnerCheckpoint(ctx, db.UpdateBatchRunnerCheckpointParams{HostID: "host", RunnerID: "run", Sequence: 2, EventType: "finished", Final: 1, UpdatedAt: 8})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d rows2=%d", rows, rows2))
			},
			dumps: []string{pdaEquivCheckpointsDump},
		},
		{
			name: "batchOutboxAnnounce",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{MessageID: batchStoreTestString("m1"), UpdatedAt: 7, Owner: "owner", RootID: st.root})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Seed announces the root through a constraint-safe UPDATE (the
			// query under test requires announced=1).
			name: "batchOutboxSetAnnouncementMessage",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				st := pdaEquivReserve(t, ctx, s)
				_, err := conn.ExecContext(ctx, "UPDATE batch_nodes SET announced=1, announce_message_id='m1' WHERE node_id=?", st.root)
				require.NoError(t, err)
				return st
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.BatchOutboxSetAnnouncementMessage(ctx, db.BatchOutboxSetAnnouncementMessageParams{MessageID: batchStoreTestString("m2"), UpdatedAt: 8, Owner: "owner", RootID: st.root})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// The single-task root is settled (terminal state, notice
			// selected, delivery pending, unannounced) via the store, which
			// is exactly the inline-election precondition.
			name: "batchOutboxInlineElect",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivSettleOne(t, ctx, s, conn)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				n, err := q.BatchOutboxInlineElect(ctx, db.BatchOutboxInlineElectParams{UpdatedAt: 7, Owner: "owner", RootID: st.root})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("node=%s delivery=%s wake=%d", n.NodeID, n.Delivery, n.Wake))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Requires notice_selected=1, delivery='done',
			// notice_message_id IS NULL on the root; delivery is moved to
			// 'done' with a constraint-safe UPDATE after the store settle.
			name: "batchOutboxNameMessage",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				st := pdaEquivSettleOne(t, ctx, s, conn)
				_, err := conn.ExecContext(ctx, "UPDATE batch_nodes SET delivery='done' WHERE node_id=?", st.root)
				require.NoError(t, err)
				return st
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.BatchOutboxNameMessage(ctx, db.BatchOutboxNameMessageParams{MessageID: batchStoreTestString("m"), UpdatedAt: 7, Owner: "owner", RootID: st.root, NodeID: st.root})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Full root incarnation fence: host/runner/tool_call match the
			// seeded reservation ("host"/""/"call").
			name: "batchOutboxConsumeWithToolResult",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivSettleOne(t, ctx, s, conn)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.BatchOutboxConsumeWithToolResult(ctx, db.BatchOutboxConsumeWithToolResultParams{MessageID: batchStoreTestString("m"), UpdatedAt: 7, Owner: "owner", RootID: st.root, NodeID: st.root, HostID: "host", RunnerID: "", ToolCallID: batchStoreTestString("call")})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			name: "getBatchRootIdentity",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				row, err := q.GetBatchRootIdentity(ctx, st.root)
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("owner=%s host=%s runner=%s", row.OwnerSessionID, row.HostID, row.RunnerID))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// Deleting the root cascades the whole seeded subtree (3 rows).
			name: "deleteBatchSubtree",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				return pdaEquivReserve(t, ctx, s)
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{RootID: st.root, NodeID: st.root, HostID: "host", OwnerSessionID: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivBatchNodesDump},
		},
		{
			// The message row is created through the existing message service
			// (same pattern as batch_reaction_test.go).
			name: "getMessage",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				m := message.NewService(db.New(conn))
				msg, err := m.Create(ctx, "owner", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "hi"}}})
				require.NoError(t, err)
				return pdaEquivState{ids: []string{msg.ID}}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				got, err := q.GetMessage(ctx, st.ids[0])
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("id=%s role=%s", got.ID, got.Role))
			},
			dumps: []string{pdaEquivMessagesDump},
		},
		{
			// The session_notice row is seeded through the generated
			// InsertSessionNotice (public store surface) and moved to
			// delivery='done' with a constraint-safe UPDATE — the precondition
			// of the mark-reacted owner update.
			name: "markSessionNoticesReactedForOwner",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				pdaEquivSeedNotice(t, ctx, conn, "delivery='done'")
				return pdaEquivState{}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.MarkSessionNoticesReactedForOwner(ctx, db.MarkSessionNoticesReactedForOwnerParams{UpdatedAt: 7, Owner: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivSessionNoticesDump},
		},
		{
			name: "clearSessionNoticesReactedFailedForOwner",
			seed: func(t *testing.T, ctx context.Context, s *BatchStore, conn *sql.DB) pdaEquivState {
				pdaEquivSeedNotice(t, ctx, conn, "reacted_failed=1")
				return pdaEquivState{}
			},
			act: func(t *testing.T, ctx context.Context, q *db.Queries, st pdaEquivState, out *[]string) {
				rows, err := q.ClearSessionNoticesReactedFailedForOwner(ctx, db.ClearSessionNoticesReactedFailedForOwnerParams{UpdatedAt: 7, Owner: "owner"})
				require.NoError(t, err)
				*out = append(*out, fmt.Sprintf("rows=%d", rows))
			},
			dumps: []string{pdaEquivSessionNoticesDump},
		},
	}
}

// B1: per-statement equivalence, table-driven, parallel subtests under a
// parallel parent (tparallel).
func TestPdaBatchPreparedEquivalence(t *testing.T) {
	t.Parallel()
	for _, c := range pdaEquivCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rawDump, rawOut := pdaEquivRun(t, c, false)
			prepDump, prepOut := pdaEquivRun(t, c, true)
			require.Equal(t, rawDump, prepDump, "prepared path must leave identical table state")
			require.Equal(t, rawOut, prepOut, "prepared path must return identical rows/rowcounts")
		})
	}
}

// B2: read-your-committed-write through the prepared tx path.
// Mutation caught: prepared tx statements reading stale/pre-commit state.
// First production-facing assertion: require.NoError on the store Reserve.
func TestPdaBatchPreparedReadYourCommits(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx) // require.NoError inside — first production-facing assertion
	p := BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, "starting")}}}}
	require.NoError(t, s.Project(ctx, p))
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "starting", got.Root.State)
}

// B3: concurrent full Reserve→Project flows on the SAME store. Errors are
// collected and asserted on the TEST goroutine only after the join.
// Mutation caught: unshared/incorrectly-bound cached statements under
// concurrent tx serialization (a statement closed while another tx uses it,
// or MaxOpenConns(1) re-prepare races → "statement is closed"/"database is
// locked", or a lost node row).
func TestPdaBatchConcurrentReserveProject(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	const workers = 2
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := batchStoreTestReservation()
			p.ToolCallID = fmt.Sprintf("call-%d", i)
			p.InputHash = fmt.Sprintf("hash-%d", i)
			r, err := s.Reserve(ctx, p)
			if err != nil {
				errs <- err
				return
			}
			proj := BatchProjection{Host: "host", Sequence: r.Snapshot.Root.RootSequence + 1, Roots: []BatchRootProjection{{Owner: "owner", Root: r.Snapshot.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(r.Snapshot.Root.NodeID, "starting")}}}}
			errs <- s.Project(ctx, proj)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		// Both commits must succeed: any error here — in particular one
		// containing "statement is closed" or "database is locked" — fails.
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes").Scan(&count))
	require.Equal(t, 6, count, "two roots × three rows each; a lost/failed commit shows here")
}

// B4: lifecycle through the store. Releasing the writer (the t.Cleanup
// release stays paired; double release is a no-op by design) must make a
// subsequent Reserve fail with a clean error, not a panic from dropped
// prepared statements.
func TestPdaBatchReserveAfterReleaseIsCleanError(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, db.ReleaseConn(conn)) // explicit release; t.Cleanup's is the paired no-op
	_, err := s.Reserve(ctx, batchStoreTestReservation())
	require.Error(t, err, "use-after-release must error cleanly, never panic")
}
