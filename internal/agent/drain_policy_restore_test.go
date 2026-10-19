package agent

// R12-H2b fence-policy tests (audit gh#10b, gh#11b, gh#10d): the root
// drain must restore the persisted policy baseline from the session
// execution fence, must not leak the preparedContinuation's Continuation
// owner on any failure path, and RunSessionAgentCall must release a
// transferred Call owner even when the ready gate fails before the turn.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// wireRootDrainFence installs a fenced execution requirement onto the
// fixture's coordinator: a fake provider registered in a real capability
// manager, a durable fence store carrying the binding plus a folder-scope,
// restricted-run and call-policy baseline. The fixture's wiring mirrors
// TestDrainCallFor_RootCallCarriesFreshCredentials (drain_unauthorized_test.go):
// a real config via the oauth provider and no driver, so drainCallFor takes
// the ROOT branch. Tests using this helper must not be parallel (the fixture
// isolates the global config paths with t.Setenv).
func wireRootDrainFence(t *testing.T, f *attemptFixture) (bindingID string, caps *diskCapabilityManager) {
	t.Helper()
	caps = newDiskCapabilityManager()
	provider := newFakeDiskProvider(nil)
	var err error
	bindingID, err = caps.RegisterProvider(provider)
	require.NoError(t, err)
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(context.Background(), f.sessID, session.SessionExecutionRequirement{
		BindingID: bindingID,
		FolderScopeSpec: &session.FolderScopeSpec{
			WorkingDir: f.env.workingDir,
			Entries:    []session.FolderScopeEntry{{Dir: ".", Ops: []session.FileOp{"read"}}},
		},
		RunAllowlistSpec: &session.RunAllowlistSpec{Restrict: true, AllowTools: []string{"fs_read"}},
		CallPolicy:       &session.CallOptionsSpec{Version: session.CallOptionsSpecVersion, DisableSubAgents: true},
	}))
	f.coord.execReqs = store
	f.coord.diskCaps = caps
	return bindingID, caps
}

// TestDrainCallFor_RestoresFencePolicyBaseline (gh#11b): the root drain
// restores the fence's persisted policy baseline — the compiled folder scope
// canonicalized against the fence's own provider, the restricted-run policy
// (compiled AND spec), and the call policy — onto the call.
//
// WHY IT FAILS ON OLD CODE: the old root branch built empty CallOptions and
// restored the provider/binding/requirement only, so FolderScope, the run
// policy and DisableSubAgents were all silently dropped.
func TestDrainCallFor_RestoresFencePolicyBaseline(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "drain-fence-policy", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	bindingID, caps := wireRootDrainFence(t, f)

	call, prep, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	require.NotNil(t, prep)

	// Folder scope: compiled, and canonicalized through the SAME live
	// provider the fence named, granting exactly the declared read op.
	require.NotNil(t, call.CallOptions.FolderScope)
	require.True(t, call.CallOptions.FolderScope.Grants(permission.FileOpRead))

	// Restricted-run policy: spec restored, compiled allowlist armed on the
	// call for turn-start enforcement.
	require.NotNil(t, call.RunAllowlistSpec)
	require.True(t, call.RunAllowlistSpec.Restrict)
	require.NotNil(t, call.RunAllowlist)

	// Call policy restored onto the same CallOptions value.
	require.True(t, call.CallOptions.DisableSubAgents)
	require.Equal(t, bindingID, call.CallOptions.DiskBindingID)

	// The uncompiled folder-scope spec travels on the call for durable
	// re-serialization.
	require.NotNil(t, call.FolderScopeSpec)

	// Dispatch-level oracle (mirrors rebuild_folderscope_spec_test.go:65-73):
	// the RESTORED scope must adjudicate checks, not merely exist — granted
	// op inside passes, ungranted op and outside paths are typed denials.
	inside := filepath.Join(f.env.workingDir, "restored-probe.txt")
	require.NoError(t, call.CallOptions.FolderScope.Check(inside, permission.FileOpRead),
		"the read op granted by the restored fence scope must allow reads inside the scoped subtree")
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(inside, permission.FileOpDelete), &denied,
		"the restored fence scope grants READ only: any other op must be a typed denial")
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(
		filepath.Join(filepath.Dir(f.env.workingDir), "outside.txt"), permission.FileOpRead), &denied,
		"a path outside every scoped subtree must be a typed denial")

	// Constructed-toolset oracle: the pinned call.Tools must OMIT the command
	// tools the restored scope excludes (KeepsCommandTools is false) and
	// carry the granted fs_read. pinnedToolNames / assertToolsOmit /
	// assertToolsContain are the package's existing helpers
	// (coordinator_tool_pinning_test.go).
	require.NotNil(t, call.Tools)
	require.NotEmpty(t, call.Tools)
	names := pinnedToolNames(call.Tools)
	assertToolsOmit(t, names, "bash", "run_command", "job_output", "job_kill")
	assertToolsContain(t, names, tools.FSReadToolName)

	// Deny-by-dispatch: the constructed fs_read tool itself must refuse a
	// path outside the restored scope (the same RunFSBatch Scope.Check the
	// real dispatch performs), using the existing runFSRead probe
	// (coordinator_disk_provider_test.go) and fsReadToolFrom.
	resp := runFSRead(t, fsReadToolFrom(t, call.Tools),
		filepath.Join(filepath.Dir(f.env.workingDir), "outside-probe.txt"))
	require.True(t, resp.IsError, "an fs_read outside the restored scope must be a tool error: %s", resp.Content)

	prep.release()
	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 0, owners, "releasing the prep token must release the Continuation owner")
}

