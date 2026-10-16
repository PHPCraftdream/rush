package session

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

var (
	ErrBatchControlStale        = errors.New("batch control: stale incarnation")
	ErrBatchControlHostMismatch = errors.New("batch control: host mismatch")
	ErrBatchControlTerminal     = errors.New("batch control: terminal target")
	ErrBatchControlUnknownHost  = errors.New("batch control: unknown host")
)

type BatchControlTarget struct{ Owner, Host, Root, Node string }

func batchControlTarget(ctx context.Context, q *db.Queries, t BatchControlTarget) (db.BatchNode, error) {
	n, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{OwnerSessionID: t.Owner, RootID: t.Root, NodeID: t.Node})
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrBatchControlStale
	}
	if err != nil {
		return n, err
	}
	if n.HostID != t.Host {
		return n, ErrBatchControlHostMismatch
	}
	r, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: t.Root, OwnerSessionID: t.Owner})
	if err != nil {
		return n, err
	}
	if !batchStateLive(r.State) {
		return n, ErrBatchControlStale
	}
	if !batchStateLive(n.State) {
		return n, ErrBatchControlTerminal
	}
	return n, nil
}

// BatchRequestStop rejects deleted/wrong-owner/wrong-host targets without
// persistence: the schema's exact-incarnation FK forbids such request rows.
// Unknown liveness writes nothing. Dead hosts use recovery, never cancellation.
func (s *BatchStore) BatchRequestStop(ctx context.Context, t BatchControlTarget, probe BatchHostProbe) (db.BatchControlRequest, error) {
	var empty db.BatchControlRequest
	if _, err := batchControlTarget(ctx, db.New(s.writer), t); err != nil {
		return empty, err
	}
	if probe == nil {
		return empty, ErrBatchControlUnknownHost
	}
	status, err := probe(t.Host)
	if err != nil {
		return empty, errors.Join(ErrBatchControlUnknownHost, err)
	}
	switch status {
	case HostStatusDead:
		_, err = s.BatchRecoverDeadHost(ctx, t.Host, probe)
		return empty, errors.Join(ErrBatchControlStale, err)
	case HostStatusAlive:
	default:
		return empty, ErrBatchControlUnknownHost
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return empty, err
	}
	if _, err = batchControlTarget(ctx, q, t); err != nil {
		return empty, err
	}
	// The existing creation API is intentionally not called: it owns a different writer.
	r, err := q.CreateBatchControlRequest(ctx, db.CreateBatchControlRequestParams{RequestID: uuid.NewString(), OwnerSessionID: t.Owner, HostID: t.Host, RootID: t.Root, NodeID: t.Node, CreatedAt: time.Now().Unix()})
	if err != nil {
		return empty, err
	}
	return r, tx.Commit()
}

func (s *BatchStore) BatchReadControl(ctx context.Context, t BatchControlTarget, id string) (db.BatchControlRequest, error) {
	r, err := db.New(s.writer).GetBatchControlRequest(ctx, db.GetBatchControlRequestParams{RequestID: id, OwnerSessionID: t.Owner, HostID: t.Host, RootID: t.Root})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.NodeID != t.Node) {
		return db.BatchControlRequest{}, ErrBatchControlStale
	}
	return r, err
}

func (s *BatchStore) BatchWaitControl(ctx context.Context, t BatchControlTarget, id string) (db.BatchControlRequest, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		r, err := s.BatchReadControl(ctx, t, id)
		if err != nil || r.Status == "answered" {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-ticker.C:
		}
	}
}

// BatchControlAccepted is proof of an accepted AND committed exact-node Stop.
// Sequence must be positive; Stop intent never synthesizes settlement.
type BatchControlAccepted struct {
	Code     string
	Sequence int64
}

// BatchControlCommit must fence Owner/Host/Root/Node in the owning Runner and
// deduplicate RequestID across pump replacement or failure after callback commit.
// It may project via this store: no writer transaction is held across callback.
// Return only after the accepted transition commits, or an error (no answer).
type BatchControlCommit func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error)

