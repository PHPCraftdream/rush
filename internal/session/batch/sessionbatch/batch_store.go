package sessionbatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
)

var (
	ErrBatchInputMismatch = errors.New("batch store: live input mismatch")
	// R5(b): a live root for the same owner/tool-call belongs to its owning
	// host's Runner; attaching from another host would resubmit foreign work.
	ErrBatchForeignLiveHost = errors.New("batch store: live root owned by another host")
	// R5(c): same host, different Runner incarnation. Cross-incarnation
	// attachment is recovery work (R9); refused here, never silently fenced.
	ErrBatchRunnerMismatch = errors.New("batch store: live root held by another runner incarnation")
)

// pdcBatchRootFenceSQL is the root stop-fence probe executed inside the
// projection transactions (batchProjectRoot). Package-level so the PD11C
// registration below and the equivalence test pin the exact production text.
var pdcBatchRootFenceSQL = `SELECT EXISTS(SELECT 1 FROM batch_control_requests WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND node_id = ? AND runner_id = ? AND ` + psfOwnerStopSQLPredicate("") + `)`

// PD11C (e): register the fixed-text statements the batch transactions
// execute outside the generated layer so PdaBatchEnsure prepares them with
// the rest. Dynamic chunked IN (...) statements stay raw by design.
func init() {
	db.PdbBatchRegisterSQL(pdcBatchRootFenceSQL)
	db.PdbBatchRegisterSQL(batchOutboxPullVoidAware)
	db.PdbBatchRegisterSQL(pdcBatchDirectChildOutcomesSQL)
	db.PdbBatchRegisterSQL(batchOutboxReactNodesVoidAware)
}

// BatchStore is inert persistence. It neither executes work nor chooses outcomes.
// The supplied DB is the single-connection writer; no store locks span I/O.
type BatchStore struct {
	writer *sql.DB
	// reader is the optional read pool for pure read snapshots. nil (or
	// equal to writer, the degraded mode where the read pool failed to
	// open) keeps the pre-PD11B behaviour exactly: read txs on the writer.
	reader *sql.DB
}

func NewBatchStore(writer *sql.DB) *BatchStore { return &BatchStore{writer: writer} }

// NewBatchStoreWithReader returns a BatchStore that routes pure read
// snapshots to the optional read pool. A nil reader, or a reader identical
// to the writer, keeps writer-routed reads. The reader pool must be a WAL
// read handle for the same database file, as returned by db.ConnectRead;
// WAL read-your-committed-write guarantees a read tx that begins after a
// writer Commit returned observes it.
func NewBatchStoreWithReader(writer, reader *sql.DB) *BatchStore {
	if reader == nil || reader == writer {
		return &BatchStore{writer: writer}
	}
	return &BatchStore{writer: writer, reader: reader}
}

