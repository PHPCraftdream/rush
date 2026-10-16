package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

type batchTestFamily struct{ got batchLeafPayload }

func (f *batchTestFamily) start(_ context.Context, _ batchLeafEnv, _ jobbatch.Launch, p batchLeafPayload, _ func(jobbatch.Result)) (jobbatch.Execution, error) {
	f.got = p
	return nil, nil
}

func batchTestLaunch(kind string, payload string) jobbatch.Launch {
	return jobbatch.Launch{ID: "r.1", Token: 1, Task: jobbatch.TaskSpec{Kind: kind, Payload: json.RawMessage(payload)}}
}

func TestBatchExecutorRoutesByKindAndRejectsBadStarts(t *testing.T) {
	// Mutation: routing a launch to the wrong family, or starting on a payload without tool/session.
	fam := &batchTestFamily{}
	e := newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{batchKindGeneric: fam})

	_, err := e.Start(context.Background(), batchTestLaunch(batchKindGeneric, `{"tool":"view","tool_call_id":"c1","session_id":"s1","input":{"a":1}}`), func(jobbatch.Result) {})
	require.NoError(t, err)
	require.Equal(t, "view", fam.got.Tool)
	require.Equal(t, "s1", fam.got.SessionID)
	require.JSONEq(t, `{"a":1}`, string(fam.got.Input))

	for name, l := range map[string]jobbatch.Launch{
		"unknown kind": batchTestLaunch("nope", `{"tool":"view","session_id":"s1"}`),
		"bad json":     batchTestLaunch(batchKindGeneric, `{`),
		"no tool":      batchTestLaunch(batchKindGeneric, `{"session_id":"s1"}`),
		"no session":   batchTestLaunch(batchKindGeneric, `{"tool":"view"}`),
	} {
		_, err := e.Start(context.Background(), l, func(jobbatch.Result) {})
		var coded interface{ Code() string }
		require.ErrorAs(t, err, &coded, name)
		require.True(t, jobbatch.ValidCode(coded.Code()) && !jobbatch.ReservedCode(coded.Code()), name)
	}
}

func TestBatchLeafResolutionFailsClosed(t *testing.T) {
	// Mutation: resolving a tool that is not in the pinned set, or swallowing the provider error.
	env := batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) {
		return newPolicyToolBuild(nil).leaves, nil
	}}
	_, err := env.resolveLeaf(batchLeafPayload{Tool: "bash", SessionID: "s1"})
	var coded interface{ Code() string }
	require.ErrorAs(t, err, &coded)
	require.Equal(t, batchCodeNoLeaf, coded.Code())

	failing := batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { return nil, errors.New("no session") }}
	_, err = failing.resolveLeaf(batchLeafPayload{Tool: "bash", SessionID: "s1"})
	require.ErrorAs(t, err, &coded)
	require.Equal(t, batchCodeNoLeaf, coded.Code())

	_, err = batchLeafEnv{}.resolveLeaf(batchLeafPayload{Tool: "bash", SessionID: "s1"})
	require.ErrorAs(t, err, &coded)
}

func TestBatchResultNeverTurnsFailureIntoSuccess(t *testing.T) {
	// Mutation: mapping an error response or a Run error to Completed, or dropping the envelope.
	ok := batchResultFromResponse(fantasy.NewTextResponse("done"), nil, false)
	require.Equal(t, jobbatch.StateCompleted, ok.State)
	require.Empty(t, ok.Code)

	denied := batchResultFromResponse(fantasy.NewTextErrorResponse("denied"), nil, false)
	require.Equal(t, jobbatch.StateFailed, denied.State)
	require.Equal(t, "tool_error", denied.Code)

	broken := batchResultFromResponse(fantasy.ToolResponse{}, errors.New("boom"), false)
	require.Equal(t, jobbatch.StateFailed, broken.State)
	require.Equal(t, "tool_run_error", broken.Code)

	stopped := batchResultFromResponse(fantasy.NewTextResponse("partial"), nil, true)
	require.Equal(t, jobbatch.StateCancelled, stopped.State)

	img := batchResultFromResponse(fantasy.NewImageResponse([]byte{1, 2, 3}, "image/png"), nil, false)
	var back fantasy.ToolResponse
	require.NoError(t, json.Unmarshal(img.Details, &back))
	require.Equal(t, []byte{1, 2, 3}, back.Data)
	require.Equal(t, "image/png", back.MediaType)
	require.True(t, jobbatch.ValidCode(denied.Code) && jobbatch.ValidCode(broken.Code))
}
