package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRebuildSessionAgentCall_UnmarkedFenceRefusesAndOSClearedRowRebuilds
// pins that an omitted queued row is refused while fenced and can rebuild
// after the explicit OS admission cleared the session fence.
func TestRebuildSessionAgentCall_UnmarkedFenceRefusesAndOSClearedRowRebuilds(t *testing.T) {
	// Build the row with the production model snapshot, then install a fence
	// naming an unregistered binding to model process-local capability loss.
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	store := &fakeFenceStore{}
	coord.execReqs = store
	coord.diskCaps = newDiskCapabilityManager()
	cfg, _ := coord.cfg.Snapshot()
	smart := toSessionModelCfg(cfg.Models[config.SelectedModelTypeSmart])
	fast := toSessionModelCfg(cfg.Models[config.SelectedModelTypeFast])
	sessionID := "queue-fence-epoch"
	fence := session.SessionExecutionRequirement{BindingID: "binding-from-earlier-epoch"}
	require.NoError(t, store.StoreSessionExecutionRequirement(t.Context(), sessionID, fence))
	data := session.SessionAgentCallData{
		SessionID:     sessionID,
		LogicalCallID: "queue-fence-row",
		Prompt:        "queued work",
		SmartModel:    &smart,
		FastModel:     &fast,
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), "row-fenced", data)
	require.Error(t, err)
	require.Equal(t, SessionAgentCall{}, call)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
	unchanged, err := store.LoadSessionExecutionRequirement(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, unchanged)
	require.Equal(t, fence, *unchanged,
		"a rejected queued row must not clear or rewrite the session fence")

	// Row rebuilds do not possess explicit-intent authority and cannot delete
	// the persistent execution requirement.
	require.NoError(t, store.DeleteSessionExecutionRequirement(t.Context(), sessionID))
	call, err = coord.RebuildSessionAgentCall(t.Context(), "row-os-epoch", data)
	require.NoError(t, err)
	require.Equal(t, sessionID, call.SessionID)
	require.Nil(t, call.CallOptions,
		"the now-unfenced provider-less row rebuilds on the ordinary OS path")
}