// beginReadTx opens the read-only transaction used by pure read paths: on
// the reader pool when one is wired, else on the writer (pre-PD11B
// behavior).
func (s *BatchStore) beginReadTx(ctx context.Context) (*sql.Tx, error) {
	pool := s.writer
	if s.reader != nil {
		pool = s.reader
	}
	return pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

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
	OriginCLI                                            bool
	Root                                                 BatchReservationNode
	Updates                                              []db.ProjectBatchNodeParams
	Notices                                              []db.SelectBatchNodeNoticeParams
	// Checkpoint is written inside the same transaction when non-nil.
	Checkpoint *db.UpdateBatchRunnerCheckpointParams

	finalizationAdmission *session.BatchFinalizationAdmission
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
	// Prepare the batch statement cache BEFORE opening the tx: the writer pool
	// has exactly one connection, and preparing while the tx holds it would
	// deadlock (PdaBatchTxQueries itself never prepares; on a miss it falls
	// back to raw SQL, which would only happen if a pool release raced with
	// the Ensure above).
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhasePrepare)
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return BatchReservationResult{}, err
	}
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseAcquire)
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchReservationResult{}, err
	}
	defer tx.Rollback()
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseStatements)
	if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{p.Owner}, p.finalizationAdmission); err != nil {
		return BatchReservationResult{}, err
	}
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
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
		if err = batchInsertTree(ctx, q, p.Root, p.Owner, p.Host, p.RunnerID, root, root, "", 0, p.Sequence, p.ToolCallID, p.InputHash, p.OriginCLI); err != nil {
			return BatchReservationResult{}, err
		}
	}
	// Reservation inserts write root_sequence/projection_sequence themselves;
	// no root advance here, matching the legacy Reserve behavior exactly.
	if err = batchProjectRoot(ctx, q, p.Host, p.RunnerID, BatchRootProjection{Owner: p.Owner, Root: root, Updates: p.Updates, Notices: p.Notices}, p.Sequence, false); err != nil {
		return BatchReservationResult{}, err
	}
	if p.Checkpoint != nil {
		if err = session.BatchRequireRow(q.UpdateBatchRunnerCheckpoint(ctx, *p.Checkpoint)); err != nil {
			return BatchReservationResult{}, err
		}
	}
	snapshot, err := batchSnapshot(ctx, q, p.Owner, root)
	if err != nil {
		return BatchReservationResult{}, err
	}
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseCommit)
	if err = tx.Commit(); err != nil {
		return BatchReservationResult{}, err
	}
	return BatchReservationResult{Snapshot: snapshot, Attached: attached}, nil
}

func batchInsertTree(ctx context.Context, q *db.Queries, spec BatchReservationNode, owner, host, runner, root, node, parent string, ordinal, sequence int64, call, hash string, originCLI bool) error {
	v := spec.Values
	v.NodeID, v.RootID, v.OwnerSessionID, v.HostID, v.RunnerID = node, root, owner, host, runner
	v.ParentID = sql.NullString{String: parent, Valid: parent != ""}
	v.Ordinal, v.RootSequence, v.ProjectionSequence = ordinal, 0, sequence
	v.ToolCallID, v.InputHash = sql.NullString{}, sql.NullString{}
	if parent == "" {
		v.RootSequence = sequence
		v.ToolCallID, v.InputHash = sql.NullString{String: call, Valid: true}, sql.NullString{String: hash, Valid: true}
		if originCLI {
			v.OriginCli = 1
		}
	}
	v.CreatedAt, v.UpdatedAt = time.Now().Unix(), time.Now().Unix()
	if _, err := q.ReserveBatchNode(ctx, v); err != nil {
		return err
	}
	for i, child := range spec.Children {
		if err := batchInsertTree(ctx, q, child, owner, host, runner, root, fmt.Sprintf("%s.%d", node, i+1), node, int64(i+1), sequence, "", "", false); err != nil {
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

		finalizationAdmission *session.BatchFinalizationAdmission
	}
	BatchRootProjection struct {
		Owner, Root string
		Updates     []db.ProjectBatchNodeParams
		Additions   []BatchAddition
		Notices     []db.SelectBatchNodeNoticeParams
	}
)

// Project commits one accepted transition, including the (host, incarnation)
// sequence checkpoint. Any missing update rolls everything back.
func (s *BatchStore) Project(ctx context.Context, p BatchProjection) error {
	// See ReserveIdentity: prepare before the tx holds the pool's only
	// connection.
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhasePrepare)
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return err
	}
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseAcquire)
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseStatements)
	owners := make([]string, 0, len(p.Roots))
	for _, root := range p.Roots {
		owners = append(owners, root.Owner)
	}
	if err := session.ValidateBatchFinalizationTx(ctx, tx, owners, p.finalizationAdmission); err != nil {
		return err
	}
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	if p.Checkpoint != nil {
		if err = session.BatchRequireRow(q.UpdateBatchRunnerCheckpoint(ctx, *p.Checkpoint)); err != nil {
			return err
		}
	}
	for _, root := range p.Roots {
		if err = batchProjectRoot(ctx, q, p.Host, p.RunnerID, root, p.Sequence, true); err != nil {
			return err
		}
	}
	jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseCommit)
	return tx.Commit()
}

