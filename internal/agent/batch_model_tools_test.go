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

type modelControlFake struct {
	ran         batchModelSpec
	inv         BatchInvocation
	addedNotify string
}

func (f *modelControlFake) Run(_ context.Context, i BatchInvocation, _ *policyLeafRegistry, s batchModelSpec) (jobbatch.ID, error) {
	f.ran = s
	f.inv = i
	return "root", nil
}

func (f *modelControlFake) Add(_ context.Context, _, _, _ string, _ BatchInvocation, _ *policyLeafRegistry, _ []batchModelItem) ([]jobbatch.ID, error) {
	f.addedNotify = "each"
	return []jobbatch.ID{"node"}, nil
}
func (f *modelControlFake) Status(context.Context, string, string) (any, error) { return nil, nil }
func (f *modelControlFake) StatusNode(context.Context, string, string, string) (any, error) {
	return nil, nil
}

func (f *modelControlFake) Output(context.Context, string, string, string, uint64) (any, error) {
	return nil, nil
}

func (f *modelControlFake) Stop(context.Context, string, string, string) (any, error) {
	return nil, nil
}

func modelRegistry(names ...string) *policyLeafRegistry {
	r := &policyLeafRegistry{entries: map[string]policyLeafEntry{}}
	for _, n := range names {
		r.entries[n] = policyLeafEntry{tool: batchTestTools(n)[0]}
	}
	return r
}

func TestModelBatchToolsRecursiveDefaultAndPinnedPolicy(t *testing.T) {
	inv := BatchInvocation{SessionID: "owner", RegistryID: "reg", Policy: batchLeafPolicyContext{RegistryID: "reg", RestrictedToolset: "only-view"}}
	spec, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{"path":"x"}`)}, {Batch: &batchModelSpec{Mode: "sequential", Notify: "each", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{"path":"y"}`)}}}}}}, inv, modelRegistry("view"))
	require.NoError(t, err, "recursive explicit batch schema must map to engine specs")
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify, "explicit omitted notify defaults to all")
	var p batchLeafPayload
	require.NoError(t, json.Unmarshal(spec.Batch.Items[0].Task.Payload, &p))
	require.Equal(t, "reg", p.RegistryID, "leaf must retain caller registry")
	require.Equal(t, jobbatch.NotifyEach, spec.Batch.Items[1].Batch.Notify, "explicit each must survive")
}

func TestModelBatchToolsRejectNestedControlAndUnavailableLeaf(t *testing.T) {
	inv := BatchInvocation{SessionID: "owner", RegistryID: "reg", Policy: batchLeafPolicyContext{RegistryID: "reg", RestrictedToolset: "r"}}
	_, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Batch: &batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "batch_stop", Input: json.RawMessage(`{}`)}}}}}}, inv, modelRegistry("view"))
	require.ErrorContains(t, err, "cannot be a batch leaf", "nested controls must fail closed")
	_, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "edit", Input: json.RawMessage(`{}`)}}}, inv, modelRegistry("view"))
	require.ErrorContains(t, err, "unavailable", "toolset cannot be widened")
}

func TestBatchModelToolCallsPreserveInvocationAndAddEach(t *testing.T) {
	f := &modelControlFake{}
	inv := BatchInvocation{SessionID: "s", RegistryID: "r"}
	ts := newBatchModelTools(f, inv, modelRegistry("view"))
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "s")
	raw, _ := json.Marshal(batchModelParams{Spec: batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{}`)}}}})
	resp, err := ts[0].Run(ctx, fantasy.ToolCall{Name: "batch_run", Input: string(raw)})
	require.NoError(t, err, "valid batch_run must reach control interface")
	require.Contains(t, resp.Content, "root")
	require.Equal(t, "r", f.inv.RegistryID, "batch_run retains caller registry")
	add, _ := json.Marshal(batchModelParams{Root: "root", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{}`)}}})
	_, err = ts[1].Run(ctx, fantasy.ToolCall{Name: "batch_add", Input: string(add)})
	require.NoError(t, err)
	require.Equal(t, "each", f.addedNotify, "later additions use NotifyEach")
}

func TestBatchModelToolInventory(t *testing.T) {
	ts := newBatchModelTools(&modelControlFake{}, BatchInvocation{SessionID: "s", RegistryID: "r"}, modelRegistry())
	names := map[string]bool{}
	for _, tool := range ts {
		names[tool.Info().Name] = true
	}
	for _, name := range []string{"batch_run", "batch_add", "batch_status", "batch_output", "batch_stop"} {
		require.True(t, names[name], "flagged batch tool missing: "+name)
	}
}
