package sessionbatch

import (
	"context"
	"encoding/json"
	"errors"

	session "github.com/PHPCraftdream/rush/internal/session/service"

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
