package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

var (
	ErrBatchInputMismatch      = errors.New("batch store: live input mismatch")
	ErrBatchProjectionConflict = errors.New("batch store: projection identity or sequence conflict")
	// R5(b): a live root for the same owner/tool-call belongs to its owning
	// host's Runner; attaching from another host would resubmit foreign work.
	ErrBatchForeignLiveHost = errors.New("batch store: live root owned by another host")
	// R5(c): same host, different Runner incarnation. Cross-incarnation
	// attachment is recovery work (R9); refused here, never silently fenced.
	ErrBatchRunnerMismatch = errors.New("batch store: live root held by another runner incarnation")
)

// BatchStore is inert persistence. It neither executes work nor chooses outcomes.
// The supplied DB is the single-connection writer; no store locks span I/O.
type BatchStore struct{ writer *sql.DB }

func NewBatchStore(writer *sql.DB) *BatchStore { return &BatchStore{writer: writer} }

// BatchReservationNode contains accepted values; identity and times are assigned by the store.
type BatchReservationNode struct {
	Values   db.ReserveBatchNodeParams
	Children []BatchReservationNode
}

// BatchReservation contains accepted values; times are assigned by the store.
// RootID empty means the store assigns a fresh UUID root (legacy Reserve);
// callers holding Runner root identity set it explicitly. Updates and Notices
// project the initial Changed views in the SAME transaction (R5(a)).
type BatchReservation struct {
	Owner, Host, RunnerID, RootID, ToolCallID, InputHash string
	Sequence                                             int64
	Root                                                 BatchReservationNode
	Updates                                              []db.ProjectBatchNodeParams
	Notices                                              []db.SelectBatchNodeNoticeParams
	// Checkpoint is written inside the same transaction when non-nil.
	Checkpoint *db.UpdateBatchRunnerCheckpointParams
}

type (
	BatchSnapshot struct {
		Root  db.BatchNode
		Nodes []db.BatchNode
	}
	BatchReservationResult struct {
		Snapshot BatchSnapshot
		Attached bool
	}
)

func batchSnapshot(ctx context.Context, q *db.Queries, owner, root string) (BatchSnapshot, error) {
	r, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: root, OwnerSessionID: owner})
	if err != nil {
		return BatchSnapshot{}, err
	}
	nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root, OwnerSessionID: owner})
	return BatchSnapshot{Root: r, Nodes: nodes}, err
}

// Reserve keeps the legacy store-generated-UUID path for existing callers.
func (s *BatchStore) Reserve(ctx context.Context, p BatchReservation) (BatchReservationResult, error) {
	return s.ReserveIdentity(ctx, p)
}

// ReserveIdentity reserves the accepted tree under the caller's root ID and
// projects initial views and notices in ONE transaction. Live attachment
// compares input hash, owning host and Runner incarnation before reuse.
func (s *BatchStore) ReserveIdentity(ctx context.Context, p BatchReservation) (BatchReservationResult, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchReservationResult{}, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	live, err := q.GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{OwnerSessionID: p.Owner, ToolCallID: sql.NullString{String: p.ToolCallID, Valid: true}})
	attached := err == nil
	root := live.RootID
	if attached {
		if live.InputHash.String != p.InputHash {
			return BatchReservationResult{}, ErrBatchInputMismatch
		}
		if live.HostID != p.Host {
			return BatchReservationResult{}, ErrBatchForeignLiveHost
		}
		if live.RunnerID != p.RunnerID {
			return BatchReservationResult{}, ErrBatchRunnerMismatch
		}
	} else {
		if !errors.Is(err, sql.ErrNoRows) {
			return BatchReservationResult{}, err
		}
		if root = p.RootID; root == "" {
			root = uuid.NewString()
		}
		if err = batchInsertTree(ctx, q, p.Root, p.Owner, p.Host, p.RunnerID, root, root, "", 0, p.Sequence, p.ToolCallID, p.InputHash); err != nil {
			return BatchReservationResult{}, err
		}
	}
	// Reservation inserts write root_sequence/projection_sequence themselves;
	// no root advance here, matching the legacy Reserve behavior exactly.
	if err = batchProjectRoot(ctx, q, p.Host, p.RunnerID, BatchRootProjection{Owner: p.Owner, Root: root, Updates: p.Updates, Notices: p.Notices}, p.Sequence, false); err != nil {
		return BatchReservationResult{}, err
	}
	if p.Checkpoint != nil {
		if err = batchRequireRow(q.UpdateBatchRunnerCheckpoint(ctx, *p.Checkpoint)); err != nil {
			return BatchReservationResult{}, err
		}
	}
	snapshot, err := batchSnapshot(ctx, q, p.Owner, root)
	if err != nil {
		return BatchReservationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchReservationResult{}, err
	}
	return BatchReservationResult{Snapshot: snapshot, Attached: attached}, nil
}

func batchInsertTree(ctx context.Context, q *db.Queries, spec BatchReservationNode, owner, host, runner, root, node, parent string, ordinal, sequence int64, call, hash string) error {
	v := spec.Values
	v.NodeID, v.RootID, v.OwnerSessionID, v.HostID, v.RunnerID = node, root, owner, host, runner
	v.ParentID = sql.NullString{String: parent, Valid: parent != ""}
	v.Ordinal, v.RootSequence, v.ProjectionSequence = ordinal, 0, sequence
	v.ToolCallID, v.InputHash = sql.NullString{}, sql.NullString{}
	if parent == "" {
		v.RootSequence = sequence
		v.ToolCallID, v.InputHash = sql.NullString{String: call, Valid: true}, sql.NullString{String: hash, Valid: true}
	}
	v.CreatedAt, v.UpdatedAt = time.Now().Unix(), time.Now().Unix()
	if _, err := q.ReserveBatchNode(ctx, v); err != nil {
		return err
	}
	for i, child := range spec.Children {
		if err := batchInsertTree(ctx, q, child, owner, host, runner, root, fmt.Sprintf("%s.%d", node, i+1), node, int64(i+1), sequence, "", ""); err != nil {
			return err
		}
	}
	return nil
}

