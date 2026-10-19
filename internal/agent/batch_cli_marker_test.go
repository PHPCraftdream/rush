package agent

// CLI marker lifecycle tests (task #1462 round 4): SubmitCLI/AddCLI over the
// REAL installed runtime (InstallBatchRuntime + real engine + real
// batchRunnerAdapter stamping/escrow + real agent driver).
//
// Resource lifetime: every fixture registers its cleanups so LIFO teardown
// order is (1) unblock/release the parked tool, (2) runner.Close + join of
// every executor goroutine and agent driver turn, (3) — registered inside
// newTestBatchStore BEFORE the above — the DB release. No goroutine can
// outlive the DB release on any assertion-failure path.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// cliMarkerAgentSpec builds a batch root carrying one marked-capable agent
// leaf owned by "owner".
func cliMarkerAgentSpec(t *testing.T, callID string) jobbatch.Spec {
	t.Helper()
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: callID, SessionID: "owner", RegistryID: "registry",
		Input: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	return jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll,
		Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}}},
	}}
}

func newCLIMarkerInstall(t *testing.T) (*BatchInstall, *escrowAgentExecutor, *escrowTestTool) {
	t.Helper()
	registryStore := NewBatchLeafRegistryStore()
	tool := &escrowTestTool{started: make(chan context.Context, 4), release: make(chan struct{})}
	registry := newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves
	require.NoError(t, registryStore.Register("registry", registry))
	ingress := escrowTestIngress{store: registryStore}
	executor := &escrowAgentExecutor{deps: CoordinatorBatchAgentDeps(), ingress: ingress, launched: make(chan struct{}, 4)}
	store, conn, _ := newTestBatchStore(t)
	deps := installTestDeps(t, store, conn, "ok")
	deps.Executor = executor
	deps.Registries = registryStore
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	// Registered AFTER newTestBatchStore, so LIFO runs this BEFORE the DB
	// release: close the runner, join every agent driver turn's Exit, then
	// wait for the executor's report goroutines.
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = install.Runner.Close(closeCtx)
		_, _, turns := executor.snapshot()
		for _, turn := range turns {
			select {
			case <-turn.Exit():
			case <-closeCtx.Done():
				t.Errorf("agent driver failed to exit during cleanup")
			}
		}
		executor.wg.Wait()
	})
	// Registered LAST so it runs FIRST: unblock the parked tool before the
	// runner is closed, on every path including assertion failure.
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	return install, executor, tool
}

