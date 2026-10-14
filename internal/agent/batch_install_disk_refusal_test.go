package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

type diskRefusalProvider struct {
	tools.DiskProvider
	values []string
}

type diskRefusalAttach struct {
	batchAttachStore
	probes int
}

func (a *diskRefusalAttach) ProbeLive(ctx context.Context, p batchAttachProbe) (jobbatch.ID, bool, error) {
	a.probes++
	return a.batchAttachStore.ProbeLive(ctx, p)
}

type diskRefusalRunner struct {
	batchRunnerHandle
	submits int
}

func (r *diskRefusalRunner) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	r.submits++
	return r.batchRunnerHandle.Submit(ctx, req)
}

func diskRefusalInvocation(env *batchInventoryEnv, provider tools.DiskProvider, custom string) BatchInvocation {
	return BatchInvocation{
		SessionID: env.sessionID, MessageID: "msg-disk", RegistryID: env.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Policy: batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.registryID,
			CustomDisk: custom, DiskProvider: tools.HasDiskProvider(provider),
		},
	}
}

func TestDiskProviderRefusalRuntimeBeforeAdmission(t *testing.T) {
	cases := []struct {
		name     string
		provider tools.DiskProvider
		custom   string
	}{
		{name: "non-comparable provider", provider: diskRefusalProvider{values: []string{"provider"}}},
		{name: "explicit OSDisk", provider: tools.OSDisk()},
		{name: "trimmed custom disk", custom: " disk "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newBatchInventoryEnv(t, "view")
			attach := &diskRefusalAttach{batchAttachStore: env.install.Runtime.cfg.Attach}
			runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
			env.install.Runtime.cfg.Attach = attach
			env.install.Runtime.cfg.Runner = runner
			ctx := env.ctx
			if tc.provider != nil {
				ctx = WithCallOptions(ctx, &CallOptions{DiskProvider: tc.provider})
			}
			inv := diskRefusalInvocation(env, tc.provider, tc.custom)
			_, err := env.install.Runtime.Invoke(ctx, inv, env.leaf,
				fantasy.ToolCall{ID: "disk-refusal", Name: "view", Input: `{}`})
			require.ErrorIs(t, err, ErrDiskProviderNotDurable,
				"disk-backed work must be refused before attach, Submit, or leaf execution")
			require.Zero(t, attach.probes, "disk refusal must precede live-attach lookup")
			require.Zero(t, runner.submits, "disk refusal must precede Submit")
			assertDiskRefusalLeavesNoEffects(t, env)
		})
	}
}

func TestDiskProviderRefusalModelFacingIngressRoutes(t *testing.T) {
	for _, route := range []string{"coordinator", "cli_bridge", "agentic_fetch"} {
		t.Run(route, func(t *testing.T) {
			env := newBatchInventoryEnv(t, "view")
			inventoryBindProcess(t, env)
			attach := &diskRefusalAttach{batchAttachStore: env.install.Runtime.cfg.Attach}
			runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
			env.install.Runtime.cfg.Attach = attach
			env.install.Runtime.cfg.Runner = runner
			ctx := WithCallOptions(batchCallCtx(env.sessionID), &CallOptions{DiskProvider: tools.OSDisk()})
			ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-disk")
			var resp fantasy.ToolResponse
			var err error
			switch route {
			case "coordinator":
				built, buildErr := newBatchWorkToolBuild(processCoordinatorBatchBinding(), []fantasy.AgentTool{env.leaf})
				require.NoError(t, buildErr)
				resp, err = built.tools[0].Run(ctx, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{}`})
			case "cli_bridge":
				stamped := withBatchBridgeInvocation(ctx, batchBridgeInvocation{RegistryID: env.registryID, MessageID: "msg-disk"})
				bridge := coordinatorBridgeFallback(stamped)
				require.NotNil(t, bridge)
				resp, err = bridge.Call(stamped, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{"path":"x"}`})
			case "agentic_fetch":
				built := armPrivateFetchIngress(newPolicyToolBuild([]fantasy.AgentTool{env.leaf}), processCoordinatorBatchBinding().ingress)
				resp, err = built.tools[0].Run(ctx, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{}`})
			}
			require.NoError(t, err)
			require.True(t, resp.IsError)
			require.Contains(t, resp.Content, ErrDiskProviderNotDurable.Error(),
				"model-facing ingress must expose the disk refusal as a tool error")
			require.Zero(t, attach.probes, "model-facing disk refusal must precede live-attach lookup")
			require.Zero(t, runner.submits, "model-facing disk refusal must precede Submit")
			assertDiskRefusalLeavesNoEffects(t, env)
		})
	}
}

func TestDiskProviderNilRemainsAdmitted(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	resp, err := env.invoke("nil-disk", "view")
	env.assertDelivered("nil-disk", "view", batchKindGeneric, resp, err)
}

func TestBatchExecutorRejectsDiskMarkedPayload(t *testing.T) {
	cases := []struct {
		name   string
		policy batchLeafPolicyContext
	}{
		{name: "provider marker", policy: batchLeafPolicyContext{DiskProvider: true}},
		{name: "custom disk", policy: batchLeafPolicyContext{CustomDisk: " disk "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(batchLeafPayload{Tool: "view", SessionID: "session", Policy: &tc.policy})
			require.NoError(t, err)
			executor := newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{batchKindGeneric: newBatchGenericFamily()})
			_, err = executor.Start(context.Background(), jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}, func(jobbatch.Result) {})
			require.ErrorIs(t, err, ErrDiskProviderNotDurable, "executor admission must refuse disk-backed payload before leaf start")
		})
	}
}

func assertDiskRefusalLeavesNoEffects(t *testing.T, env *batchInventoryEnv) {
	t.Helper()
	var rows int
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM batch_nodes`).Scan(&rows))
	require.Zero(t, rows, "refused disk-backed call must create no batch rows")
	var asyncRows int
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM async_jobs WHERE owner_session_id = ?1`, env.sessionID).Scan(&asyncRows))
	require.Zero(t, asyncRows, "refused call must not reach the legacy async ledger")
	require.Zero(t, env.counters.hooks.Load(), "refused disk-backed call must not run a leaf")
	require.Zero(t, env.counters.perms.Load(), "refused disk-backed call must not reach permissions")
}