func batchProjectRoot(ctx context.Context, q *db.Queries, host, runner string, p BatchRootProjection, sequence int64, advance bool) error {
	// Reservation inserts carry their sequence already; every other
	// projection fences the root sequence before touching rows.
	if advance {
		if err := session.BatchRequireRow(q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: p.Root, HostID: host, RunnerID: runner, OwnerSessionID: p.Owner, Sequence: sequence, UpdatedAt: time.Now().Unix()})); err != nil {
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
			return session.ErrBatchProjectionConflict
		}
		if err := batchInsertTree(ctx, q, a.Node, p.Owner, host, runner, p.Root, fmt.Sprintf("%s.%d", a.Parent, a.Ordinal), a.Parent, a.Ordinal, sequence, "", "", false); err != nil {
			return err
		}
	}
	for _, v := range p.Updates {
		v.RootID, v.HostID, v.OwnerSessionID, v.Sequence, v.UpdatedAt, v.RunnerID = p.Root, host, p.Owner, sequence, time.Now().Unix(), runner
		if err := session.BatchRequireRow(q.ProjectBatchNode(ctx, v)); err != nil {
			return err
		}
	}
	for _, v := range p.Notices {
		v.RootID, v.HostID, v.OwnerSessionID, v.RunnerID = p.Root, host, p.Owner, runner
		v.NoticeSequence = sql.NullInt64{Int64: sequence, Valid: true}
		v.UpdatedAt = time.Now().Unix()
		// R12-F round 4: read the target node's ROOT row first. If that root
		// was durably retired by a rerun (BatchRerunReconcile's in-tx void,
		// delivery='void'), its subtree can never select notices — the
		// selection is a tolerated no-op. This is the ONLY tolerance: every
		// other zero-row SelectBatchNodeNotice stays a hard
		// .
		root, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{RootID: p.Root, NodeID: p.Root, OwnerSessionID: p.Owner})
		if err != nil {
			return err
		}
		if root.Delivery == "void" {
			continue
		}
		var fenced int
		if err := q.PdbBatchRawQueryRow(ctx, pdcBatchRootFenceSQL, p.Owner, host, p.Root, p.Root, root.RunnerID).Scan(&fenced); err != nil {
			return err
		}
		if fenced != 0 {
			v.Wake = 0
		}
		// Every selected notice names a settled node, and settled nodes are
		// always in the same transition's Changed set (engine.go), so the
		// preceding ProjectBatchNode update already moved the row to exactly
		// this projection_sequence. 0 rows therefore means notice_selected
		// was already 1 — a projection identity conflict the commit path
		// must surface, never silently tolerate.
		if err := session.BatchRequireRow(q.SelectBatchNodeNotice(ctx, v)); err != nil {
			return err
		}
	}
	return nil
}

func batchStateLive(state string) bool {
	return state == "pending" || state == "starting" || state == "running" || state == "cancelling"
}

func (s *BatchStore) Read(ctx context.Context, owner, root string) (BatchSnapshot, error) {
	tx, err := s.beginReadTx(ctx)
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
	return session.BatchRequireRow(db.New(s.writer).AnswerBatchControlRequest(ctx, p))
}

func (s *BatchStore) DeleteRoot(ctx context.Context, owner, host, root string) error {
	q, err := db.PdaBatchWriterQueries(ctx, s.writer)
	if err != nil {
		return err
	}
	return session.BatchRequireRow(q.DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{OwnerSessionID: owner, HostID: host, RootID: root, NodeID: root}))
}

// RootOwner resolves the owning session of any projected root; cross-root
// transitions (admission, Release) project under the row's owner, never the
// hook identity's owner. It is a single read, not a lock across writer I/O.
func (s *BatchStore) RootOwner(ctx context.Context, root string) (string, error) {
	q, err := db.PdaBatchWriterQueries(ctx, s.writer)
	if err != nil {
		return "", err
	}
	row, err := q.GetBatchRootIdentity(ctx, root)
	if err != nil {
		return "", err
	}
	return row.OwnerSessionID, nil
}

