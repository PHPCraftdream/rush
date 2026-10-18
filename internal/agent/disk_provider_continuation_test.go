package agent

// CI-only tests for the agent-side continuation core (R12-H2b §2.3).
// Each test names the production mutation it catches: deleting or
// weakening the guarded code path would let a continuation silently
// run on OS disk, adopt the wrong provider, leak an owner or drop the
// requirement on a durable row.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFenceStore is a tiny in-memory session fence store; err simulates
// a DB/read failure so tests can prove unreadable fences fail closed.
type fakeFenceStore struct {
	mu   sync.Mutex
	rows map[string]session.SessionExecutionRequirement
	err  error
}

func (f *fakeFenceStore) StoreSessionExecutionRequirement(_ context.Context, sessionID string, req session.SessionExecutionRequirement) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = make(map[string]session.SessionExecutionRequirement)
	}
	f.rows[sessionID] = req
	return nil
}

func (f *fakeFenceStore) LoadSessionExecutionRequirement(_ context.Context, sessionID string) (*session.SessionExecutionRequirement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if req, ok := f.rows[sessionID]; ok {
		return &req, nil
	}
	return nil, nil
}

// continuationOwnerCount derives the Continuation owner count from the
// manager's owner set (the diagnostic must never be a stored counter).
func continuationOwnerCount(m *diskCapabilityManager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.owners {
		if key.kind == diskOwnerContinuation {
			n++
		}
	}
	return n
}

// callOwnerCount derives the Call owner count from the owner set.
func callOwnerCount(m *diskCapabilityManager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.owners {
		if key.kind == diskOwnerCall {
			n++
		}
	}
	return n
}

func newContinuationTestCoordinator(store session.SessionExecutionRequirementStore, caps *DiskProviderRegistry) *coordinator {
	return &coordinator{execReqs: store, diskCaps: caps}
}

// TestPrepareContinuation_ResolvesLiveFence catches deleting the fence
// resolution or the Continuation owner acquisition: a continuation would
// then lose its provider identity or the binding would be pruned under
// it. Also pins release idempotence — a double release must not delete
// another owner's retention.
func TestPrepareContinuation_ResolvesLiveFence(t *testing.T) {
	caps := newDiskCapabilityManager()
	provider := newFakeDiskProvider(map[string]string{"a.txt": "hi"})
	bindingID, err := caps.RegisterProvider(provider)
	require.NoError(t, err)
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(t.Context(), "s1", session.SessionExecutionRequirement{BindingID: bindingID}))
	coord := newContinuationTestCoordinator(store, caps)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", nil)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	assert.Equal(t, bindingID, prepared.requirement.BindingID)
	assert.True(t, reflect.DeepEqual(provider, prepared.provider),
		"prepared provider must be the registered provider object")
	assert.Equal(t, 1, continuationOwnerCount(caps), "exactly one Continuation owner")

	prepared.release()
	assert.Equal(t, 0, continuationOwnerCount(caps))
	prepared.release() // idempotent: a second release must be a no-op
	assert.Equal(t, 0, continuationOwnerCount(caps))
}

// TestPrepareContinuation_SourceIdentityWinsOverNewerFence catches the
// redirect-to-latest-provider regression (design §2.2): a new fence
// epoch naming B2 must never redirect an existing continuation whose
// source envelope names B1.
func TestPrepareContinuation_SourceIdentityWinsOverNewerFence(t *testing.T) {
	caps := newDiskCapabilityManager()
	idB1, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	idB2, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(t.Context(), "s1", session.SessionExecutionRequirement{BindingID: idB2}))
	coord := newContinuationTestCoordinator(store, caps)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", &session.SessionExecutionRequirement{BindingID: idB1})
	require.NoError(t, err)
	require.NotNil(t, prepared)
	assert.Equal(t, idB1, prepared.requirement.BindingID,
		"source envelope identity must be preserved over the newer fence")
	prepared.release()
}

// TestPrepareContinuation_LostBindingFailsClosed catches a lost binding
// being tolerated (OS-disk promotion): an unregistered binding must
// return ErrDiskProviderUnavailable and must not leak a Continuation
// owner on the failed path.
func TestPrepareContinuation_LostBindingFailsClosed(t *testing.T) {
	caps := newDiskCapabilityManager()
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(t.Context(), "s1",
		session.SessionExecutionRequirement{BindingID: "never-registered"}))
	coord := newContinuationTestCoordinator(store, caps)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", nil)
	require.Nil(t, prepared)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
	assert.Equal(t, 0, continuationOwnerCount(caps),
		"a failed resolution must not leak a Continuation owner")
}

