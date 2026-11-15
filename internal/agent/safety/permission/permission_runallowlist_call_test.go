package permission

// Call-binding semantics for the per-session restricted-run gate
// (round-3 SDK review R3-4): ClearSessionRunAllowlistForCall must delete
// an entry only when the stored ownerCallID matches — a stale clear from
// an old call, or a clear from the unrelated epoch-bound mechanism, must
// never remove a newer turn's policy.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearSessionRunAllowlistForCall_CallMatchRemovesEntry(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	svc.SetSessionRunAllowlistForCall("sess", epochTestAllowlist(t), "call-1")
	_, ok := entryFor(t, svc, "sess")
	require.True(t, ok, "entry must exist after SetSessionRunAllowlistForCall")

	svc.ClearSessionRunAllowlistForCall("sess", "call-1")
	_, ok = entryFor(t, svc, "sess")
	assert.False(t, ok, "an ownerCallID-matched clear must remove the entry")
}

func TestClearSessionRunAllowlistForCall_StaleCallClearKeepsNewerTurnPolicy(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	// call-1 arms a policy that denies "view"; call-2 (the newer turn)
	// re-arms a policy that allows it. call-1's stale cleanup must not
	// touch call-2's entry.
	svc.SetSessionRunAllowlistForCall("sess", epochTestAllowlist(t), "call-1")
	svc.SetSessionRunAllowlistForCall("sess", epochTestAllowlist(t, "view"), "call-2")
	svc.ClearSessionRunAllowlistForCall("sess", "call-1")

	entry, ok := entryFor(t, svc, "sess")
	require.True(t, ok, "stale clear must never delete a newer turn's entry")
	assert.Equal(t, "call-2", entry.ownerCallID)

	allowed, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "sess",
		ToolName:  "view",
		Action:    "read",
	})
	require.NoError(t, err)
	assert.True(t, allowed, "the newer turn's allow-view policy must still be in force after the stale clear")
}

func TestClearSessionRunAllowlistForCall_DoesNotMatchOtherBinding(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	// Epoch-bound entry: ownerCallID is "" so a call-scoped clear with
	// "call-1" must not match it.
	svc.SetSessionRunAllowlistForEpoch("sess", epochTestAllowlist(t), 5)
	svc.ClearSessionRunAllowlistForCall("sess", "call-1")
	_, ok := entryFor(t, svc, "sess")
	assert.True(t, ok, "call-scoped clear must not remove an epoch-bound entry (ownerCallID mismatch)")

	// Call-bound entry: ownerEpoch is 0 so an epoch-scoped clear with 5
	// must not match it either.
	svc.SetSessionRunAllowlistForCall("sess", epochTestAllowlist(t), "call-1")
	svc.ClearSessionRunAllowlistForEpoch("sess", 5)
	entry, ok := entryFor(t, svc, "sess")
	assert.True(t, ok, "epoch-scoped clear must not remove a call-bound entry (ownerEpoch mismatch)")
	assert.Equal(t, "call-1", entry.ownerCallID)
}
