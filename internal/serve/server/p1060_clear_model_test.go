package server

// Task #1060: set_session_models must be able to clear a role's
// model+provider back to "not set" (inherit the config default), not just
// its reasoning effort (already covered by p696's ReasoningEffortClear
// tests). Distinguishing "don't touch" from "clear" for provider/model
// relies on the SAME nil-vs-non-nil pointer convention the handler already
// uses for partial updates: a caller sends a non-nil ModelOverrideWire with
// empty Provider/Model to explicitly reset the slot, versus omitting the
// key entirely (nil) to leave it untouched.
//
// Also pins backward compatibility: an old client that only ever sends
// smartModel/fastModel (no workerModel/reviewerModel keys at all) must
// leave worker/reviewer completely untouched.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetSessionModels_ClearsWorkerModelBackToUnset(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p1060-clear-worker")
	require.NoError(t, err)

	setPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{Provider: "worker-provider", Model: "worker-model", ReasoningEffort: "high"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: setPayload})

	seeded, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "worker-model", seeded.WorkerModelID, "fixture sanity: the worker override must have landed")

	// A non-nil ModelOverrideWire with empty provider/model + the effort
	// clear sentinel is a full reset request, distinct from omitting the
	// key (which the sibling p466 tests prove leaves the slot untouched).
	clearPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:   sess.ID,
		WorkerModel: &ModelOverrideWire{ReasoningEffort: ReasoningEffortClear},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetSessionModels, Payload: clearPayload})

	cleared, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, cleared.WorkerModelProvider, "provider must be reset to unset")
	require.Empty(t, cleared.WorkerModelID, "model must be reset to unset")
	require.Empty(t, cleared.WorkerModelReasoningEffort, "effort must be reset to unset")
}

func TestSetSessionModels_ClearsReviewerModelBackToUnset(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p1060-clear-reviewer")
	require.NoError(t, err)

	setPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:     sess.ID,
		ReviewerModel: &ModelOverrideWire{Provider: "reviewer-provider", Model: "reviewer-model"},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: setPayload})

	clearPayload, err := json.Marshal(SetSessionModelsPayload{
		SessionID:     sess.ID,
		ReviewerModel: &ModelOverrideWire{},
	})
	require.NoError(t, err)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c2", Type: CmdSetSessionModels, Payload: clearPayload})

	cleared, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, cleared.ReviewerModelProvider)
	require.Empty(t, cleared.ReviewerModelID)
}

// TestSetSessionModels_OldSmartFastOnlyPayloadLeavesWorkerReviewerUntouched
// pins backward compatibility: a payload built the way a pre-#1060 client
// would build it (SmartModel/FastModel only, WorkerModel/ReviewerModel keys
// entirely absent from the JSON) must not create or alter a worker/reviewer
// override on the session.
func TestSetSessionModels_OldSmartFastOnlyPayloadLeavesWorkerReviewerUntouched(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	sess, err := a.Sessions.Create(ctx, "p1060-backward-compat")
	require.NoError(t, err)

	// Marshal a raw JSON object with only the pre-#1060 fields, so this test
	// cannot silently pass by relying on Go's zero-value nil pointer for a
	// field a modern client would still declare in its type.
	raw := []byte(`{"sessionID":"` + sess.ID + `","smartModel":{"provider":"smart-provider","model":"smart-model"},"fastModel":{"provider":"fast-provider","model":"fast-model"}}`)
	handleSetSessionModels(ctx, a, newTestClient(), WSMessage{ID: "c1", Type: CmdSetSessionModels, Payload: raw})

	after, err := a.Sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-model", after.SmartModelID)
	require.Equal(t, "fast-model", after.FastModelID)
	require.Empty(t, after.WorkerModelID, "an old-format payload must never touch the worker slot")
	require.Empty(t, after.ReviewerModelID, "an old-format payload must never touch the reviewer slot")
}
