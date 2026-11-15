package permission

// Epoch-binding semantics for the per-session restricted-run gate
// (round-2 SDK review R2-1): a stale run's deferred
// ClearSessionRunAllowlistForEpoch must never delete a newer owner's
// freshly armed policy, and legacy epoch-less entries keep their exact
// old behavior under the unconditional clear.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func epochTestAllowlist(t *testing.T, allowTools ...string) RunAllowlist {
	t.Helper()
	compiled, err := BuildRunAllowlist(RunAllowlistSpec{Restrict: true, AllowTools: allowTools})
	require.NoError(t, err)
	return compiled
}

func newEpochTestService(t *testing.T) *permissionService {
	t.Helper()
	svc, ok := NewPermissionService(t.Context(), "/tmp", false, nil, nil).(*permissionService)
	require.True(t, ok)
	// Auto-approve the test session so Request takes the restricted-run
	// gate branch (the branch the epoch binding protects).
	svc.AutoApproveSession("sess")
	return svc
}

func entryFor(t *testing.T, svc *permissionService, sessionID string) (sessionRunAllowlistEntry, bool) {
	t.Helper()
	svc.runAllowlistBySessionMu.RLock()
	defer svc.runAllowlistBySessionMu.RUnlock()
	entry, ok := svc.runAllowlistBySession[sessionID]
	return entry, ok
}

func TestClearSessionRunAllowlistForEpoch_EpochMatchRemovesEntry(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	svc.SetSessionRunAllowlistForEpoch("sess", epochTestAllowlist(t), 5)
	_, ok := entryFor(t, svc, "sess")
	require.True(t, ok, "entry must exist after SetSessionRunAllowlistForEpoch")

	svc.ClearSessionRunAllowlistForEpoch("sess", 5)
	_, ok = entryFor(t, svc, "sess")
	assert.False(t, ok, "epoch-matched clear must remove the entry")
}

func TestClearSessionRunAllowlistForEpoch_StaleClearKeepsNewerOwnerPolicy(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	// Era 5 arms a policy that denies "view"; era 6 (the newer owner)
	// re-arms a policy that allows it. The stale era-5 cleanup must not
	// touch era 6's entry.
	svc.SetSessionRunAllowlistForEpoch("sess", epochTestAllowlist(t), 5)
	svc.SetSessionRunAllowlistForEpoch("sess", epochTestAllowlist(t, "view"), 6)
	svc.ClearSessionRunAllowlistForEpoch("sess", 5)

	entry, ok := entryFor(t, svc, "sess")
	require.True(t, ok, "stale clear must never delete a newer owner's entry")
	assert.Equal(t, uint64(6), entry.ownerEpoch)

	allowed, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "sess",
		ToolName:  "view",
		Action:    "read",
	})
	require.NoError(t, err)
	assert.True(t, allowed, "the newer owner's allow-view policy must still be in force after the stale clear")
}

func TestClearSessionRunAllowlistForEpoch_LegacyEntryUnaffectedByEpochClear(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)

	svc.SetSessionRunAllowlist("sess", epochTestAllowlist(t))
	svc.ClearSessionRunAllowlistForEpoch("sess", 7)
	_, ok := entryFor(t, svc, "sess")
	assert.True(t, ok, "epoch 0 (legacy) entries must not be matched by a reserved run's epoch-bound clear")

	svc.ClearSessionRunAllowlist("sess")
	_, ok = entryFor(t, svc, "sess")
	assert.False(t, ok, "legacy unconditional clear must still remove a legacy entry")
}

func TestInheritSessionRunAllowlist_CopiesEpochBoundEntry(t *testing.T) {
	t.Parallel()
	svc := newEpochTestService(t)
	svc.AutoApproveSession("child")

	svc.SetSessionRunAllowlistForEpoch("parent", epochTestAllowlist(t, "view"), 42)
	svc.InheritSessionRunAllowlist("parent", "child")

	entry, ok := entryFor(t, svc, "child")
	require.True(t, ok, "child must inherit the parent's gate")
	assert.Equal(t, uint64(42), entry.ownerEpoch, "inheritance copies the whole entry, epoch included")

	allowed, err := svc.Request(t.Context(), CreatePermissionRequest{
		SessionID: "child",
		ToolName:  "view",
		Action:    "read",
	})
	require.NoError(t, err)
	assert.True(t, allowed, "inherited gate must be queryable for the child session")
}
