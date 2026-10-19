package app

// R12-H2b new-epoch tests: an EXPLICIT per-run DiskProvider — custom or
// the canonical OS provider (sdk.OSDiskProvider()) — starts a NEW
// execution epoch for the session. The explicit OS run must pass the
// turn guard despite a stale stored fence, and ExecuteRun must supersede
// (clear) the stored fence under local exclusive admission (the in-process
// mailbox reservation; the cross-process session lock is taken later in the
// turn, exactly as for the existing provider-run fence write) so later
// provider-less runs follow the new epoch's ordinary OS-disk path. A run
// with NO explicit provider on a fenced session still fails closed —
// the ordinary/unmarked path is never downgraded to the real disk
// (sdk/README.md:362-368).
//
// Mutations caught (and not caught):
//   - Deleting ExecuteRun's supersede branch leaves the stale fence
//     after the OS run (Test 1's fence-nil assertion fails) and the
//     provider-less follow-up then fails with the dead-binding error
//     (Test 1's follow-up NoError fails).
//   - Reverting ONLY the guard's explicit-OS pass is NOT caught here: the
//     supersede branch has already deleted the fence by the time the guard
//     runs, so the guard finds no fence. That mutation is caught by the
//     direct guard case in coordinator_run_queue_fence_test.go.
//   - Weakening the provider-less refusal (Test 2): the fixture names a
//     dead binding, so PrepareContinuation already fails with
//     ErrDiskProviderUnavailable before the guard's final empty-binding
//     refusal; Test 2 pins the never-downgrade outcome (typed refusal,
//     fence untouched), not that specific line.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestExplicitOSRunSupersedesStaleFence drives the exact CI regression
// shape of sdk TestSdkProviderRunShapesIsolation's third run: run 1 is a
// custom-provider run whose binding is pruned on return (no hold taken),
// so the stored fence names a dead binding; an explicit OSDiskProvider()
// run on the same session must still succeed, supersede the fence, and
// let a following provider-less run take the ordinary OS-disk path.
func TestExplicitOSRunSupersedesStaleFence(t *testing.T) {
	ctx := context.Background()
	application, sessionID, _, _ := runLiveProviderTurn(t,
		"OS_EPOCH_MARKER", func() string { return "unused" }, "")
	loader := fenceLoader(t, application)

	stale, err := loader.LoadSessionExecutionRequirement(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, stale,
		"the custom-provider run must have left a stored fence before the OS epoch")

	dir := filepath.Join(application.config.WorkingDir(), "scope")
	osRes, err := application.ExecuteRun(ctx, RunRequest{
		Prompt: "write probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
			DiskProvider: tools.OSDisk(),
		},
	})
	require.NoError(t, err, "an explicit OSDiskProvider() run starts a new epoch and must pass the stale-fence guard")
	require.NotNil(t, osRes)

	loaded, err := loader.LoadSessionExecutionRequirement(ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, loaded,
		"the explicit OS run must supersede (clear) the stored fence under exclusive admission")

	// With the fence gone the session is unfenced: a provider-less run
	// must follow the ordinary OS-disk path instead of failing closed.
	res, err := application.ExecuteRun(ctx, RunRequest{
		Prompt: "write probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
		},
	})
	require.NoError(t, err, "the superseded session must be unfenced: a provider-less run follows the ordinary OS-disk path")
	require.NotNil(t, res)

	loaded, err = loader.LoadSessionExecutionRequirement(ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, loaded,
		"the provider-less run persists no fence and must not resurrect the superseded one")
}

// TestFencedSessionProviderlessRunStillRefused is the never-downgrade
// oracle (sdk/README.md:362-368): a run with NO explicit provider on a
// fenced session is refused typed — it lands in NEITHER ExecuteRun
// admission branch and neither passes nor supersedes the fence.
func TestFencedSessionProviderlessRunStillRefused(t *testing.T) {
	ctx := context.Background()
	application, sessionID, _, _ := runLiveProviderTurn(t,
		"NOEPOCH_MARKER", func() string { return "unused" }, "")
	loader := fenceLoader(t, application)

	stale, err := loader.LoadSessionExecutionRequirement(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, stale, "the fixture must start from a fenced session")

	dir := filepath.Join(application.config.WorkingDir(), "scope")
	// JSON mode returns an error envelope alongside the error, so the result
	// is not asserted nil.
	_, err = application.ExecuteRun(ctx, RunRequest{
		Prompt: "write probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
		},
	})
	require.Error(t, err, "a provider-less run on a fenced session must fail closed")
	require.ErrorIs(t, err, agent.ErrDiskProviderUnavailable)

	loaded, err := loader.LoadSessionExecutionRequirement(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, loaded,
		"the refused run must not have superseded or rewritten the stored fence")
}