// BatchControlPump is explicitly serial. The host boundary must retain ONE
// pump per owning Runner incarnation; multiple requesters may use other stores.
// Host identity is supplied by that boundary, not by requester input.
type BatchControlPump struct {
	mu     sync.Mutex
	store  *BatchStore
	host   string
	commit BatchControlCommit
}

func NewBatchControlPump(s *BatchStore, owningHost string, commit BatchControlCommit) *BatchControlPump {
	return &BatchControlPump{store: s, host: owningHost, commit: commit}
}

func (p *BatchControlPump) Pump(ctx context.Context, owner, root string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == "" || p.commit == nil {
		return 0, ErrBatchControlHostMismatch
	}
	r, err := db.New(p.store.writer).GetBatchRoot(ctx, db.GetBatchRootParams{RootID: root, OwnerSessionID: owner})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBatchControlStale
	}
	if err != nil {
		return 0, err
	}
	if r.HostID != p.host {
		return 0, ErrBatchControlHostMismatch
	}
	requests, err := p.store.PendingControls(ctx, owner, p.host, root)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, request := range requests {
		t := BatchControlTarget{Owner: owner, Host: p.host, Root: root, Node: request.NodeID}
		current, e := p.store.BatchReadControl(ctx, t, request.RequestID)
		if e != nil {
			return count, e
		}
		if current.Status != "pending" {
			continue
		}
		if _, e = batchControlTarget(ctx, db.New(p.store.writer), t); e != nil {
			if errors.Is(e, ErrBatchControlTerminal) || errors.Is(e, ErrBatchControlStale) {
				if e = p.answerClosed(ctx, t, current.RequestID); e != nil {
					return count, e
				}
				count++
				continue
			}
			return count, e
		}
		accepted, e := p.commit(ctx, current)
		if e != nil {
			return count, e
		}
		if accepted.Sequence <= 0 || accepted.Code == "" {
			return count, ErrBatchProjectionConflict
		}
		// Exact identity CAS; answered controls are never redelivered by this pump.
		e = p.store.AnswerControl(ctx, db.AnswerBatchControlRequestParams{RequestID: current.RequestID, OwnerSessionID: owner, HostID: p.host, RootID: root, NodeID: current.NodeID, RunnerID: current.RunnerID, AnswerCode: accepted.Code, AnswerSequence: sql.NullInt64{Int64: accepted.Sequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})
		if e != nil {
			return count, e
		}
		count++
	}
	return count, nil
}

// answerClosed validates and answers terminal races in one writer transaction.
func (p *BatchControlPump) answerClosed(ctx context.Context, t BatchControlTarget, id string) error {
	tx, err := p.store.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return err
	}
	root, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: t.Root, OwnerSessionID: t.Owner})
	if err != nil {
		return err
	}
	node, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{RootID: t.Root, NodeID: t.Node, OwnerSessionID: t.Owner})
	if err != nil {
		return err
	}
	if root.HostID != p.host || node.HostID != p.host {
		return ErrBatchControlHostMismatch
	}
	if node.RunnerID != root.RunnerID {
		return ErrBatchProjectionConflict
	}
	code := "terminal"
	if !batchStateLive(root.State) {
		code = "stale"
	} else if batchStateLive(node.State) {
		return ErrBatchProjectionConflict
	}
	sequence := root.RootSequence
	if node.ProjectionSequence > sequence {
		sequence = node.ProjectionSequence
	}
	if sequence <= 0 {
		return ErrBatchProjectionConflict
	}
	err = batchRequireRow(q.AnswerBatchControlRequest(ctx, db.AnswerBatchControlRequestParams{RequestID: id, OwnerSessionID: t.Owner, RootID: t.Root, NodeID: t.Node, HostID: p.host, RunnerID: node.RunnerID, AnswerCode: code, AnswerSequence: sql.NullInt64{Int64: sequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}}))
	if err != nil {
		return err
	}
	return tx.Commit()
}