type BatchCLIIdentity struct{ Owner, Root, Host, Runner, State string }

func (s *BatchStore) BatchCLIIdentities(ctx context.Context) ([]BatchCLIIdentity, error) {
	rows, err := s.writer.QueryContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,state FROM batch_nodes WHERE node_id=root_id ORDER BY created_at,root_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchCLIIdentity{}
	for rows.Next() {
		var x BatchCLIIdentity
		if err = rows.Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &x.State); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *BatchStore) BatchCLIFind(ctx context.Context, prefix string) (BatchCLIIdentity, string, error) {
	if strings.TrimSpace(prefix) == "" {
		return BatchCLIIdentity{}, "", errors.New("batch target is empty")
	}
	// Exact nodes (including roots) take precedence over prefix matches.
	var x BatchCLIIdentity
	var node string
	err := s.writer.QueryRowContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,node_id,state FROM batch_nodes WHERE node_id=?`, prefix).Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &node, &x.State)
	if err == nil {
		return x, node, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BatchCLIIdentity{}, "", err
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,node_id,state FROM batch_nodes WHERE node_id=root_id AND node_id LIKE ? ESCAPE '\' ORDER BY root_id`, strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(prefix)+"%")
	if err != nil {
		return BatchCLIIdentity{}, "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err = rows.Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &node, &x.State); err != nil {
			return BatchCLIIdentity{}, "", err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return BatchCLIIdentity{}, "", err
	}
	if count == 0 {
		return BatchCLIIdentity{}, "", sql.ErrNoRows
	}
	if count > 1 {
		return BatchCLIIdentity{}, "", errors.New("batch target prefix is ambiguous")
	}
	return x, node, nil
}

func (s *BatchStore) BatchCLIGetNode(ctx context.Context, owner, root, node string) (db.BatchNode, error) {
	return db.New(s.writer).GetBatchNode(ctx, db.GetBatchNodeParams{OwnerSessionID: owner, RootID: root, NodeID: node})
}

func (s *BatchStore) BatchCLIPollControl(ctx context.Context, t BatchControlTarget, id string, wait time.Duration) (db.BatchControlRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return s.BatchWaitControl(ctx, t, id)
}

// BatchLiveProbe is one read-only live-attach lookup. The fences are exactly
// ReserveIdentity's attach block: input hash, owning host and Runner
// incarnation. A mismatch is an ERROR, never a resubmission. The
// AnnounceMessageID-derived log key names the generated started-ack
// message, not the caller's policy message id.
//
// SubmittedMessageID and SubmittedRegistryID are OPTIONAL probe diagnostics
// (review 4, live input mismatch): the caller may carry the invoking
// message id and pinned registry identity so the mismatch path can log
// stored-vs-submitted identity. Callers that do not supply them leave the
// fields empty; they are never matched against, only logged.
type BatchLiveProbe struct {
	Owner, Host, RunnerID, ToolCallID, InputHash string
	// SubmittedMessageID and SubmittedRegistryID are diagnostics only.
	SubmittedMessageID  string
	SubmittedRegistryID string
}

