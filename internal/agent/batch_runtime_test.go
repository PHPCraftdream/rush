package agent

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// Not parallel: shares folder-scope globals with the collision test.
var (
	folderScopeA = permission.FolderScopeSpec{WorkingDir: "/w", Entries: []permission.FolderScopeEntry{{Dir: "a", Ops: []permission.FileOp{permission.FileOpList}}}}
	folderScopeB = permission.FolderScopeSpec{WorkingDir: "/w", Entries: []permission.FolderScopeEntry{{Dir: "b", Ops: []permission.FileOp{permission.FileOpList}}}}
)

func testRuntimeConfig() batchRuntimeConfig {
	profile, err := NewBatchProfileDefault()
	if err != nil {
		panic(err)
	}
	return batchRuntimeConfig{
		Host:     "host-1",
		RunnerID: "runner-1",
		Profile:  profile,
		Leaves:   func(string) (*policyLeafRegistry, error) { return nil, errors.New("unused") },
		Attach:   newFakeAttach(),
	}
}

func TestNewBatchRuntimeRejectsMissingDeps(t *testing.T) {
	t.Parallel()
	cfg := testRuntimeConfig()
	cfg.Host = ""
	_, err := newBatchRuntime(cfg)
	require.Error(t, err)
	cfg = testRuntimeConfig()
	cfg.Profile = nil
	_, err = newBatchRuntime(cfg)
	require.ErrorIs(t, err, errBatchProfileNotWired)
	cfg = testRuntimeConfig()
	cfg.Leaves = nil
	_, err = newBatchRuntime(cfg)
	require.ErrorIs(t, err, errBatchLeavesNotWired)
	cfg = testRuntimeConfig()
	cfg.Attach = nil
	_, err = newBatchRuntime(cfg)
	require.ErrorIs(t, err, errBatchAttachNotWired)
}

// Not parallel: goroutine-count observation must run exclusively.
func TestNewBatchRuntimeNoSideEffects(t *testing.T) {
	before := runtime.NumGoroutine()
	runtime.Gosched()
	leafCalls := 0
	cfg := testRuntimeConfig()
	cfg.Leaves = func(string) (*policyLeafRegistry, error) {
		leafCalls++
		return nil, errors.New("unused")
	}
	for i := 0; i < 25; i++ {
		rt, err := newBatchRuntime(cfg)
		require.NoError(t, err)
		require.NotNil(t, rt)
		attach := cfg.Attach.(*fakeAttach)
		require.Zero(t, attach.probeCalls(), "construction must not probe")
	}
	require.Zero(t, leafCalls, "construction must not resolve leaves")
	require.LessOrEqual(t, runtime.NumGoroutine(), before, "construction must not start goroutines")
}

func TestBindRunnerOnce(t *testing.T) {
	t.Parallel()
	rt, err := newBatchRuntime(testRuntimeConfig())
	require.NoError(t, err)
	require.NoError(t, rt.bindRunner(newFakeRunner(nil)))
	require.Error(t, rt.bindRunner(newFakeRunner(nil)))
	require.Error(t, rt.bindRunner(nil))
}

func TestInvokeDeliveryNotWired(t *testing.T) {
	t.Parallel()
	rt, err := newBatchRuntime(testRuntimeConfig())
	require.NoError(t, err)
	resp, err := rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"), newTestCall("call-1", `{"a":1}`))
	require.ErrorIs(t, err, errBatchDeliveryNotWired)
	require.Zero(t, resp)
}

func TestInvokeRunnerNotWired(t *testing.T) {
	t.Parallel()
	cfg := testRuntimeConfig()
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("view"), newTestCall("call-1", `{"a":1}`))
	require.ErrorIs(t, err, errBatchRunnerNotWired)
}

