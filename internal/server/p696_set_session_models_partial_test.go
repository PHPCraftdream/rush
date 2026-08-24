package server

// Task #696 (F8, docs/reviews/2026-08-24_11-31-29-readonly-review-b3b470cf.md):
// handleSetSessionModels performs several independent column-scoped updates
// (smart/fast models, worker/reviewer models, and each pair's reasoning
// efforts) and used to reply an unqualified EventResponse{"status":"ok"}
// even when one of the later updates had failed with only a slog.Warn —
// and an early failure of a later-invoked group replied a bare EventError
// that hid which earlier groups had already landed. The client could never
// tell partial failure from full success.
//
// The reply now carries SetSessionModelsResult (applied/failed/errors),
// following DeleteOtherSessionsResult's shape (task #684). These tests
// force exactly one update method to fail deterministically — a thin
// wrapper around the real session.Service, no timing or race dependency —
// and assert the reply separates applied from failed fields and matches
// the actual DB state. They also pin the reasoning-effort wire contract's
// empty-versus-ReasoningEffortClear distinction: omitted preserves the
// stored effort, the sentinel resets it to unset.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// failOneUpdateService wraps a real session.Service and fails exactly the
// update methods whose flags are set, delegating everything else (including
// Get, which the handler uses for effort backfill) to the embedded real
// service unchanged.
type failOneUpdateService struct {
	session.Service
	failUpdateModels                        bool
	failUpdateWorkerReviewerModels          bool
	failUpdateWorkerReviewerReasoningEffort bool
	failUpdateReasoningEffort               bool
}

func (f *failOneUpdateService) UpdateModels(ctx context.Context, sessionID string, smart, fast *session.ModelSlotUpdate) error {
	if f.failUpdateModels {
		return errors.New("simulated: smart/fast model write failed")
	}
	return f.Service.UpdateModels(ctx, sessionID, smart, fast)
}

func (f *failOneUpdateService) UpdateWorkerReviewerModels(ctx context.Context, sessionID string, worker, reviewer *session.ModelSlotUpdate) error {
	if f.failUpdateWorkerReviewerModels {
		return errors.New("simulated: worker/reviewer model write failed")
	}
	return f.Service.UpdateWorkerReviewerModels(ctx, sessionID, worker, reviewer)
}

func (f *failOneUpdateService) UpdateWorkerReviewerReasoningEffort(ctx context.Context, sessionID, workerEffort, reviewerEffort string) error {
	if f.failUpdateWorkerReviewerReasoningEffort {
		return errors.New("simulated: worker/reviewer effort write failed")
	}
	return f.Service.UpdateWorkerReviewerReasoningEffort(ctx, sessionID, workerEffort, reviewerEffort)
}

func (f *failOneUpdateService) UpdateReasoningEffort(ctx context.Context, sessionID, smartEffort, fastEffort string) error {
	if f.failUpdateReasoningEffort {
		return errors.New("simulated: smart/fast effort write failed")
	}
	return f.Service.UpdateReasoningEffort(ctx, sessionID, smartEffort, fastEffort)
}

// callSetSessionModels marshals the payload, invokes the handler on a
// newTestClient (whose c.reply is a synchronous non-blocking send into a
// buffered channel, so the reply is already queued when the handler
// returns), and returns the reply carrying reqID.
func callSetSessionModels(t *testing.T, ctx context.Context, a *appPkg.App, reqID string, payload SetSessionModelsPayload) *WSMessage {
	t.Helper()
	client := newTestClient()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, client, WSMessage{ID: reqID, Type: CmdSetSessionModels, Payload: raw})
	for {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID != reqID {
				continue
			}
			return &env
		default:
			require.FailNowf(t, "handler must reply to the request", "no reply with ID %s queued", reqID)
			return nil
		}
	}
}

func TestHandleSetSessionModels_LaterStepFailureReportsPerFieldStatus(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p696-partial-failure")
	require.NoError(t, err)

	// Fail exactly the LAST group this payload asks for; every earlier
	// group must still land and be reported as applied.
	a.Sessions = &failOneUpdateService{Service: a.Sessions, failUpdateWorkerReviewerReasoningEffort: true}

	reply := callSetSessionModels(t, ctx, a, "req-696-partial", SetSessionModelsPayload{
		SessionID:   sess.ID,
		SmartModel:  &ModelOverrideWire{Provider: "smart-provider", Model: "smart-model", ReasoningEffort: "high"},
		FastModel:   &ModelOverrideWire{Provider: "fast-provider", Model: "fast-model"},
		WorkerModel: &ModelOverrideWire{Provider: "worker-provider", Model: "worker-model", ReasoningEffort: "medium"},
	})

	require.Equal(t, EventResponse, reply.Type,
		"a partial failure must still be an EventResponse, not EventError, so the client can inspect which field groups landed")
	var result SetSessionModelsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))

	require.ElementsMatch(t, []string{
		setModelsFieldSmartFast,
		setModelsFieldSmartFastEffort,
		setModelsFieldWorkerReviewer,
	}, result.Applied, "the three earlier groups genuinely landed and must be reported applied")
	require.ElementsMatch(t, []string{setModelsFieldWorkerReviewerEffort}, result.Failed,
		"the failed later step must be reported failed, not folded into an unqualified ok")
	require.Equal(t, map[string]string{
		setModelsFieldWorkerReviewerEffort: "simulated: worker/reviewer effort write failed",
	}, result.Errors)

	// Verify against real DB state: everything except the failed group landed.
	after, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-model", after.SmartModelID)
	require.Equal(t, "fast-model", after.FastModelID)
	require.Equal(t, "high", after.SmartModelReasoningEffort)
	require.Equal(t, "worker-model", after.WorkerModelID)
	require.Empty(t, after.WorkerModelReasoningEffort, "the failed effort write must not have landed")
}

