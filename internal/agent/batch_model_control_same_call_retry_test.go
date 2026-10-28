package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestModelBatchRunSameCallIDRetry(t *testing.T) {
	env := newH2aGatedInstall(t)
	env.fake.arm()
	control := NewInstalledBatchModelControl(
		env.install.RunnerHandle, env.install.Delivery, env.store, nil,
		batchTestSessions(env.sessionID), env.install.Runtime.cfg.Host,
		env.install.Runtime.cfg.RunnerID, h2aTestProfile(t), nil,
		env.install.RootLeases,
	)
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	template := BatchInvocation{
		SessionID: env.sessionID, MessageID: "retry-message", RegistryID: env.registryID,
		Origin: "coordinator", Delivery: batchDeliveryChoice{Mode: batchDeliveryDetached},
		Policy: batchLeafPolicyContext{RegistryID: env.registryID, RestrictedToolset: env.fingerprint, DiskProvider: true, DiskBindingID: env.bindingID},
	}
	installedTools, err := control.InstallBatchModelControls(reg, template)
	require.NoError(t, err)
	var runTool fantasy.AgentTool
	for _, tool := range installedTools {
		if tool.Info().Name == "batch_run" {
			runTool = tool
		}
	}
	require.NotNil(t, runTool)
	input, err := json.Marshal(batchModelParams{Spec: batchModelSpec{
		Mode: "parallel", Items: []batchModelItem{{Tool: "fs_write", Input: json.RawMessage(h2aLeafInput(env.targetPath))}},
	}})
	require.NoError(t, err)
	call := fantasy.ToolCall{ID: "reused-call-id", Name: "batch_run", Input: string(input)}
	ctx := batchCallCtx(env.sessionID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "retry-message")
	first, err := runTool.Run(ctx, call)
	require.NoError(t, err)
	require.False(t, first.IsError, first.Content)
	var firstRoot string
	require.NoError(t, json.Unmarshal([]byte(first.Content), &firstRoot))
	require.NotEmpty(t, firstRoot)

	second, err := runTool.Run(ctx, call)
	require.NoError(t, err)
	require.False(t, second.IsError, second.Content)
	var secondRoot string
	require.NoError(t, json.Unmarshal([]byte(second.Content), &secondRoot))
	require.Equal(t, firstRoot, secondRoot, "same-call-id retry should attach to its still-live root")

	env.fake.letGo()
	require.Eventually(t, func() bool { return env.leaf.runCount() >= 1 }, 5*time.Second, time.Millisecond)
}
