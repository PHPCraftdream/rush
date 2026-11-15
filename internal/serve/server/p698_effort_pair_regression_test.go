package server

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type effortCallSpy struct {
	session.Service
	pairCalls []string
	slotCalls []string
}

func (s *effortCallSpy) UpdateReasoningEffort(ctx context.Context, id, smart, fast string) error {
	s.pairCalls = append(s.pairCalls, "smart-fast")
	return s.Service.UpdateReasoningEffort(ctx, id, smart, fast)
}

func (s *effortCallSpy) UpdateWorkerReviewerReasoningEffort(ctx context.Context, id, worker, reviewer string) error {
	s.pairCalls = append(s.pairCalls, "worker-reviewer")
	return s.Service.UpdateWorkerReviewerReasoningEffort(ctx, id, worker, reviewer)
}

func (s *effortCallSpy) UpdateSmartReasoningEffort(ctx context.Context, id, effort string) error {
	s.slotCalls = append(s.slotCalls, "smart")
	return s.Service.UpdateSmartReasoningEffort(ctx, id, effort)
}

func (s *effortCallSpy) UpdateFastReasoningEffort(ctx context.Context, id, effort string) error {
	s.slotCalls = append(s.slotCalls, "fast")
	return s.Service.UpdateFastReasoningEffort(ctx, id, effort)
}

func (s *effortCallSpy) UpdateWorkerReasoningEffort(ctx context.Context, id, effort string) error {
	s.slotCalls = append(s.slotCalls, "worker")
	return s.Service.UpdateWorkerReasoningEffort(ctx, id, effort)
}

func (s *effortCallSpy) UpdateReviewerReasoningEffort(ctx context.Context, id, effort string) error {
	s.slotCalls = append(s.slotCalls, "reviewer")
	return s.Service.UpdateReviewerReasoningEffort(ctx, id, effort)
}

func TestHandleSetSessionModels_EffortUpdatesOnlySentSlots(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "effort-slot-isolation")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.UpdateReasoningEffort(ctx, sess.ID, "high", "medium"))
	require.NoError(t, a.Sessions.UpdateWorkerReviewerReasoningEffort(ctx, sess.ID, "high", "low"))

	spy := &effortCallSpy{Service: a.Sessions}
	a.Sessions = spy

	// ModelSlot presence with an empty effort is not an effort write. The
	// requested efforts must go through only their individual slot methods;
	// pair calls can overwrite omitted slots when backed by stale state.
	reply := callSetSessionModels(t, ctx, a, "req-effort-slots", SetSessionModelsPayload{
		SessionID:     sess.ID,
		SmartModel:    &ModelOverrideWire{Provider: "p", Model: "m", ReasoningEffort: "low"},
		FastModel:     &ModelOverrideWire{Provider: "p", Model: "f"},
		WorkerModel:   &ModelOverrideWire{Provider: "p", Model: "w", ReasoningEffort: "max"},
		ReviewerModel: &ModelOverrideWire{Provider: "p", Model: "r"},
	})
	require.Equal(t, EventResponse, reply.Type)
	got, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"worker", "smart"}, spy.slotCalls)
	require.Empty(t, spy.pairCalls, "the handler must not use pair writes that can overwrite omitted slots")
	require.Equal(t, "low", got.SmartModelReasoningEffort)
	require.Equal(t, "medium", got.FastModelReasoningEffort)
	require.Equal(t, "max", got.WorkerModelReasoningEffort)
	require.Equal(t, "low", got.ReviewerModelReasoningEffort)

	// A back-to-back request touching the other member of each pair must
	// preserve the first request's efforts while updating fast/reviewer.
	reply = callSetSessionModels(t, ctx, a, "req-effort-slots-second", SetSessionModelsPayload{
		SessionID:     sess.ID,
		SmartModel:    &ModelOverrideWire{Provider: "p", Model: "m"},
		FastModel:     &ModelOverrideWire{Provider: "p", Model: "f", ReasoningEffort: "max"},
		WorkerModel:   &ModelOverrideWire{Provider: "p", Model: "w"},
		ReviewerModel: &ModelOverrideWire{Provider: "p", Model: "r", ReasoningEffort: "medium"},
	})
	require.Equal(t, EventResponse, reply.Type)
	got, err = a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "low", got.SmartModelReasoningEffort)
	require.Equal(t, "max", got.FastModelReasoningEffort)
	require.Equal(t, "max", got.WorkerModelReasoningEffort)
	require.Equal(t, "medium", got.ReviewerModelReasoningEffort)

	// Clear each pair's touched first slot. Legacy pair methods clear with SQL
	// NULL, and the slot methods must retain that exact storage semantics.
	reply = callSetSessionModels(t, ctx, a, "req-effort-clear", SetSessionModelsPayload{
		SessionID:   sess.ID,
		SmartModel:  &ModelOverrideWire{Provider: "p", Model: "m", ReasoningEffort: ReasoningEffortClear},
		WorkerModel: &ModelOverrideWire{Provider: "p", Model: "w", ReasoningEffort: ReasoningEffortClear},
	})
	require.Equal(t, EventResponse, reply.Type)
	var smartNull, workerNull bool
	require.NoError(t, a.DB().QueryRowContext(ctx, `SELECT smart_model_reasoning_effort IS NULL, worker_model_reasoning_effort IS NULL FROM sessions WHERE id = ?`, sess.ID).Scan(&smartNull, &workerNull))
	require.True(t, smartNull)
	require.True(t, workerNull)
}