// fenceFailingGetSessions fails the session load deterministically.
type fenceFailingGetSessions struct {
	session.Service
}

func (s *fenceFailingGetSessions) Get(context.Context, string) (session.Session, error) {
	return session.Session{}, errors.New("db down")
}

// failSecondGetSessions serves the FIRST sessions.Get from the real service
// and fails every later one, so the FIRST resolveSessionModels inside
// drainCallFor succeeds (building the per-call toolset) and the post-refresh
// SECOND resolve fails at its first step (coordinator_models.go:103-105)
// without reaching the tool build again. session.Service.Get:
// `Get(ctx context.Context, id string) (Session, error)`
// (internal/session/session.go:168). At the failure moment it snapshots the
// construction manager's owner diagnostics, so the test can prove the
// Construction owner was LIVE when the failure happened (reachability oracle:
// an earlier failure — e.g. pinCallTools' own fail() at
// coordinator_models_call_tools.go:106-120, which closes the call scope by
// itself — cannot satisfy the assertions below).
type failSecondGetSessions struct {
	session.Service
	gets atomic.Int64
	caps *diskCapabilityManager

	mu                     sync.Mutex
	ownersAtFailure        int
	constructionsAtFailure int
}

// errSecondResolve is the sentinel the injected second-Get failure returns;
// drainCallFor surfaces it wrapped ("failed to load session %q: %w" at
// coordinator_models.go:103-105), so errors.Is matches.
var errSecondResolve = errors.New("db down (second resolve)")

func (s *failSecondGetSessions) Get(ctx context.Context, id string) (session.Session, error) {
	if s.gets.Add(1) == 1 {
		return s.Service.Get(ctx, id)
	}
	if s.caps != nil {
		owners, constrs, _ := s.caps.OwnerDiagnostics()
		s.mu.Lock()
		s.ownersAtFailure, s.constructionsAtFailure = owners, constrs
		s.mu.Unlock()
	}
	return session.Session{}, errSecondResolve
}

// snapshotAtFailure returns the owner diagnostics captured when the injected
// second Get failed.
func (s *failSecondGetSessions) snapshotAtFailure() (gets, owners, constrs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets.Load(), int64(s.ownersAtFailure), int64(s.constructionsAtFailure)
}

// TestDrainCallFor_ReleasesContinuationOwnerWhenModelResolutionFails
// (gh#10b): a root-drain failure AFTER the fence was resolved — here the
// session load inside resolveSessionModels (coordinator_models.go:
// `sess, err := c.sessions.Get(ctx, sessionID)`, the first fallible step of
// the root branch's model resolution) — must release the prepared
// Continuation owner instead of returning with it still pinned.
//
// WHY IT FAILS ON OLD CODE: the old root branch returned the resolution
// error directly; the preparedContinuation owner leaked past the error
// return, so the manager still holds one owner and the final assertion
// fails (owners == 1).
func TestDrainCallFor_ReleasesContinuationOwnerWhenModelResolutionFails(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "drain-fence-leak", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	_, caps := wireRootDrainFence(t, f)
	f.coord.sessions = &fenceFailingGetSessions{Service: f.env.sessions}

	_, prep, err := f.coord.drainCallFor(ctx, f.sessID)
	require.Error(t, err)
	require.Nil(t, prep)
	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 0, owners, "the failed root drain must release the Continuation owner")
}