// BatchProbeLive reports the live root reserved for (owner, tool call id)
// under the caller's fences, WITHOUT inserting or projecting anything: the
// commit hook reserves at engine Submit time, so a probe must stay read-only
// or a first caller would create two reservations. attached=false with a nil
// error means no live root exists; the caller submits.
func (s *BatchStore) BatchProbeLive(ctx context.Context, p BatchLiveProbe) (string, bool, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	live, err := db.New(tx).GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{OwnerSessionID: p.Owner, ToolCallID: sql.NullString{String: p.ToolCallID, Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// R11F round-2 (P1-7 item 7): the schema forces task_kind/task_payload to
	// NULL on the kind='batch' ROOT row, so the one-item TASK spec can only be
	// reconstructed from the root's TASK CHILD row(s). Read them inside the
	// same read-only tx.
	children, err := db.New(tx).ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: live.RootID, OwnerSessionID: p.Owner})
	if err != nil {
		return "", false, err
	}
	detail := batchProbeHashDetail(live, children, p.InputHash)
	if !detail.direct && !detail.fallback {
		slog.Warn("batch probe live input mismatch",
			"root", live.RootID,
			"tool_call_id", p.ToolCallID,
			"stored_hash", live.InputHash.String,
			"submitted_hash", p.InputHash,
			"reconstructed_task_hash", detail.taskHash,
			"direct_match", detail.direct,
			"fallback_match", detail.fallback,
			"announce_message_id", live.AnnounceMessageID.String,
			"submitted_message_id", p.SubmittedMessageID,
			"submitted_registry_id", p.SubmittedRegistryID,
			"stored_registry_id", detail.storedRegistryID,
			"task_children", batchProbeTaskChildCount(live, children))
		return "", false, ErrBatchInputMismatch
	}
	slog.Debug("batch probe live attach",
		"root", live.RootID,
		"tool_call_id", p.ToolCallID,
		"match", batchProbeMatchName(detail),
		"announce_message_id", live.AnnounceMessageID.String,
		"submitted_message_id", p.SubmittedMessageID,
		"submitted_registry_id", p.SubmittedRegistryID,
		"stored_registry_id", detail.storedRegistryID)
	if live.HostID != p.Host {
		return "", false, ErrBatchForeignLiveHost
	}
	if live.RunnerID != p.RunnerID {
		return "", false, ErrBatchRunnerMismatch
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return live.RootID, true, nil
}

// batchProbeTaskChildCount counts the root's direct task children for the
// mismatch diagnostics; it never reads payload content.
func batchProbeTaskChildCount(live db.BatchNode, children []db.BatchNode) int {
	count := 0
	for _, n := range children {
		if n.Kind == "task" && n.ParentID.Valid && n.ParentID.String == live.RootID {
			count++
		}
	}
	return count
}

// batchProbeMatchName names which hash comparison matched, for diagnostics.
func batchProbeMatchName(d batchProbeHashes) string {
	if d.direct {
		return "direct"
	}
	return "fallback"
}

// batchProbeHashes is the outcome of the probe's two hash comparisons,
// used both for the attach decision and the mismatch diagnostics.
type batchProbeHashes struct {
	direct   bool
	fallback bool
	taskHash string
	// storedRegistryID carries the stored task payload's registry_id for
	// mismatch diagnostics only; it is never read or logged otherwise.
	storedRegistryID string
}

// batchProbeHashDetail compares the probe hash against the reserved rows and
// reports which comparison matched plus the reconstructed task hash for
// diagnostics. The direct column match covers probes that already carry the
// commit hook's canonical Spec hash; the recomputed match covers probes whose
// hash is the canonical hash of the one-item TASK spec (batch_invocation.go).
// R11F round-2 (P1-7 item 7): the task kind and payload live ONLY on the
// root's single TASK CHILD row (a root row has task_kind NULL by schema
// CHECK), so the spec is reconstructed from that child — never from the
// root. Zero or several task children is ambiguous and fails closed. The
// reconstruction carries the child's stored group and timeout too, so a
// caller hashing the FULL task spec (jobbatch.TaskSpec Kind, Group, Payload
// and Timeout) matches; dropping them previously diverged from any such
// caller's hash.
func batchProbeHashDetail(live db.BatchNode, children []db.BatchNode, hash string) batchProbeHashes {
	detail := batchProbeHashes{direct: live.InputHash.String == hash}
	var task *db.BatchNode
	for i := range children {
		n := children[i]
		if n.NodeID == live.RootID || !n.ParentID.Valid || n.ParentID.String != live.RootID {
			continue
		}
		if n.Kind != "task" || task != nil {
			detail.taskHash = ""
			return detail
		}
		task = &children[i]
	}
	if task == nil || !task.TaskKind.Valid || !task.TaskPayload.Valid {
		return detail
	}
	detail.storedRegistryID = batchProbeStoredRegistryID(*task)
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind:    task.TaskKind.String,
		Group:   task.TaskGroup,
		Payload: json.RawMessage(task.TaskPayload.String),
		Timeout: time.Duration(task.TimeoutNs),
	}}
	detail.taskHash = batchCommitSpecHash(spec)
	detail.fallback = detail.taskHash == hash
	return detail
}