// Concurrent invocations from ONE session with different restricted toolsets
// and folder scopes must carry their own policy context and pinned registry
// into the executor transport: no session-keyed collision anywhere.
func TestConcurrentInvocationsNoPolicyCollision(t *testing.T) {
	t.Parallel()
	attach := newFakeAttach()
	runner := newFakeRunner(nil)
	cfg := testRuntimeConfig()
	cfg.Attach = attach
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	leafA := &policyLeafRegistry{entries: map[string]policyLeafEntry{"view": {}}}
	leafB := &policyLeafRegistry{entries: map[string]policyLeafEntry{"view": {}}}
	resolved := make(map[string]*policyLeafRegistry)
	cfg.Leaves = func(registryID string) (*policyLeafRegistry, error) {
		if registryID == "reg-a" {
			resolved[registryID] = leafA
			return leafA, nil
		}
		if registryID == "reg-b" {
			resolved[registryID] = leafB
			return leafB, nil
		}
		return nil, errors.New("unknown registry")
	}
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)

	invA, invB := testInvocation(), testInvocation()
	invA.RegistryID, invA.Policy.RegistryID = "reg-a", "reg-a"
	invA.Policy.FolderScope = &folderScopeA
	invA.Policy.RestrictedToolset = "toolset-a"
	invB.RegistryID, invB.Policy.RegistryID = "reg-b", "reg-b"
	invB.Policy.FolderScope = &folderScopeB
	invB.Policy.RestrictedToolset = "toolset-b"
	invB.Policy.MessageID = "msg-2"

	start := make(chan struct{})
	done := make(chan error, 2)
	for i, tc := range []struct {
		inv BatchInvocation
	}{{invA}, {invB}} {
		callID := "call-a"
		if i == 1 {
			callID = "call-b"
		}
		go func(callID string, inv BatchInvocation) {
			<-start
			_, err := rt.Invoke(context.Background(), inv, newTestLeaf("view"),
				newTestCall(callID, `{"a":1}`))
			done <- err
		}(callID, tc.inv)
	}
	close(start)
	require.NoError(t, <-done)
	require.NoError(t, <-done)

	specs := runner.submitSpecs()
	require.Len(t, specs, 2)
	seen := make(map[string]bool, 2)
	for _, spec := range specs {
		p, err := decodeLeafPayload(spec)
		require.NoError(t, err)
		require.NotNil(t, p.Policy)
		require.False(t, seen[p.Policy.RegistryID], "each invocation carries its own policy context")
		seen[p.Policy.RegistryID] = true
		// The full policy set travels: scope, allowlist, message id.
		ctx := batchLeafRunContext(context.Background(), p)
		require.Equal(t, p.Policy.FolderScope, ctx.Value(batchFolderScopeContextKey{}))
		require.Equal(t, p.Policy.RunAllowlist, ctx.Value(batchRunAllowlistContextKey{}))
		require.NotEmpty(t, p.Policy.MessageID)
		require.NotEmpty(t, p.Policy.RestrictedToolset)
	}
	require.Contains(t, seen, "reg-a")
	require.Contains(t, seen, "reg-b")
	// Leaf resolution is keyed by registry identity, never by session.
	require.Same(t, leafA, resolved["reg-a"])
	require.Same(t, leafB, resolved["reg-b"])
	require.NotSame(t, leafA, leafB)
	env := rt.LeafEnv()
	gotA, err := env.Leaves("reg-a")
	require.NoError(t, err)
	require.Same(t, leafA, gotA)
	_, err = env.Leaves("sess-1")
	require.Error(t, err, "session-keyed lookup must fail closed")
}

func TestInvokeValidateSubmitFailsBeforeRunnerCall(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(nil)
	cfg := testRuntimeConfig()
	cfg.Runner = runner
	cfg.Delivery = newFakeDelivery(fantasy.ToolResponse{})
	cfg.MaxNodes = 1
	rt, err := newBatchRuntime(cfg)
	require.NoError(t, err)
	// An unclassifiable leaf name fails admission before any Submit.
	_, err = rt.Invoke(context.Background(), testInvocation(), newTestLeaf("mystery_tool"),
		newTestCall("call-1", `{"a":1}`))
	require.ErrorIs(t, err, errBatchSubmitBeforeInvalid)
	require.True(t, errors.Is(err, errBatchUnknownTool))
	require.Zero(t, runner.submitCalls(), "runner must not be called after admission failure")
}
