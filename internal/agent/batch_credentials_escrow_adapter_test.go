package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchLeafRegistryCredentialsAreStoreLocal(t *testing.T) {
	leaf := &installCredentialTestTool{}
	registry := newPolicyToolBuild([]fantasy.AgentTool{leaf}).leaves
	one, two := NewBatchLeafRegistryStore(), NewBatchLeafRegistryStore()
	require.NoError(t, one.Register("r", registry))
	require.NoError(t, two.Register("r", registry))
	first, err := one.Resolve("r")
	require.NoError(t, err)
	second, err := two.Resolve("r")
	require.NoError(t, err)
	firstLeaf, ok := first.entries["agent"].tool.(*batchCredentialLeaf)
	require.True(t, ok)
	secondLeaf, ok := second.entries["agent"].tool.(*batchCredentialLeaf)
	require.True(t, ok)
	require.NotSame(t, firstLeaf.escrow, secondLeaf.escrow)
	require.Same(t, leaf, registry.entries["agent"].tool)
}

type installCredentialTestTool struct{ fantasy.AgentTool }

func (installCredentialTestTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: "agent"} }
func (installCredentialTestTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, nil
}

func TestBatchCredentialRegistrationIsTransactional(t *testing.T) {
	escrow := newBatchCredentialEscrow()
	oldKey := batchCredentialKey{registryID: "r", sessionID: "s", callID: "old", tool: "agent"}
	newKey := batchCredentialKey{registryID: "r", sessionID: "s", callID: "new", tool: "agent"}
	require.NoError(t, escrow.register(oldKey, "root", escrowTestCreds("old")))
	_, err := escrow.registerBatch([]batchCredentialRegistration{
		{key: newKey, root: "root", creds: escrowTestCreds("new")},
		{key: oldKey, root: "root", creds: escrowTestCreds("replacement")},
	})
	require.Error(t, err)
	creds, complete, err := escrow.consume(oldKey, "root.1")
	require.NoError(t, err)
	require.Equal(t, "old", creds.Credentials[0].APIKey)
	complete()
	_, _, err = escrow.consume(newKey, "root.1")
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
}

func TestBatchCredentialAdapterRegistrationIgnoresNonAgentPayload(t *testing.T) {
	adapter := &batchRunnerAdapter{}
	registrations, err := adapter.credentialRegistrations(context.Background(), "root", jobbatch.Spec{
		Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: []byte("not-json")},
	})
	require.NoError(t, err)
	require.Empty(t, registrations)
}

func TestBatchCredentialLeafWrapperForwardsToolSurface(t *testing.T) {
	leaf := &escrowTestTool{started: make(chan context.Context, 1), release: make(chan struct{})}
	escrow := newBatchCredentialEscrow()
	wrapped := &batchCredentialLeaf{AgentTool: leaf, escrow: escrow}
	require.Equal(t, leaf.Info(), wrapped.Info())
	require.Same(t, escrow, wrapped.credentialEscrow())
}