// BatchDeliveryOneItemCompletion resolves THE completion of a one-item batch
// (R11F P1-2). The R11B commit hook writes the Settled envelope on the
// settled NODE — for a one-item batch that is the single task child
// (root.1), not the root — so the read is: the root envelope when present,
// else the single task child's envelope. More than one child of the root
// with an unreadable stored envelope fails CLOSED (error), never a
// fabricated or partial completion; a multi-item root without envelopes is
// handled by the terminal derivation below. Read-only.
//
// Terminal-without-envelope roots (review 5a): when neither the root nor a
// single task child carries a response envelope, the completion is derived
// from ONE consistent snapshot read (the s.Read transaction above): the root
// AND every direct child must be terminal, otherwise the batch is live and
// ok=false. A ONE-ITEM root derives from its single TASK CHILD's own
// terminal outcome: the StartRejected outcome lives on the task (jobbatch
// engine.go EventStartRejected) and the root is only the fallback. A
// MULTI-ITEM root returns the ROOT-level outcome (root state/code/summary):
// a per-item summary would need a structured per-node list (node id, state,
// code per task), which this scalar state/code/summary envelope cannot carry
// without a shape change. No executor details, no media or metadata, no
// fabricated success; Details and Ref stay empty because no executor
// envelope exists. Accepted-execution envelopes are
// returned unchanged by the earlier reads above.
func (s *BatchStore) BatchDeliveryOneItemCompletion(ctx context.Context, owner, root string) (session.BatchDeliveryEnvelope, bool, error) {
	snap, err := s.Read(ctx, owner, root)
	if err != nil {
		// A root row that does not exist (yet, or ever — e.g. a foreign or
		// unknown root) is "no persisted completion", NOT an error: the
		// batchDoneSource contract (batch_delivery.go LoadDone) reads a
		// persisted completion without waiting and reports ok=false when
		// none exists, and AwaitDone's durable poll must keep waiting on
		// such a root until ctx cancel instead of treating it as terminal.
		// This no-rows error can only originate from the FIRST query of
		// batchSnapshot (GetBatchRoot, a :one query on the root row,
		// batch_nodes.sql); the second query (ListBatchNodes) is :many and
		// returns an empty slice, never sql.ErrNoRows. So errors.Is
		// sql.ErrNoRows here unambiguously means "root row absent".
		if errors.Is(err, sql.ErrNoRows) {
			return session.BatchDeliveryEnvelope{}, false, nil
		}
		return session.BatchDeliveryEnvelope{}, false, err
	}
	if e, ok, err := batchDeliveryEnvelopeOf(snap.Root); ok || err != nil {
		return e, ok, err
	}
	var children []db.BatchNode
	for _, n := range snap.Nodes {
		if n.NodeID != root && n.ParentID.Valid && n.ParentID.String == root {
			children = append(children, n)
		}
	}
	if len(children) == 1 {
		if e, ok, err := batchDeliveryEnvelopeOf(children[0]); ok || err != nil {
			return e, ok, err
		}
	}
	// No envelope anywhere. A terminal root WITHOUT an executor response
	// (StartRejected, pending-stop terminal, recovery-interrupted) is
	// delivered honestly from persisted terminal facts; a live root or a
	// live task child stays live (ok=false) and keeps waiting.
	return batchTerminalCompletion(snap.Root, children)
}