// TestRunSessionAgentCall_ReadyGateFailureReleasesTransferredOwner
// (gh#10d): a rebuilt call's transferred Call owner (registered on its
// build scope, mirroring RebuildSessionAgentCall's transfer) must be
// released when RunSessionAgentCall bails out on the ready gate.
//
// readyGate semantics (ready_gate.go): Go runs f in a counted goroutine and
// Wait blocks until no task is pending, returning the FIRST non-nil error
// any task reported — sticky, it keeps returning it on later calls.
//
// WHY IT FAILS ON OLD CODE: the scope-close defer sat AFTER the
// readyWg.Wait() return in RunSessionAgentCall, so the gate failure
// returned with the registered Call owner still live — owners == 1 and the
// assertion fails.
func TestRunSessionAgentCall_ReadyGateFailureReleasesTransferredOwner(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	caps := newDiskCapabilityManager()
	bindingID, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	opts, release, err := bindExistingCallDiskBinding(caps, bindingID, "call-1", nil)
	require.NoError(t, err)
	opts.buildScope.register(release) // mirrors RebuildSessionAgentCall's transfer (coordinator_run_queue_call.go:286)

	coord.readyWg.Go(func() error { return errors.New("gate failed") })
	require.Eventually(t, func() bool { return coord.readyWg.Wait() != nil },
		5*time.Second, 10*time.Millisecond, "the gate task must fail the gate")

	_, err = coord.RunSessionAgentCall(context.Background(), SessionAgentCall{
		SessionID: "s1", LogicalCallID: "l1", Prompt: "p", CallOptions: opts,
	})
	require.Error(t, err)
	owners, _, _ := caps.OwnerDiagnostics()
	require.Equal(t, 0, owners, "the transferred Call owner must be released when the ready gate fails")
}

// TestDrainCallFor_BuildCallFailureReleasesConstructionOwner (gh#10b,
// post-construction-pin failure): the first resolveSessionModels builds the
// per-call toolset under the batch binding, pinning a Construction owner
// into caps2 AND registering its release on capOpts.buildScope; the
// post-refresh second resolve then fails on sessions.Get — the ONE failure
// path where nothing but drainCallFor's abandon defer closes the call scope
// (pinCallTools' fail() is not reached: the second resolve dies on Get
// before building again).
//
// WHY IT FAILS ON OLD CODE: deleting capOpts.buildScope.close() from the
// abandon defer (agent_drain.go:112-114) leaves the Construction owner
// pinned — ownersConstr == 1 and the caps2 assertions fail; deleting
// prep.release() fails the fence-caps assertion.
//
// DOES NOT CATCH: failures before the first tool build (no pins exist), and
// the ready-gate path (pinCallTools' fail() closes the scope itself; the
// Call-owner half is covered by
// TestRunSessionAgentCall_ReadyGateFailureReleasesTransferredOwner).
//
// The exact-error + gets==2 + at-failure owners==1 assertions make an earlier
// failure (pinCallTools' fail() closing the scope itself) unable to satisfy
// the test.
func TestDrainCallFor_BuildCallFailureReleasesConstructionOwner(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	f := newAttemptFixture(t, "drain-construction-pin-leak", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	caps2 := newDiskCapabilityManager()
	prevKindOverride := batchLeafKindOverride
	t.Cleanup(func() { batchLeafKindOverride = prevKindOverride })
	require.NoError(t, BindCoordinatorBatchRuntimeBindings(f.coord, &fakeBatchIngress{}, nil, caps2))
	// AFTER the bind: wireRootDrainFence overwrites coord.diskCaps with the
	// fence caps; caps2 keeps only the Construction owner.
	_, fenceCaps := wireRootDrainFence(t, f)
	// The oauth fixture's expired token makes refreshExpiredToken refresh
	// (coordinator_providers_auth.go:30-37); succeed it like
	// drain_unauthorized_test.go's countRefreshes(nil).
	f.coord.refreshOAuth2TokenFn = func(context.Context, config.ProviderConfig) error { return nil }
	failing := &failSecondGetSessions{Service: f.env.sessions, caps: caps2}
	f.coord.sessions = failing

	_, prep, err := f.coord.drainCallFor(ctx, f.sessID)
	require.Error(t, err)
	require.Nil(t, prep)
	require.ErrorIs(t, err, errSecondResolve,
		"the failure must be the injected second-resolve Get error, not an earlier build failure")
	gets, ownersAtFailure, constrsAtFailure := failing.snapshotAtFailure()
	require.EqualValues(t, 2, gets,
		"exactly two Get calls: the first resolve and the post-refresh second resolve")
	require.EqualValues(t, 1, ownersAtFailure,
		"the Construction owner must be LIVE at the failure moment (reachability)")
	require.EqualValues(t, 1, constrsAtFailure,
		"the construction record must exist at the failure moment")

	ownersFence, _, _ := fenceCaps.OwnerDiagnostics()
	require.Equal(t, 0, ownersFence,
		"the failed root drain must release the fence's Continuation owner")
	ownersConstr, constrs, _ := caps2.OwnerDiagnostics()
	require.Equal(t, 0, ownersConstr,
		"the first resolve's Construction owner must be released by the drain's failure cleanup")
	require.Equal(t, 0, constrs,
		"the construction record must be pruned once its owner is released")
}