func TestHandleSetSessionModels_EarlyStepFailureStillReportsAppliedFields(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p696-early-failure")
	require.NoError(t, err)

	a.Sessions = &failOneUpdateService{Service: a.Sessions, failUpdateModels: true}

	reply := callSetSessionModels(t, ctx, a, "req-696-early", SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmartModel: &ModelOverrideWire{Provider: "smart-provider", Model: "smart-model", ReasoningEffort: "high"},
	})

	// The old code replied a bare EventError here, hiding that independent
	// groups were never attempted or (for later groups) had landed.
	require.Equal(t, EventResponse, reply.Type)
	var result SetSessionModelsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))
	require.ElementsMatch(t, []string{setModelsFieldSmartFastEffort}, result.Applied,
		"groups after the failed one are still attempted and reported")
	require.ElementsMatch(t, []string{setModelsFieldSmartFast}, result.Failed)

	after, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelID, "the failed model write must not have landed")
	require.Equal(t, "high", after.SmartModelReasoningEffort, "the independent effort write still landed")
}

func TestHandleSetSessionModels_FullSuccessReportsAllAppliedNoneFailed(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p696-full-success")
	require.NoError(t, err)

	reply := callSetSessionModels(t, ctx, a, "req-696-ok", SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmartModel: &ModelOverrideWire{Provider: "smart-provider", Model: "smart-model", ReasoningEffort: "high"},
		FastModel:  &ModelOverrideWire{Provider: "fast-provider", Model: "fast-model", ReasoningEffort: "low"},
	})

	require.Equal(t, EventResponse, reply.Type)
	var result SetSessionModelsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))
	require.ElementsMatch(t, []string{setModelsFieldSmartFast, setModelsFieldSmartFastEffort}, result.Applied)
	require.Empty(t, result.Failed, "control: nothing failed, Failed must be empty")
	require.Empty(t, result.Errors, "control: nothing failed, Errors must be empty")

	after, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-model", after.SmartModelID)
	require.Equal(t, "fast-model", after.FastModelID)
	require.Equal(t, "high", after.SmartModelReasoningEffort)
	require.Equal(t, "low", after.FastModelReasoningEffort)
}

func TestHandleSetSessionModels_EffortClearSentinelVersusOmission(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "p696-clear-vs-omit")
	require.NoError(t, err)

	// Seed both effort pairs directly through the real service.
	require.NoError(t, a.Sessions.UpdateReasoningEffort(ctx, sess.ID, "high", "medium"))
	require.NoError(t, a.Sessions.UpdateWorkerReviewerReasoningEffort(ctx, sess.ID, "high", "low"))

	// (a) OMIT: a smart-only effort change must preserve fast's stored
	// effort, not clobber it — the pre-existing backfill behaviour the
	// flash-loop comment in the handler guards.
	reply := callSetSessionModels(t, ctx, a, "req-696-omit", SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmartModel: &ModelOverrideWire{Provider: "p1", Model: "m1", ReasoningEffort: "low"},
	})
	require.Equal(t, EventResponse, reply.Type)
	var result SetSessionModelsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))
	require.Empty(t, result.Failed)
	after, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "low", after.SmartModelReasoningEffort)
	require.Equal(t, "medium", after.FastModelReasoningEffort, "an omitted effort must preserve the stored value")

	// (b) CLEAR: the sentinel must reset smart's effort to unset while the
	// again-omitted fast side stays preserved — the state an empty value
	// could not express before task #696.
	reply = callSetSessionModels(t, ctx, a, "req-696-clear", SetSessionModelsPayload{
		SessionID:  sess.ID,
		SmartModel: &ModelOverrideWire{Provider: "p1", Model: "m1", ReasoningEffort: ReasoningEffortClear},
	})
	require.Equal(t, EventResponse, reply.Type)
	result = SetSessionModelsResult{}
	require.NoError(t, json.Unmarshal(reply.Payload, &result))
	require.Empty(t, result.Failed)
	require.Contains(t, result.Applied, setModelsFieldSmartFastEffort)
	after, err = a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelReasoningEffort, "the clear sentinel must reset the effort to unset")
	require.Equal(t, "medium", after.FastModelReasoningEffort, "the omitted side must still be preserved through a clear on the other side")

	// (c) CLEAR on the worker/reviewer pair too, reviewer omitted+preserved.
	reply = callSetSessionModels(t, ctx, a, "req-696-clear-wr", SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{Provider: "wp", Model: "wm", ReasoningEffort: ReasoningEffortClear},
	})
	require.Equal(t, EventResponse, reply.Type)
	after, err = a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.WorkerModelReasoningEffort, "worker effort must be explicitly cleared")
	require.Equal(t, "low", after.ReviewerModelReasoningEffort, "reviewer effort must be preserved when omitted")
}