// batchDeliveryEnvelopeOf decodes one node's stored response envelope.
// ok=false when the node has no envelope yet (or the column is empty).
func batchDeliveryEnvelopeOf(n db.BatchNode) (session.BatchDeliveryEnvelope, bool, error) {
	if !n.ResponseEnvelope.Valid || n.ResponseEnvelope.String == "" {
		return session.BatchDeliveryEnvelope{}, false, nil
	}
	var e session.BatchDeliveryEnvelope
	if err := json.Unmarshal([]byte(n.ResponseEnvelope.String), &e); err != nil {
		return session.BatchDeliveryEnvelope{}, false, err
	}
	e.Ref = n.ResponseEnvelopeRef.String
	return e, true, nil
}

// batchNodeTerminal reports whether one batch node row is in a final state.
// The set matches jobbatch's State.Terminal and the terminal-state SQL of
// batch_stop.go; pending, starting, running and cancelling are live.
func batchNodeTerminal(n db.BatchNode) bool {
	switch n.State {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// batchProbeStoredRegistryID extracts ONLY the registry_id metadata field
// from the stored task payload, for mismatch diagnostics; it never reads or
// logs any other payload content. Empty when absent or unparseable.
func batchProbeStoredRegistryID(task db.BatchNode) string {
	if !task.TaskPayload.Valid {
		return ""
	}
	var p struct {
		RegistryID string `json:"registry_id"`
	}
	if err := json.Unmarshal([]byte(task.TaskPayload.String), &p); err != nil {
		return ""
	}
	return p.RegistryID
}

// batchTerminalEnvelope builds the derived envelope from the primary row's
// terminal facts, falling back to the fallback row's facts when the primary
// carries none.
func batchTerminalEnvelope(primary, fallback db.BatchNode) session.BatchDeliveryEnvelope {
	code := primary.OutcomeCode
	if code == "" {
		code = fallback.OutcomeCode
	}
	summary := primary.OutcomeSummary
	if summary == "" {
		summary = fallback.OutcomeSummary
	}
	if summary == "" {
		summary = primary.Cause
	}
	if summary == "" {
		summary = fallback.Cause
	}
	if summary == "" {
		summary = code
	}
	if summary == "" {
		summary = primary.State + " without executor response"
	}
	return session.BatchDeliveryEnvelope{
		State:   primary.State,
		Code:    code,
		Summary: summary,
	}
}

// batchTerminalCompletion derives the honest, error-shaped completion of a
// terminal root that has no executor response envelope. It reports done only
// when the root AND every direct child are terminal in the SAME snapshot; a
// live row keeps the completion "not ready" (ok=false).
//
// A ONE-ITEM root derives from its single TASK CHILD's own terminal outcome:
// EventStartRejected assigns the rejected outcome/code to the task
// (jobbatch engine.go EventStartRejected) and structural settlement computes
// the root state WITHOUT copying the task outcome into the root, so the
// child's facts are the real terminal facts; the root's own outcome is the
// fallback. A MULTI-ITEM root returns the ROOT-level outcome: a per-item
// summary would need a structured per-node list (node id, state, code per
// task), which this scalar state/code/summary envelope cannot carry without
// a shape change; the root-level outcome is the only honest aggregate
// persisted for it. The summary falls back through the primary outcome
// summary, the fallback outcome summary, the persisted causes, the outcome
// code itself, and finally a plain "<state> without executor response"
// note. No executor details, no media or metadata, no fabricated success.
func batchTerminalCompletion(root db.BatchNode, children []db.BatchNode) (session.BatchDeliveryEnvelope, bool, error) {
	if !batchNodeTerminal(root) {
		return session.BatchDeliveryEnvelope{}, false, nil
	}
	for _, n := range children {
		if !batchNodeTerminal(n) {
			return session.BatchDeliveryEnvelope{}, false, nil
		}
	}
	if len(children) == 1 {
		return batchTerminalEnvelope(children[0], root), true, nil
	}
	return batchTerminalEnvelope(root, root), true, nil
}
