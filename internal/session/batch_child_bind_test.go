package session

// Durable delegation child-link bind (task #1490): BatchBindChildSession
// must persist batch_nodes.child_session_id BEFORE the child session runs,
// fenced on the full reservation identity, and the write must survive later
// NULL projections (ProjectBatchNode's COALESCE) and the live walk's reads.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// batchBindFixture reserves one batch root (RunnerID "runner") with two task
// children under owner "owner", moves the second child's node (the
// delegation node) to 'starting', and creates one child session row parented
// to "owner". It returns the store, the raw writer pool, the data dir, the
// task node id and the root id.
func batchBindFixture(t *testing.T) (*BatchStore, *sql.DB, string, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))

	p := batchStoreTestReservation()
	p.RunnerID = "runner"
	s := NewBatchStore(conn)
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	taskNode := r.Snapshot.Nodes[1].NodeID
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state = 'starting' WHERE node_id = ?", taskNode)
	require.NoError(t, err)

	require.NoError(t, seedSession(ctx, db.New(conn), "owner-child"))
	_, err = conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = 'owner' WHERE id = 'owner-child'")
	require.NoError(t, err)
	return s, conn, dir, taskNode, r.Snapshot.Root.RootID
}

// batchBindReadLink reads child_session_id through an INDEPENDENT sqlite
// pool on the same database file — proving the link is durable on disk, not
// just visible through the writer's in-memory page cache.
func batchBindReadLink(t *testing.T, dir, taskNode string) sql.NullString {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "rush.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	defer raw.Close()
	var linked sql.NullString
	require.NoError(t, raw.QueryRowContext(context.Background(), "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&linked))
	return linked
}

func TestBatchBindChildSessionPersistsEdge(t *testing.T) {
	t.Parallel()
	s, _, dir, taskNode, root := batchBindFixture(t)
	ctx := context.Background()

	require.NoError(t, s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child"))
	require.True(t, batchBindReadLink(t, dir, taskNode).Valid, "the link must be durable through an independent pool")
	require.Equal(t, "owner-child", batchBindReadLink(t, dir, taskNode).String)

	// MUTATION: a later attach-path projection writing a NULL
	// child_session_id that CLEARED the column (no COALESCE) would leave
	// the link NULL here and fail the equality below.
	_, err := s.Reserve(ctx, BatchReservation{
		Owner: "owner", Host: "host", RunnerID: "runner", ToolCallID: "call", InputHash: "hash",
		Sequence: 2,
		Root:     BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: batchStoreTestString("parallel"), State: "running", ControlState: "none"}, Children: []BatchReservationNode{batchStoreTestTask(), batchStoreTestTask()}},
		Updates:  []db.ProjectBatchNodeParams{batchStoreTestUpdate(taskNode, "running")},
	})
	require.NoError(t, err)
	require.Equal(t, "owner-child", batchBindReadLink(t, dir, taskNode).String,
		"the COALESCE must preserve the bound child link across the attach projection")
}

func TestBatchBindChildSessionNegativeTwins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	newVoidedRoot := func(t *testing.T, conn *sql.DB, root string) {
		t.Helper()
		// The schema CHECK requires the full notice shape for
		// notice_selected=1; delivery='void' is the history-void marker.
		_, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'batch',
			notice_state = 'completed', notice_cause = '', notice_sequence = 1, notice_ordinal = 0,
			delivery = 'void' WHERE node_id = ?`, root)
		require.NoError(t, err)
	}

	cases := []struct {
		name    string
		mutate  func(t *testing.T, conn *sql.DB, root, taskNode string)
		bind    func(s *BatchStore, root, taskNode string) error
		wantErr error
	}{
		{
			name: "wrong owner",
			// MUTATION: dropping the owner scope from the node lookup lets a
			// foreign owner bind its id onto another session's node.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "other-owner", "host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindNodeUnknown,
		},
		{
			name: "wrong host",
			// MUTATION: dropping the host fence lets another host's bind
			// succeed → the ErrBatchBindForeignIdentity assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "other-host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindForeignIdentity,
		},
		{
			name: "wrong runner",
			// MUTATION: dropping the runner-incarnation fence lets a stale
			// Runner bind → the ErrBatchBindForeignIdentity assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "other-runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindForeignIdentity,
		},
		{
			name: "stale token",
			// MUTATION: dropping the token fence lets a superseded
			// incarnation bind → the ErrBatchBindStaleToken assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 1, "owner-child")
			},
			wantErr: ErrBatchBindStaleToken,
		},
		{
			name: "node not live",
			mutate: func(t *testing.T, conn *sql.DB, root, taskNode string) {
				_, e := conn.ExecContext(ctx, "UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?", taskNode)
				require.NoError(t, e)
			},
			// MUTATION: binding onto a settled node would resurrect a
			// terminal task's linkage → the ErrBatchBindNodeNotLive
			// assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindNodeNotLive,
		},
		{
			name: "root completed",
			mutate: func(t *testing.T, conn *sql.DB, root, taskNode string) {
				_, e := conn.ExecContext(ctx, "UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?", root)
				require.NoError(t, e)
			},
			// MUTATION: a terminal root accepting binds would violate the
			// root-liveness invariant → the ErrBatchBindRootNotLive
			// assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindRootNotLive,
		},
		{
			name: "root cancelling",
			mutate: func(t *testing.T, conn *sql.DB, root, taskNode string) {
				_, e := conn.ExecContext(ctx, "UPDATE batch_nodes SET state = 'cancelling' WHERE node_id = ?", root)
				require.NoError(t, e)
			},
			// MUTATION: a cancelling root accepting binds would bind a child
			// into work being torn down → the ErrBatchBindRootNotLive
			// assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindRootNotLive,
		},
		{
			name:   "voided root",
			mutate: func(t *testing.T, conn *sql.DB, root, taskNode string) { newVoidedRoot(t, conn, root) },
			// MUTATION: losing the history-void predicate (notice_selected=1
			// AND delivery='void') would bind children onto a rerun-voided
			// root → the ErrBatchBindRootNotLive assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child")
			},
			wantErr: ErrBatchBindRootNotLive,
		},
		{
			name: "child parented elsewhere",
			mutate: func(t *testing.T, conn *sql.DB, root, taskNode string) {
				require.NoError(t, seedSession(ctx, db.New(conn), "other-parent"))
				require.NoError(t, seedSession(ctx, db.New(conn), "foreign-child"))
				_, e := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = 'other-parent' WHERE id = 'foreign-child'")
				require.NoError(t, e)
			},
			// MUTATION: dropping the child-parent EXISTS fence lets a child
			// of ANOTHER session be bound → the ErrBatchBindChildForeign
			// assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "foreign-child")
			},
			wantErr: ErrBatchBindChildForeign,
		},
		{
			name: "child missing",
			// MUTATION: dropping the child-session EXISTS fence lets a
			// nonexistent child id be bound → the ErrBatchBindChildForeign
			// assertion fails.
			bind: func(s *BatchStore, root, taskNode string) error {
				return s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "no-such-child")
			},
			wantErr: ErrBatchBindChildForeign,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, conn, _, taskNode, root := batchBindFixture(t)
			if tc.mutate != nil {
				tc.mutate(t, conn, root, taskNode)
			}
			err := tc.bind(s, root, taskNode)
			require.ErrorIs(t, err, tc.wantErr)
			var linked sql.NullString
			require.NoError(t, conn.QueryRowContext(ctx, "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&linked))
			require.False(t, linked.Valid, "a failed bind must leave the link NULL")
		})
	}
}

func TestBatchBindChildSessionConflict(t *testing.T) {
	t.Parallel()
	s, conn, _, taskNode, root := batchBindFixture(t)
	ctx := context.Background()
	require.NoError(t, seedSession(ctx, db.New(conn), "owner-child-b"))
	_, err := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = 'owner' WHERE id = 'owner-child-b'")
	require.NoError(t, err)

	require.NoError(t, s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child"))
	// MUTATION: letting a second bind OVERWRITE the first (no
	// child_session_id fence in the UPDATE) would repoint the node at a
	// different child → the ErrBatchBindChildConflict assertion and the
	// first-child read-back below fail.
	err = s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child-b")
	require.ErrorIs(t, err, ErrBatchBindChildConflict)
	var linked sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&linked))
	require.Equal(t, "owner-child", linked.String, "the FIRST bound child must survive a conflicting bind")
}

func TestBatchBindChildSessionIdempotent(t *testing.T) {
	t.Parallel()
	s, conn, _, taskNode, root := batchBindFixture(t)
	ctx := context.Background()
	require.NoError(t, s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child"))
	var before string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&before))
	// MUTATION: turning the same-child rebind into a conflict (requiring
	// child_session_id IS NULL instead of same-value tolerance) breaks
	// repeat binds → the second NoError below fails.
	require.NoError(t, s.BatchBindChildSession(ctx, "owner", "host", "runner", root, taskNode, 0, "owner-child"))
	var after string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNode).Scan(&after))
	require.Equal(t, before, after, "the bound child must be unchanged by an idempotent rebind")
}

func TestBatchBindChildSessionTokenOverflow(t *testing.T) {
	t.Parallel()
	s, _, _, taskNode, root := batchBindFixture(t)
	// MUTATION: dropping the int63 guard would truncate the uint64 token
	// into a negative int64 → the ErrBatchValueOverflow assertion fails.
	err := s.BatchBindChildSession(context.Background(), "owner", "host", "runner", root, taskNode, uint64(1)<<63, "owner-child")
	require.ErrorIs(t, err, ErrBatchValueOverflow)
}
