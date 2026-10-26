package app

// R12-H2b new-epoch tests: an EXPLICIT per-run DiskProvider — custom or
// the canonical OS provider — starts a NEW execution epoch for the session.
// Fence mutations are performed only by the runOwned admission callback,
// after the inter-process lock and workspace check. Provider-less runs
// validate the persistent requirement and never delete it.

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

// TestExplicitOSRunSupersedesStaleFence drives the CI regression shape of an
// explicit OS epoch: a stale stored fence must be superseded under the session
// lock, and a following omitted-provider run validates rather than deletes it.
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
		"the explicit OS run must supersede the stored fence after lock admission")

	// A provider-less ExecuteRun must never mutate a fence; the no-resurrection
	// assertion here also pins the already-established OS epoch.
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
