// W3 timeout normalization tests: the batch parser copy, the direct-call
// ingress carriage, and the explicit-leaf ambiguity rejection.
package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestBatchParseTimeoutParam is the table over the retained parser copy.
// Every case names the production mutation it catches.
func TestBatchParseTimeoutParam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tool    string
		input   string
		wantNil bool
		wantErr string
		check   func(t *testing.T, it *batchTimeoutIntent)
	}{
		{
			// Mutation: treating the absence of a timeout as an error or as
			// a zero-intent (breaking every no-timeout batch call).
			name: "no timeout key yields no intent", tool: "bash",
			input: `{"command":"x"}`, wantNil: true,
		},
		{
			// Mutation: dropping bare-number acceptance (#1159 contract).
			name: "bare number is wake_only", tool: "bash",
			input: `{"command":"x","timeout":30}`, wantNil: false,
			check: func(t *testing.T, it *batchTimeoutIntent) {
				require.Equal(t, batchTimeoutWakeOnly, it.Kind)
				require.Equal(t, 30, it.Seconds)
				require.Equal(t, 30*time.Second, it.Duration)
			},
		},
		{
			name: "wake_only object", tool: "run_command",
			input: `{"cmd":"x","timeout":{"seconds":10,"kind":"wake_only"}}`,
			check: func(t *testing.T, it *batchTimeoutIntent) {
				require.Equal(t, batchTimeoutWakeOnly, it.Kind)
				require.Equal(t, 10, it.Seconds)
			},
		},
		{
			name: "terminate_and_wake object", tool: "bash",
			input: `{"command":"x","timeout":{"seconds":10,"kind":"terminate_and_wake"}}`,
			check: func(t *testing.T, it *batchTimeoutIntent) {
				require.Equal(t, batchTimeoutTerminateAndWake, it.Kind)
				require.Equal(t, 10, it.Seconds)
			},
		},
		{
			// Mutation: dropping the legacy run_command alias.
			name: "timeout_seconds alias is terminate", tool: "run_command",
			input: `{"timeout_seconds":45}`,
			check: func(t *testing.T, it *batchTimeoutIntent) {
				require.Equal(t, batchTimeoutTerminateAndWake, it.Kind)
				require.Equal(t, 45, it.Seconds)
			},
		},
		{
			// Mutation: silently preferring one axis when both are set.
			name: "both timeout forms rejected", tool: "run_command",
			input:   `{"timeout_seconds":45,"timeout":30}`,
			wantErr: "provide at most one of timeout or the legacy timeout_seconds",
		},
		{
			// Mutation: floor drift below 5 seconds.
			name: "seconds below floor rejected", tool: "bash",
			input:   `{"timeout":{"seconds":4,"kind":"wake_only"}}`,
			wantErr: "must be between 5 and 604800",
		},
		{
			// Mutation: ceil drift past 7 days.
			name: "seconds above ceil rejected", tool: "bash",
			input:   `{"timeout":{"seconds":604801,"kind":"wake_only"}}`,
			wantErr: "must be between 5 and 604800",
		},
		{
			// Mutation: accepting an unknown kind string.
			name: "bogus kind rejected", tool: "bash",
			input:   `{"timeout":{"seconds":10,"kind":"bogus"}}`,
			wantErr: "timeout.kind must be",
		},
		{
			// Mutation: accepting a non-numeric, non-object shape.
			name: "string shape rejected", tool: "bash",
			input:   `{"timeout":"abc"}`,
			wantErr: "timeout must be an object",
		},
		{
			// Mutation: accepting a container shape.
			name: "array shape rejected", tool: "bash",
			input:   `{"timeout":[]}`,
			wantErr: "timeout must be an object",
		},
		{
			// Mutation: applying the timeout contract to tools that never
			// promised a timeout field (agentic_fetch owns its HTTP client
			// timeout; view has none).
			name: "agentic_fetch input timeout ignored", tool: "agentic_fetch",
			input: `{"url":"x","timeout":30}`, wantNil: true,
		},
		{
			name: "view input timeout ignored", tool: "view",
			input: `{"path":"x","timeout":30}`, wantNil: true,
		},
		{
			// Mutation: treating an explicit null timeout as an intent.
			name: "null timeout yields no intent", tool: "bash",
			input: `{"command":"x","timeout":null}`, wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			it, err := batchParseTimeoutParam(tc.tool, tc.input)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, it)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, it)
				return
			}
			require.NotNil(t, it)
			if tc.check != nil {
				tc.check(t, it)
			}
		})
	}
}

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
	require.Equal(t, batchTimeoutWakeOnly, p.Timeout.Kind)
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

// TestBatchModelLeafTimeoutAmbiguityRejected catches the mutation where an
// explicit batch item deadline and a nested tool-input timeout are both
// accepted (two unresolvable authorities), plus the positive twins pinning
// the single-authority contracts.
func TestBatchModelLeafTimeoutAmbiguityRejected(t *testing.T) {
	t.Parallel()
	reg := newPolicyToolBuild([]fantasy.AgentTool{newTestLeaf("bash")}).leaves
	inv := BatchInvocation{SessionID: "sess-1", CallID: "call-model", RegistryID: "reg-1"}

	// Ambiguous: item timeout AND nested input timeout.
	spec, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x","timeout":{"seconds":10,"kind":"terminate_and_wake"}}`), Timeout: "10s",
	}}}, inv, reg)
	require.ErrorContains(t, err, "ambiguous; provide at most one")
	require.Nil(t, spec.Task)

	// Positive twin: ONLY the nested input terminate intent feeds the
	// termination deadline.
	spec, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{
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
	spec, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x"}`), Timeout: "10s",
	}}}, inv, reg)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Len(t, spec.Batch.Items, 1)
	require.Equal(t, 10*time.Second, spec.Batch.Items[0].Task.Timeout)

	// Positive twin: ONLY a bare-number input timeout rides the payload.
	spec, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{
		Tool: "bash", Input: json.RawMessage(`{"command":"x","timeout":30}`),
	}}}, inv, reg)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Len(t, spec.Batch.Items, 1)
	require.Zero(t, spec.Batch.Items[0].Task.Timeout)
	p, err = decodeLeafPayload(spec.Batch.Items[0])
	require.NoError(t, err)
	require.NotNil(t, p.Timeout)
	require.Equal(t, batchTimeoutWakeOnly, p.Timeout.Kind)
	require.Equal(t, 30, p.Timeout.Seconds)
}