// TestPrepareContinuation_UnreadableFenceFailsClosed catches a DB/read
// error degrading to "no fence": the load error must wrap
// ErrDiskProviderUnavailable so the continuation fails closed instead of
// running a provider-bound call on OS disk.
func TestPrepareContinuation_UnreadableFenceFailsClosed(t *testing.T) {
	caps := newDiskCapabilityManager()
	store := &fakeFenceStore{err: errors.New("disk I/O error")}
	coord := newContinuationTestCoordinator(store, caps)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", nil)
	require.Nil(t, prepared)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable,
		"the load error's wrap must stay classifiable as unavailable")
	assert.Equal(t, 0, continuationOwnerCount(caps))
}

// TestPrepareContinuation_NoFenceNoSourceReturnsNil catches a spurious
// requirement on the ordinary path: no fence and no source means (nil,
// nil) with no owner acquired.
func TestPrepareContinuation_NoFenceNoSourceReturnsNil(t *testing.T) {
	caps := newDiskCapabilityManager()
	coord := newContinuationTestCoordinator(&fakeFenceStore{}, caps)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", nil)
	require.NoError(t, err)
	assert.Nil(t, prepared)
	assert.Equal(t, 0, caps.ownerCount(), "no owner may be acquired")
}

// TestPrepareContinuation_MissingCapabilityManagerFailsClosed catches a
// fixture/flag-off coordinator with a fence but no bound manager
// resolving as "no requirement": it must fail closed typed.
func TestPrepareContinuation_MissingCapabilityManagerFailsClosed(t *testing.T) {
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(t.Context(), "s1",
		session.SessionExecutionRequirement{BindingID: "b1"}))
	coord := newContinuationTestCoordinator(store, nil)

	prepared, err := coord.PrepareContinuation(t.Context(), "s1", nil)
	require.Nil(t, prepared)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
}

// TestBindExistingCallDiskBinding_OwnerSetSemantics catches premature
// binding pruning: the binding must survive while BOTH its Call owner
// and a separate Continuation owner live, and die only once every owner
// releases. Also covers the unknown-binding fail-closed path.
func TestBindExistingCallDiskBinding_OwnerSetSemantics(t *testing.T) {
	caps := newDiskCapabilityManager()
	provider := newFakeDiskProvider(nil)
	bindingID, err := caps.RegisterProvider(provider)
	require.NoError(t, err)

	opts, releaseCall, err := bindExistingCallDiskBinding(caps, bindingID, "call-1", nil)
	require.NoError(t, err)
	assert.Equal(t, bindingID, opts.DiskBindingID)
	assert.Equal(t, 1, callOwnerCount(caps), "exactly one Call owner")

	// Unknown binding fails closed with the typed error.
	_, releaseUnknown, err := bindExistingCallDiskBinding(caps, "missing", "call-2", nil)
	require.Nil(t, releaseUnknown)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)

	// A separate continuation owner keeps the binding alive too.
	key := diskOwnerKey{kind: diskOwnerContinuation, instance: "cont-1", bindingID: bindingID}
	require.NoError(t, caps.acquire(key))
	releaseCall()
	releaseCall() // idempotent
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "binding must survive while the continuation owner lives")
	caps.release(key)
	_, err = caps.Lookup(bindingID)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable,
		"binding must die once every owner releases (no premature prune)")
}

// TestReleaseResidualContinuations_KeepsCallOwners catches a graceful
// shutdown release that kills live execution retention: only
// Continuation owner keys may be dropped; call/construction owners keep
// the binding retained.
func TestReleaseResidualContinuations_KeepsCallOwners(t *testing.T) {
	caps := newDiskCapabilityManager()
	bindingID, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	require.NoError(t, caps.acquire(diskOwnerKey{kind: diskOwnerCall, instance: "call-1", bindingID: bindingID}))
	contKey := diskOwnerKey{kind: diskOwnerContinuation, instance: "cont-1", bindingID: bindingID}
	require.NoError(t, caps.acquire(contKey))

	caps.releaseResidualContinuations()
	assert.Equal(t, 0, continuationOwnerCount(caps))
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err,
		"binding must stay retained while its call owner lives")
	caps.release(diskOwnerKey{kind: diskOwnerCall, instance: "call-1", bindingID: bindingID})
	_, err = caps.Lookup(bindingID)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
}