// BatchAddition reserves a new accepted subtree at its parent ordinal.
type (
	BatchAddition struct {
		Parent  string
		Ordinal int64
		Node    BatchReservationNode
	}
	BatchProjection struct {
		Host, RunnerID string
		Sequence       int64
		Roots          []BatchRootProjection
		// Checkpoint records the (host, incarnation) transition sequence for
		// rootless transitions and Final; optional, applied in the same tx.
		Checkpoint *db.UpdateBatchRunnerCheckpointParams
	}
	BatchRootProjection struct {
		Owner, Root string
		Updates     []db.ProjectBatchNodeParams
		Additions   []BatchAddition
		Notices     []db.SelectBatchNodeNoticeParams
	}
)

func batchRequireRow(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBatchProjectionConflict
	}
	return nil
}

// Project commits one accepted transition, including the (host, incarnation)
// sequence checkpoint. Any missing update rolls everything back.
func (s *BatchStore) Project(ctx context.Context, p BatchProjection) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if p.Checkpoint != nil {
		if err = batchRequireRow(q.UpdateBatchRunnerCheckpoint(ctx, *p.Checkpoint)); err != nil {
			return err
		}
	}
	for _, root := range p.Roots {
		if err = batchProjectRoot(ctx, q, p.Host, p.RunnerID, root, p.Sequence, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func batchProjectRoot(ctx context.Context, q *db.Queries, host, runner string, p BatchRootProjection, sequence int64, advance bool) error {
	// Reservation inserts carry their sequence already; every other
	// projection fences the root sequence before touching rows.
	if advance {
		if err := batchRequireRow(q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: p.Root, HostID: host, RunnerID: runner, OwnerSessionID: p.Owner, Sequence: sequence, UpdatedAt: time.Now().Unix()})); err != nil {
			return err
		}
	}
	// Additions precede updates: a freshly appended node's own Changed view
	// must find its row (fence projection_sequence < sequence still holds).
	for _, a := range p.Additions {
		parent, e := q.GetBatchNode(ctx, db.GetBatchNodeParams{RootID: p.Root, NodeID: a.Parent, OwnerSessionID: p.Owner})
		if e != nil {
			return e
		}
		if parent.HostID != host || parent.Kind != "batch" || !batchStateLive(parent.State) {
			return ErrBatchProjectionConflict
		}
		if err := batchInsertTree(ctx, q, a.Node, p.Owner, host, runner, p.Root, fmt.Sprintf("%s.%d", a.Parent, a.Ordinal), a.Parent, a.Ordinal, sequence, "", ""); err != nil {
			return err
		}
	}
	for _, v := range p.Updates {
		v.RootID, v.HostID, v.OwnerSessionID, v.Sequence, v.UpdatedAt, v.RunnerID = p.Root, host, p.Owner, sequence, time.Now().Unix(), runner
		if err := batchRequireRow(q.ProjectBatchNode(ctx, v)); err != nil {
			return err
		}
	}
	for _, v := range p.Notices {
		v.RootID, v.HostID, v.OwnerSessionID, v.RunnerID = p.Root, host, p.Owner, runner
		v.NoticeSequence = sql.NullInt64{Int64: sequence, Valid: true}
		v.UpdatedAt = time.Now().Unix()
		if err := batchRequireRow(q.SelectBatchNodeNotice(ctx, v)); err != nil {
			return err
		}
	}
	return nil
}

func batchStateLive(state string) bool {
	return state == "pending" || state == "starting" || state == "running" || state == "cancelling"
}

func (s *BatchStore) Read(ctx context.Context, owner, root string) (BatchSnapshot, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatchSnapshot{}, err
	}
	defer tx.Rollback()
	snapshot, err := batchSnapshot(ctx, db.New(tx), owner, root)
	if err != nil {
		return BatchSnapshot{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchSnapshot{}, err
	}
	return snapshot, nil
}

func (s *BatchStore) LiveRootsForHost(ctx context.Context, host string) ([]db.BatchNode, error) {
	return db.New(s.writer).ListLiveBatchRootsForHost(ctx, host)
}

func (s *BatchStore) CreateControl(ctx context.Context, owner, host, root, node string) (db.BatchControlRequest, error) {
	return db.New(s.writer).CreateBatchControlRequest(ctx, db.CreateBatchControlRequestParams{RequestID: uuid.NewString(), OwnerSessionID: owner, HostID: host, RootID: root, NodeID: node, CreatedAt: time.Now().Unix()})
}

func (s *BatchStore) PendingControls(ctx context.Context, owner, host, root string) ([]db.BatchControlRequest, error) {
	return db.New(s.writer).ListPendingBatchControlRequests(ctx, db.ListPendingBatchControlRequestsParams{OwnerSessionID: owner, HostID: host, RootID: root})
}

func (s *BatchStore) AnswerControl(ctx context.Context, p db.AnswerBatchControlRequestParams) error {
	return batchRequireRow(db.New(s.writer).AnswerBatchControlRequest(ctx, p))
}

func (s *BatchStore) DeleteRoot(ctx context.Context, owner, host, root string) error {
	return batchRequireRow(db.New(s.writer).DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{OwnerSessionID: owner, HostID: host, RootID: root, NodeID: root}))
}
