package sdk_test

// R12-F item 5 (SDK contract under RUSH_BATCH_RUNTIME ON):
//
//   - a custom RunOverrides.DiskProvider is refused at admission: the SDK
//     entry point (Client.Run -> app.ExecuteRun) surfaces the stable
//     sentinel sdk.ErrDiskProviderNotDurable (R11G's refusal, re-exported
//     at sdk.sdk.go) and NO provider traffic happens;
//   - cancelling a run's context works with the flag ON: Run returns a
//     context.Canceled-wrapped error and the Client stays usable.
//
// Revert-checks: dropping the flag-ON refusal in the coordinator's
// finishToolAssembly (internal/agent/coordinator_tools.go) fails
// TestSDKBatchRuntimeOnCustomDiskProviderRefusedNotDurable (the run would
// complete against the scripted provider and hits would be non-zero);
// breaking ctx propagation through the flag-ON tool assembly fails
// TestSDKBatchRuntimeOnRunContextCancellation.
//
// The flag-OFF disk-provider behavior is owned by sdk_disk_provider_test.go
// and is NOT touched here.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKBatchRuntimeOnCustomDiskProviderRefusedNotDurable pins the
// documented flag-ON contract: a custom DiskProvider never reaches the
// durable batch runtime, and the SDK entry point says so with the stable
// sentinel. Trace: Client.Run (sdk/sdk.go:562) -> app.ExecuteRun ->
// coordinator toolset build -> finishToolAssembly's flag-ON branch returns
// agent.ErrDiskProviderNotDurable (internal/agent/coordinator_tools.go,
// the `tools.HasDiskProvider(opts.DiskProvider)` check) before the first
// provider request.
func TestSDKBatchRuntimeOnCustomDiskProviderRefusedNotDurable(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		sseChunks(t, w, []map[string]any{textChunk("probe", "MUST_NOT_BE_HIT"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err, "a flag-ON app must install the batch runtime cleanly")
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-disk-refused"
	seedTenantSession(t, workDir, sessionID)

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "custom disk under the batch runtime",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			DiskProvider: newFakeSDKDisk(),
		},
	})
	require.Error(t, err, "the flag-ON runtime must refuse a custom DiskProvider")
	require.Nil(t, res, "the refusal is admission-time: no run result exists")
	require.ErrorIs(t, err, sdk.ErrDiskProviderNotDurable,
		"the refusal must surface as the stable sdk.ErrDiskProviderNotDurable sentinel, got %v", err)
	require.Zero(t, hits.Load(), "the refusal must happen before any provider traffic")
}

// TestSDKBatchRuntimeOnRunContextCancellation covers the flag-ON
// cancellation contract: cancelling the caller's context aborts the in-turn
// run with a context.Canceled-wrapped error, and the Client takes another
// run afterwards. Trace: the first assertion against real state is the
// provider request counter reaching 1 (the run really is in turn against
// the scripted provider) before the cancel fires.
func TestSDKBatchRuntimeOnRunContextCancellation(t *testing.T) {
	isolateGlobalConfig(t)
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	workDir := evalTempDir(t)
	const marker = "BATCH_RUNTIME_CANCEL_RECOVERY_OK"
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Hold the first turn open until the test cancels (or the request
		// context dies with it); after release the turn finishes cleanly.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-batch-cancel"
	const sessionID2 = "sdk-batch-cancel-recovery"
	seedTenantSession(t, workDir, sessionID)
	seedTenantSession(t, workDir, sessionID2)

	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, rerr := client.Run(runCtx, sdk.RunRequest{
			Prompt:            "cancelled mid-turn",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionID,
			HideSpinner:       true,
		})
		errCh <- rerr
	}()

	// First real-schema assertion: the provider actually saw the in-turn
	// request, so the cancel below races nothing.
	deadline := time.Now().Add(60 * time.Second)
	for hits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the flag-ON run never reached the provider")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled,
			"cancelling the caller's context must abort the flag-ON run, got %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled flag-ON run never returned")
	}

	// The client must survive the cancellation: a follow-up run on a fresh
	// session (the cancelled turn's session may still unwind) completes.
	close(release)
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "after cancellation",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID2,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, marker, res.FinalText)
}