// TestDiskRequirementForCall_Synthesis catches ToSessionAgentCallData
// dropping the requirement on durable rows: the stamped envelope wins,
// an unstamped binding synthesizes the full envelope, and no binding ID
// means nil. A dropped envelope would make a later restart fail closed
// instead of restoring.
func TestDiskRequirementForCall_Synthesis(t *testing.T) {
	// Stamped envelope wins outright.
	stamped := &session.SessionExecutionRequirement{BindingID: "stamped-1"}
	call := SessionAgentCall{DiskRequirement: stamped, CallOptions: &CallOptions{DiskBindingID: "other"}}
	assert.Same(t, stamped, diskRequirementForCall(call))

	// Options with binding ID + custom provider and no envelope
	// synthesize one from the trusted call state.
	scope := &permission.FolderScopeSpec{WorkingDir: "/w", KeepCommandTools: true}
	allow := &permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"fs_read"}}
	opts := &CallOptions{
		DiskBindingID:    "bind-9",
		DiskProvider:     newFakeDiskProvider(nil),
		DisableSubAgents: true,
		ModelRole:        config.SelectedModelTypeSmart,
	}
	call = SessionAgentCall{FolderScopeSpec: scope, RunAllowlistSpec: allow, CallOptions: opts}
	req := diskRequirementForCall(call)
	require.NotNil(t, req)
	assert.Equal(t, "bind-9", req.BindingID)
	require.NotNil(t, req.FolderScopeSpec)
	assert.Equal(t, "/w", req.FolderScopeSpec.WorkingDir)
	assert.True(t, req.FolderScopeSpec.KeepCommandTools)
	require.NotNil(t, req.RunAllowlistSpec)
	assert.True(t, req.RunAllowlistSpec.Restrict)
	assert.Equal(t, []string{"fs_read"}, req.RunAllowlistSpec.AllowTools)
	require.NotNil(t, req.CallPolicy)
	assert.True(t, req.CallPolicy.DisableSubAgents)
	assert.Equal(t, string(config.SelectedModelTypeSmart), req.CallPolicy.ModelRole)

	// Options with NO binding ID never synthesize.
	call = SessionAgentCall{CallOptions: &CallOptions{DiskProvider: newFakeDiskProvider(nil)}}
	assert.Nil(t, diskRequirementForCall(call),
		"a call without a minted binding has no requirement")
}

// TestSessionAgentCallData_DiskRequirementRoundTrip catches a
// To/FromSessionAgentCallData pair dropping the stamped envelope on the
// rebuilt call: a restart must see the same binding identity the
// original call carried, not inherit "the latest provider".
func TestSessionAgentCallData_DiskRequirementRoundTrip(t *testing.T) {
	req := &session.SessionExecutionRequirement{BindingID: "bind-rt", Epoch: 3}
	call := SessionAgentCall{SessionID: "s1", Prompt: "go", DiskRequirement: req}
	data := ToSessionAgentCallData(call)
	require.NotNil(t, data.DiskRequirement)
	assert.Equal(t, "bind-rt", data.DiskRequirement.BindingID)
	assert.Equal(t, int64(3), data.DiskRequirement.Epoch)

	rebuilt, err := FromSessionAgentCallData(data)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.DiskRequirement)
	assert.Equal(t, "bind-rt", rebuilt.DiskRequirement.BindingID)
	assert.Equal(t, int64(3), rebuilt.DiskRequirement.Epoch)
}

// TestRequirementFromCall_AliasMatchesSynthesis pins the thin alias to
// diskRequirementForCall so producer call sites and the durable
// serializer can never diverge.
func TestRequirementFromCall_AliasMatchesSynthesis(t *testing.T) {
	opts := &CallOptions{DiskBindingID: "bind-a", DiskProvider: newFakeDiskProvider(nil)}
	call := SessionAgentCall{CallOptions: opts}
	got := requirementFromCall(call)
	require.NotNil(t, got)
	assert.Equal(t, "bind-a", got.BindingID)
}

// Compile-time guards: the fakes must keep satisfying the interfaces
// this chunk depends on.
var (
	_ session.SessionExecutionRequirementStore = (*fakeFenceStore)(nil)
	_ tools.DiskProvider                       = (*fakeDiskProvider)(nil)
)
