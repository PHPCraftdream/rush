package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchLeafRegistryCredentialsAreStoreLocal(t *testing.T) {
	leaf := &installCredentialTestTool{}
	registry := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{leaf}).Leaves
	one, two := NewBatchLeafRegistryStore(), NewBatchLeafRegistryStore()
	require.NoError(t, one.Register("r", registry))
	require.NoError(t, two.Register("r", registry))
	first, err := one.Resolve("r")
	require.NoError(t, err)
	second, err := two.Resolve("r")
	require.NoError(t, err)
	firstLeaf, resolveErr := first.Resolve("agent")
	require.NoError(t, resolveErr)
	secondLeaf, resolveErr := second.Resolve("agent")
	require.NoError(t, resolveErr)
	firstCredential, ok := firstLeaf.(*batchCredentialLeaf)
	require.True(t, ok)
	secondCredential, ok := secondLeaf.(*batchCredentialLeaf)
	require.True(t, ok)
	require.NotSame(t, firstCredential.escrow, secondCredential.escrow)
	registryLeaf, resolveErr := registry.Resolve("agent")
	require.NoError(t, resolveErr)
	require.Same(t, leaf, registryLeaf)
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
	require.NoError(t, escrow.Register(oldKey.adapterKey(), "root", escrowTestCreds("old")))
	_, err := escrow.RegisterBatch([]batchadapter.CredentialRegistration{
		{Key: newKey.adapterKey(), Root: "root", Credentials: escrowTestCreds("new")},
		{Key: oldKey.adapterKey(), Root: "root", Credentials: escrowTestCreds("replacement")},
	})
	require.Error(t, err)
	creds, complete, err := escrow.consume(oldKey, "root.1")
	require.NoError(t, err)
	require.Equal(t, "old", creds.Credentials[0].APIKey)
	complete()
	_, _, err = escrow.consume(newKey, "root.1")
	require.ErrorIs(t, err, batchadapter.ErrCredentialEscrowMissing)
}

func TestBatchCredentialAdapterRegistrationIgnoresNonAgentPayload(t *testing.T) {
	adapter := &batchRunnerAdapter{}
	registrations, err := adapter.credentialRegistrations(context.Background(), "root", jobbatch.Spec{
		Task: &jobbatch.TaskSpec{Kind: batchdispatch.KindGeneric, Payload: []byte("not-json")},
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