func TestBatchCLISubmitMarkerWithoutCredentials(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, executor, tool := newCLIMarkerInstall(t)
	batch, err := install.SubmitCLI(context.Background(), "owner", cliMarkerAgentSpec(t, "cli-none-call"))
	require.NoError(t, err)
	started := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	require.Nil(t, callCredentialsFrom(started))
	close(tool.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = batch.Wait(closeCtx)
	require.NoError(t, err)
	requests, payloads, _ := executor.snapshot()
	require.Len(t, requests, 1)
	require.False(t, requests[0].CallCredentials)
	require.Len(t, payloads, 1)
	require.NotContains(t, string(payloads[0]), "call_credentials")
}

func TestBatchCLISubmitMarkerWithCredentials(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, executor, tool := newCLIMarkerInstall(t)
	ctx := withCallCredentials(context.Background(), escrowTestCreds("cli-tenant-secret"))
	batch, err := install.SubmitCLI(ctx, "owner", cliMarkerAgentSpec(t, "cli-cred-call"))
	require.NoError(t, err)
	started := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	creds := callCredentialsFrom(started)
	require.NotNil(t, creds)
	require.Equal(t, "cli-tenant-secret", creds.Credentials[0].APIKey)
	close(tool.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = batch.Wait(closeCtx)
	require.NoError(t, err)
	requests, payloads, _ := executor.snapshot()
	require.Len(t, requests, 1)
	require.True(t, requests[0].CallCredentials)
	require.Len(t, payloads, 1)
	require.Contains(t, string(payloads[0]), `"call_credentials":true`)
	require.NotContains(t, string(payloads[0]), "cli-tenant-secret")
}

// TestBatchCLIAddMarkerWithoutCredentials is the AddCLI twin of the submit
// marker-none case: a host context (no call credentials) adds a leaf to a
// live CLI-owned root; the leaf starts unmarked and observes no credentials.
//
// First behavioral assertion: the second leaf's tool context carries nil
// credentials. Mutation caught: an Add path that stamped the marker
// unconditionally (or leaked caller credentials into a credential-less Add)
// would surface a marked payload here.
func TestBatchCLIAddMarkerWithoutCredentials(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, executor, tool := newCLIMarkerInstall(t)
	batch, err := install.SubmitCLI(context.Background(), "owner", cliMarkerAgentSpec(t, "cli-add-none-root"))
	require.NoError(t, err)
	root := batch.ID()
	started := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	require.Nil(t, callCredentialsFrom(started))
	require.NoError(t, install.AddCLI(context.Background(), root, root,
		[]jobbatch.Spec{cliMarkerAgentSpec(t, "cli-add-none-item")}))
	second := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	require.Nil(t, callCredentialsFrom(second))
	// The root stays CLI-owned after the Add.
	require.Equal(t, "owner", install.CLIOwner(root))
	require.True(t, install.OwnsCLI(root))
	close(tool.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = batch.Wait(closeCtx)
	require.NoError(t, err)
	requests, payloads, _ := executor.snapshot()
	require.Len(t, requests, 2)
	require.Len(t, payloads, 2)
	require.False(t, requests[1].CallCredentials)
	require.NotContains(t, string(payloads[1]), "call_credentials")
}

// TestBatchCLIAddMarkerWithCredentials pins the AddCLI credential path: the
// root is submitted WITHOUT credentials; the Add carries call credentials,
// so the added leaf is marked, its escrow entry is registered in the
// install's registry-store escrow, and the leaf driver observes the cloned
// secret.
//
// Mutation caught: an AddCLI path that skipped stampBatchCallCredentials or
// the escrow registration would fail the driver (missing escrow entry) or
// leave the leaf unmarked; a path that serialized the secret would fail the
// payload NotContains.
func TestBatchCLIAddMarkerWithCredentials(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, executor, tool := newCLIMarkerInstall(t)
	batch, err := install.SubmitCLI(context.Background(), "owner", cliMarkerAgentSpec(t, "cli-add-cred-root"))
	require.NoError(t, err)
	root := batch.ID()
	started := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	require.Nil(t, callCredentialsFrom(started))
	addCtx := withCallCredentials(context.Background(), escrowTestCreds("cli-add-secret"))
	require.NoError(t, install.AddCLI(addCtx, root, root,
		[]jobbatch.Spec{cliMarkerAgentSpec(t, "cli-add-cred-item")}))
	second := receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	creds := callCredentialsFrom(second)
	require.NotNil(t, creds)
	require.Equal(t, "cli-add-secret", creds.Credentials[0].APIKey, "the leaf observes the CLONED secret from the escrow")
	require.Equal(t, "owner", install.CLIOwner(root))
	require.True(t, install.OwnsCLI(root))
	close(tool.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = batch.Wait(closeCtx)
	require.NoError(t, err)
	_, payloads, _ := executor.snapshot()
	require.Len(t, payloads, 2)
	require.Contains(t, string(payloads[1]), `"call_credentials":true`)
	require.NotContains(t, string(payloads[1]), "cli-add-secret")
}

// TestBatchCLIAddUnknownRootFailsCleanly: AddCLI over a missing/unknown root
// reports a clean ownership error — no panic, no engine call, no goroutines.
//
// Mutation caught: an AddCLI that dereferenced the missing owner (or treated
// the empty owner as "owned by anyone") would panic or accept the add.
func TestBatchCLIAddUnknownRootFailsCleanly(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, _, _ := newCLIMarkerInstall(t)
	missing := jobbatch.ID("cli-add-missing-root")
	err := install.AddCLI(context.Background(), missing, missing,
		[]jobbatch.Spec{cliMarkerAgentSpec(t, "cli-add-missing-item")})
	require.ErrorContains(t, err, "root is not owned")
	require.False(t, install.OwnsCLI(missing))
}

// TestBatchCLIAddUnownedRootFailsCleanly: a root submitted directly through
// the runner handle (bypassing SubmitCLI, so NO CLI owner is recorded) is
// refused by AddCLI — the ownership map, not the engine, is the authority.
//
// Mutation caught: an AddCLI that resolved ownership from the durable store
// or the engine instead of the install's own owners map would accept adds
// for roots this install never claimed.
func TestBatchCLIAddUnownedRootFailsCleanly(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	install, executor, tool := newCLIMarkerInstall(t)
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "cli-add-foreign-call", SessionID: "other", RegistryID: "registry",
		Input: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	root := jobbatch.ID("cli-add-unowned-root")
	_, err = install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{
		Root: root,
		Spec: jobbatch.Spec{Batch: &jobbatch.BatchSpec{
			Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll,
			Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}}},
		}},
		Ref: "cli-add-unowned",
	})
	require.NoError(t, err)
	receiveEscrowStarted(t, context.Background(), tool.started)
	receiveEscrowLaunched(t, context.Background(), executor.launched)
	require.False(t, install.OwnsCLI(root))
	err = install.AddCLI(context.Background(), root, root,
		[]jobbatch.Spec{cliMarkerAgentSpec(t, "cli-add-unowned-item")})
	require.ErrorContains(t, err, "root is not owned")
	close(tool.release)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = install.RunnerHandle.batchFor(root).Wait(closeCtx)
	require.NoError(t, err)
}
