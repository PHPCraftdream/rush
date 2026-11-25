// W3 timeout normalization tests: the batch parser copy, the direct-call
// ingress carriage, and the explicit-leaf ambiguity rejection.
package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/stretchr/testify/require"
)

// TestBatchIngressDirectTerminateTimeoutLatchesSpecTimeout catches the
// mutation where a direct call's terminate_and_wake intent never reaches
// the termination deadline (today's gap): TaskSpec.Timeout must carry it.
func TestBatchIngressDirectTerminateTimeoutLatchesSpecTimeout(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	t.Cleanup(runner.settleBatches)
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("bash"),
		fantasy.ToolCall{ID: "call-term", Name: "bash", Input: `{"command":"x","timeout":{"seconds":30,"kind":"terminate_and_wake"}}`})
	require.NoError(t, err)
	specs := runner.submitSpecs()
	require.Len(t, specs, 1)
	require.Equal(t, 30*time.Second, specs[0].Task.Timeout)
	p, err := decodeLeafPayload(specs[0])
	require.NoError(t, err)
	require.Nil(t, p.Timeout, "a terminate intent rides the spec deadline, never the payload")
}

// TestBatchIngressDirectWakeOnlyTimeoutCarriedInPayload catches the
// mutation where a wake_only intent is silently dropped on the direct path,
// AND the hash-determinism mutation this package exists to guard: a
// now()-derived field in the payload (e.g. an absolute due time) would make
// batchInvocationHash differ across retries of one logical call, breaking
// in-flight singleflight matching and live-root attach.
func TestBatchIngressDirectWakeOnlyTimeoutCarriedInPayload(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	t.Cleanup(runner.settleBatches)
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("bash"),
		fantasy.ToolCall{ID: "call-wake", Name: "bash", Input: `{"command":"x","timeout":30}`})
	require.NoError(t, err)
	specs := runner.submitSpecs()
	require.Len(t, specs, 1)
	require.Zero(t, specs[0].Task.Timeout, "wake_only never sets the termination deadline")
	p, err := decodeLeafPayload(specs[0])
	require.NoError(t, err)
	require.NotNil(t, p.Timeout)
	require.Equal(t, batchinst.TimeoutWakeOnly, p.Timeout.Kind)
	require.Equal(t, 30, p.Timeout.Seconds)

	// Hash determinism: the same logical call built again (fresh time, same
	// input) must produce a byte-identical spec hash. With a now()-derived
	// DueUnix in the payload this assertion FAILS — that is the mutation.
	again, err := batchOneItemSpec(testInvocation(),
		fantasy.ToolCall{ID: "call-wake", Name: "bash", Input: `{"command":"x","timeout":30}`},
		json.RawMessage(`{"command":"x","timeout":30}`))
	require.NoError(t, err)
	require.Equal(t, batchInvocationHash(specs[0]), batchInvocationHash(again),
		"the spec hash must stay a pure function of the call input: no wall-clock fields in the payload")
}

// TestBatchIngressInvalidTimeoutFailsClosed catches the mutation where an
// invalid timeout is accepted at ingress instead of failing the call. The
// failure is the model's input, so it comes back as a tool error result: a
// Go error here is fatal and ended the whole run.
func TestBatchIngressInvalidTimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	t.Cleanup(runner.settleBatches)
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	resp, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("bash"),
		fantasy.ToolCall{ID: "call-bad", Name: "bash", Input: `{"command":"x","timeout":{"seconds":1,"kind":"wake_only"}}`})
	require.NoError(t, err, "a bad tool input must not end the turn")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "must be between 5 and 604800")

	resp, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("run_command"),
		fantasy.ToolCall{ID: "call-both", Name: "run_command", Input: `{"command":"x","timeout":30,"timeout_seconds":30}`})
	require.NoError(t, err, "a bad tool input must not end the turn")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not both")
	require.Zero(t, runner.submitCalls(), "an invalid timeout never submits")
}

func TestBatchIngressInvalidCanonicalInputFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
	}{
		{name: "malformed JSON", input: `{"command":`},
		{name: "empty string", input: ""},
		{name: "whitespace", input: "  \t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := newFakeRunner(nil)
			t.Cleanup(runner.settleBatches)
			cfg := testRuntimeConfig()
			cfg.Runner = runner
			cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{Content: "done"})
			rt, err := newBatchRuntime(cfg)
			require.NoError(t, err)

			resp, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("bash"),
				fantasy.ToolCall{ID: "call-invalid-input", Name: "bash", Input: tc.input})
			require.NoError(t, err, "invalid tool input must not end the turn")
			require.True(t, resp.IsError)
			require.Zero(t, runner.submitCalls(), "invalid input never submits")
		})
	}
}

// TestBatchModelLeafTimeoutAmbiguityRejected catches the mutation where an
// explicit batch item deadline and a nested tool-input timeout are both
// accepted (two unresolvable authorities), plus the positive twins pinning
// the single-authority contracts.
func TestBatchModelLeafTimeoutAmbiguityRejected(t *testing.T) {
	t.Parallel()
	reg := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{newTestLeaf("bash")}).Leaves
	inv := BatchInvocation{SessionID: "sess-1", CallID: "call-model", RegistryID: "reg-1"}

	// Ambiguous: item timeout AND nested input timeout.
	spec, err := modelBatchSpec(modelcontrol.Spec{Mode: "parallel", Items: []modelcontrol.Item{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x","timeout":{"seconds":10,"kind":"terminate_and_wake"}}`), Timeout: "10s",
	}}}, inv, reg)
	require.ErrorContains(t, err, "ambiguous; provide at most one")
	require.Nil(t, spec.Task)

	// Positive twin: ONLY the nested input terminate intent feeds the
	// termination deadline.
	spec, err = modelBatchSpec(modelcontrol.Spec{Mode: "parallel", Items: []modelcontrol.Item{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x","timeout":{"seconds":10,"kind":"terminate_and_wake"}}`),
	}}}, inv, reg)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Len(t, spec.Batch.Items, 1)
	require.Equal(t, 10*time.Second, spec.Batch.Items[0].Task.Timeout)
	p, err := decodeLeafPayload(spec.Batch.Items[0])
	require.NoError(t, err)
	require.Nil(t, p.Timeout)

	// Positive twin: ONLY the explicit string form keeps today's contract.
	spec, err = modelBatchSpec(modelcontrol.Spec{Mode: "parallel", Items: []modelcontrol.Item{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x"}`), Timeout: "10s",
	}}}, inv, reg)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Len(t, spec.Batch.Items, 1)
	require.Equal(t, 10*time.Second, spec.Batch.Items[0].Task.Timeout)

	// Positive twin: ONLY a bare-number input timeout rides the payload.
	spec, err = modelBatchSpec(modelcontrol.Spec{Mode: "parallel", Items: []modelcontrol.Item{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x","timeout":30}`),
	}}}, inv, reg)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Len(t, spec.Batch.Items, 1)
	require.Zero(t, spec.Batch.Items[0].Task.Timeout)
	p, err = decodeLeafPayload(spec.Batch.Items[0])
	require.NoError(t, err)
	require.NotNil(t, p.Timeout)
	require.Equal(t, batchinst.TimeoutWakeOnly, p.Timeout.Kind)
	require.Equal(t, 30, p.Timeout.Seconds)
}
