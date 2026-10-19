package agent

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
	tool := &escrowTestTool{started: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	registry := newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves
	require.NoError(t, registryStore.Register("registry", registry))
	ingress := escrowTestIngress{store: registryStore}
	executor := &escrowAgentExecutor{deps: CoordinatorBatchAgentDeps(), ingress: ingress, launched: make(chan struct{}, 1)}
	store, conn, _ := newTestBatchStore(t)
	deps := installTestDeps(t, store, conn, "ok")
	deps.Executor = executor
	deps.Registries = registryStore
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
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
