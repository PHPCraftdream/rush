package sessionbatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// BatchDeliveryRootEnvelope loads the persisted settled envelope of the root
// node, or ok=false when the root has not settled yet (or settled without an
// envelope). Read-only: no writer, no election, no projection.
func (s *BatchStore) BatchDeliveryRootEnvelope(ctx context.Context, owner, root string) (session.BatchDeliveryEnvelope, bool, error) {
	snap, err := s.Read(ctx, owner, root)
	if err != nil {
		return session.BatchDeliveryEnvelope{}, false, err
	}
	n := snap.Root
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

// BatchDeliveryElectInline wraps BatchAnnounceInlineResult with a typed
// won/lost outcome: won=false means the election was lost (another consumer
// delivered; no history inserted by the loser). A real error is a FAILED ACK,
// not a lost election, and is returned as an error.
func (s *BatchStore) BatchDeliveryElectInline(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (BatchInlineAnnouncement, bool, error) {
	a, err := s.BatchAnnounceInlineResult(ctx, messages, owner, root, params)
	if errors.Is(err, ErrBatchAnnouncementLost) {
		return BatchInlineAnnouncement{}, false, nil
	}
	if err != nil {
		return BatchInlineAnnouncement{}, false, err
	}
	return a, true, nil
}

// BatchDeliveryElectStarted wraps BatchAnnounceStarted with the same typed
// won/lost outcome as BatchDeliveryElectInline.
func (s *BatchStore) BatchDeliveryElectStarted(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (message.Message, bool, error) {
	m, err := s.BatchAnnounceStarted(ctx, messages, owner, root, params)
	if errors.Is(err, ErrBatchAnnouncementLost) {
		return message.Message{}, false, nil
	}
	if err != nil {
		return message.Message{}, false, err
	}
	return m, true, nil
}

// ErrBatchAnnouncementLost means another ack/inline consumer won or the
// addressed incarnation disappeared. No history is inserted by the loser.
var ErrBatchAnnouncementLost = errors.New("batch announcement election lost")

// BatchAnnounceStarted commits started history and root announcement together.
// It does not require a live state, delete a failed reservation, or dispatch.
func (s *BatchStore) BatchAnnounceStarted(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (message.Message, error) {
	// See batch_delivery_store.go: prepare before the tx holds the pool's only
	// connection.
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return message.Message{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, err
	}
	defer tx.Rollback()
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	// Acquire the writer and elect before history insertion. The placeholder is
	// never externally visible; the actual message identity replaces it in tx.
	n, err := q.BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{Owner: owner, RootID: root, MessageID: sql.NullString{String: "reserved", Valid: true}, UpdatedAt: time.Now().Unix()})
	if err != nil {
		return message.Message{}, err
	}
	if n != 1 {
		return message.Message{}, ErrBatchAnnouncementLost
	}
	msg, err := pdcCreateTx(ctx, q, tx, messages, owner, params)
	if err != nil {
		return message.Message{}, err
	}
	if err = session.BatchRequireRow(q.BatchOutboxSetAnnouncementMessage(ctx, db.BatchOutboxSetAnnouncementMessageParams{Owner: owner, RootID: root, MessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix()})); err != nil {
		return message.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return message.Message{}, err
	}
	messages.PublishCreated(msg)
	return msg, nil
}

// BatchInlineAnnouncement is an elected one-item completion, never stop intent.
type BatchInlineAnnouncement struct {
	Msg          message.Message
	DeliveryRows int64
}

// BatchAnnounceInlineResult elects only an unannounced selected terminal root
// with exactly one task. Ack, history and delivery consumption are atomic.
// A pull/ack/inline loser inserts no history. Call only with the actual elected
// completion envelope, not with a generic control/stop-intent answer.
func (s *BatchStore) BatchAnnounceInlineResult(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (BatchInlineAnnouncement, error) {
	// See batch_delivery_store.go: prepare before the tx holds the pool's only
	// connection.
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	defer tx.Rollback()
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	_, err = q.BatchOutboxInlineElect(ctx, db.BatchOutboxInlineElectParams{Owner: owner, RootID: root, UpdatedAt: time.Now().Unix()})
	if errors.Is(err, sql.ErrNoRows) {
		return BatchInlineAnnouncement{}, ErrBatchAnnouncementLost
	}
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	msg, err := pdcCreateTx(ctx, q, tx, messages, owner, params)
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	id := sql.NullString{String: msg.ID, Valid: true}
	if err = session.BatchRequireRow(q.BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{Owner: owner, RootID: root, MessageID: id, UpdatedAt: time.Now().Unix()})); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	if err = session.BatchRequireRow(q.BatchOutboxNameMessage(ctx, db.BatchOutboxNameMessageParams{Owner: owner, RootID: root, NodeID: root, MessageID: id, UpdatedAt: time.Now().Unix()})); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	messages.PublishCreated(msg)
	return BatchInlineAnnouncement{Msg: msg, DeliveryRows: 1}, nil
}

var ErrBatchToolResultIdentity = errors.New("batch tool result: invalid delivery identity")

// pdcTxQuerierMessages is the optional additive capability of message
// services whose transactional writes can run on a caller-supplied Querier
// (message.CreateTxQ/UpdateTxQ, PD11C). Batch call sites type-assert to it
// and fall back to CreateTx/UpdateTx on the bare tx when it is absent, so
// plain message.Service implementations and every existing test fake keep
// working unchanged.
type pdcTxQuerierMessages interface {
	CreateTxQ(ctx context.Context, q db.Querier, sessionID string, params message.CreateMessageParams) (message.Message, error)
	UpdateTxQ(ctx context.Context, q db.Querier, msg message.Message) (func(), error)
}

// pdcCreateTx runs the fused transaction's message INSERT through the
// binder-backed Queries when the service supports it, else through the
// legacy bare-tx CreateTx. Behavior is identical; only statement preparation
// differs.
func pdcCreateTx(ctx context.Context, q *db.Queries, tx *sql.Tx, messages message.Service, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	if s, ok := messages.(pdcTxQuerierMessages); ok {
		return s.CreateTxQ(ctx, q, sessionID, params)
	}
	return messages.CreateTx(ctx, tx, sessionID, params)
}

// pdcUpdateTx is pdcCreateTx's counterpart for the fused reaction update.
func pdcUpdateTx(ctx context.Context, q *db.Queries, tx *sql.Tx, messages message.Service, msg message.Message) (func(), error) {
	if s, ok := messages.(pdcTxQuerierMessages); ok {
		return s.UpdateTxQ(ctx, q, msg)
	}
	return messages.UpdateTx(ctx, tx, msg)
}

// BatchToolResultIdentity fences one consumed root incarnation.
type BatchToolResultIdentity struct {
	Owner, RootID, NodeID, HostID, RunnerID, ToolCallID string
}

// BatchConsumeToolResult atomically persists the actual tool message and consumes
// the matching root. A lost incarnation CAS is a successful ordinary result write.
func (s *BatchStore) BatchConsumeToolResult(ctx context.Context, messages message.Service, owner string, identity BatchToolResultIdentity, params message.CreateMessageParams) (msg message.Message, consumed bool, retErr error) {
	if owner == "" || owner != identity.Owner || identity.RootID == "" || identity.NodeID != identity.RootID || identity.HostID == "" || identity.RunnerID == "" || identity.ToolCallID == "" || params.Role != message.Tool {
		return message.Message{}, false, ErrBatchToolResultIdentity
	}
	// Prepare the batch statement cache BEFORE opening the tx: the writer pool
	// has exactly one connection, and preparing while the tx holds it would
	// deadlock (PdaBatchTxQueries itself never prepares; on a miss it falls
	// back to raw SQL, which would only happen if a pool release raced with
	// the Ensure below).
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return message.Message{}, false, err
	}
	// A lost incarnation CAS still persists the result message, so every
	// call is a productive write and is admitted.
	release, admission, err := session.AdmitFinalizationMutation(ctx, s.writer, owner)
	if release != nil {
		defer func() { retErr = session.BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return message.Message{}, false, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, false, err
	}
	defer tx.Rollback()
	if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return message.Message{}, false, err
	}
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	msg, err = pdcCreateTx(ctx, q, tx, messages, owner, params)
	if err != nil {
		return message.Message{}, false, err
	}
	rows, err := q.BatchOutboxConsumeWithToolResult(ctx, db.BatchOutboxConsumeWithToolResultParams{
		MessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(),
		Owner: owner, RootID: identity.RootID, NodeID: identity.NodeID, HostID: identity.HostID,
		RunnerID: identity.RunnerID, ToolCallID: sql.NullString{String: identity.ToolCallID, Valid: true},
	})
	if err != nil {
		return message.Message{}, false, err
	}
	if err = session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return message.Message{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return message.Message{}, false, err
	}
	messages.PublishCreated(msg)
	return msg, rows == 1, nil
}
