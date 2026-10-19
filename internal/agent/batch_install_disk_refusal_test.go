package agent

// R12-H2 §1/§3: a custom DiskProvider supplied through the trusted call
// state (BindCallDiskProvider) with an App-owned capability manager is a
// SUPPORTED provider — the runtime must NOT refuse it; execution proceeds
// through the real ingress, executor and Runner. An explicit OSDisk is not
// custom-provider loss either: it runs without any binding. Only a payload
// that MARKS itself provider-backed without a binding handle is refused,
// fail-closed, with ErrDiskProviderUnavailable (batch_exec.go Start).

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

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
	submits      int
	submitsBound int
	mu           sync.Mutex
	roots        []jobbatch.ID
}

// record remembers every runner-minted root so a test can drive the REAL
// execution-owner release trigger: jobbatch disposes a settled root's
// execution ONLY when the root is released — Runner.Release fires
// EventRelease → handle.giveUp → maybeDispose → the physical-Dispose
// wrapper drops the Execution owner (runner_commit.go, runner_control.go).
// A retained root legitimately keeps its Execution owner (§1.2 binds
// release to physical Dispose).
func (r *diskRefusalRunner) record(root jobbatch.ID) {
	r.mu.Lock()
	r.roots = append(r.roots, root)
	r.mu.Unlock()
}

func (r *diskRefusalRunner) recordedRoots() []jobbatch.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]jobbatch.ID(nil), r.roots...)
}

func (r *diskRefusalRunner) Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error) {
	r.mu.Lock()
	r.submits++
	r.mu.Unlock()
	id, err := r.batchRunnerHandle.Submit(ctx, req)
	if err == nil {
		r.record(id)
	}
	return id, err
}

// SubmitBound forwards to the REAL installed handle so the fixture satisfies
// batchBoundRunnerHandle: a root-owning (bound) Run acquires the Root owner
// and must submit through SubmitBound, not silently fail closed in the
// control's type assertion (R12-H2a P1-6).
func (r *diskRefusalRunner) SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error) {
	r.mu.Lock()
	// A payload carrying a RegistryID goes through SubmitBound, not Submit
	// (batch_ingress.go): count it into the same observable so assertions on
	// "the call reached the real Runner" hold for bound runs too.
	r.submits++
	r.submitsBound++
	r.mu.Unlock()
	bound, ok := r.batchRunnerHandle.(batchBoundRunnerHandle)
	if !ok {
		return "", nil, errors.New("diskRefusalRunner: installed handle does not implement SubmitBound")
	}
	id, batch, err := bound.SubmitBound(ctx, req)
	if err == nil {
		r.record(id)
	}
	return id, batch, err
}

// Release forwards to the REAL installed handle so the fixture satisfies
// batchReleaseHandle: the ingress/model control adopt the Runner's Release
// as the AUTOMATIC post-Done disposal trigger, so the decorator must not
// swallow it (R12-H2 §1.2).
func (r *diskRefusalRunner) Release(ctx context.Context, id jobbatch.ID) error {
	handle, ok := r.batchRunnerHandle.(batchReleaseHandle)
	if !ok {
		return errors.New("diskRefusalRunner: installed handle does not implement Release")
	}
	return handle.Release(ctx, id)
}

func diskRefusalInvocation(env *batchInventoryEnv, bindingID string, custom bool) BatchInvocation {
	return BatchInvocation{
		SessionID: env.sessionID, MessageID: "msg-disk", RegistryID: env.registryID,
		Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupIO],
		Policy: batchLeafPolicyContext{
			// The REAL restricted-toolset fingerprint of the pinned
			// registry (the executor validates it when a binding id is
			// carried).
			RegistryID:        env.registryID,
			RestrictedToolset: batchToolsetFingerprint([]fantasy.AgentTool{env.leaf}),
			CustomDisk:        map[bool]string{true: "supported-provider", false: ""}[custom],
			// A supported custom provider carries its immutable binding
			// handle, minted by BindCallDiskProvider with the trusted call
			// state; OSDisk carries none.
			DiskProvider: bindingID != "", DiskBindingID: bindingID,
		},
	}
}

// bindTestCallDiskProvider mints the trusted call state's binding handle
// through the PRODUCTION entry point: it registers the provider in the
// App-owned manager and acquires the Call owner. The returned closure
// releases exactly that owner.
func bindTestCallDiskProvider(t *testing.T, env *batchInventoryEnv, provider tools.DiskProvider) (*CallOptions, func()) {
	t.Helper()
	opts, release, err := BindCallDiskProvider(env.install.DiskBindings, provider, "test-call-"+env.sessionID, nil)
	require.NoError(t, err, "a custom provider bound through the trusted call state is supported")
	return opts, release
}

func TestDiskProviderCustomAndOSDiskAreSupported(t *testing.T) {
	cases := []struct {
		name     string
		provider func() tools.DiskProvider
		custom   bool
	}{
		// A non-comparable (map/slice-bearing) provider value is a legal
		// custom provider: binding identity is the opaque minted ID, never
		// the provider value.
		{name: "non-comparable provider", provider: func() tools.DiskProvider { return diskRefusalProvider{values: []string{"provider"}} }, custom: true},
		// Explicit OSDisk is not custom-provider loss: it runs without any
		// binding.
		{name: "explicit OSDisk", provider: func() tools.DiskProvider { return tools.OSDisk() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newBatchInventoryEnv(t, "view")
			attach := &diskRefusalAttach{batchAttachStore: env.install.Runtime.cfg.Attach}
			runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
			env.install.Runtime.cfg.Attach = attach
			env.install.Runtime.cfg.Runner = runner
			ctx := env.ctx
			bindingID := ""
			if tc.custom {
				opts, release := bindTestCallDiskProvider(t, env, tc.provider())
				ctx = WithCallOptions(ctx, opts)
				bindingID = opts.DiskBindingID
				defer release()
			} else {
				ctx = WithCallOptions(ctx, &CallOptions{DiskProvider: tc.provider()})
			}
			inv := diskRefusalInvocation(env, bindingID, tc.custom)
			resp, err := env.install.Runtime.Invoke(ctx, inv, env.leaf,
				fantasy.ToolCall{ID: "disk-supported", Name: "view", Input: `{}`})
			require.NoError(t, err, "a supported provider must not be refused")
			require.False(t, resp.IsError, "leaf response: %s", resp.Content)
			require.Positive(t, attach.probes, "execution proceeded through the real attach probe")
			require.Positive(t, runner.submits, "execution proceeded through the real Runner Submit")
			if tc.custom {
				require.Positive(t, runner.submitsBound, "a root-owning bound run submitted through SubmitBound")
			}
			if tc.custom {
				// The registered binding is live while the call owns it.
				_, err := env.install.DiskBindings.Lookup(bindingID)
				require.NoError(t, err)
			}
		})
	}
}

// TestDiskProviderBindingDiesWhenEveryOwnerEnds pins the owner-set deletion
// rule (R12-H2 §1.1) end to end: the Call owner is released explicitly (the
// release token BindCallDiskProvider returned), the Root owner is released by
// the Done watcher and the Execution owner ONLY by the physical-Dispose
// wrapper — then the binding's owner set is empty and the binding is deleted.
func TestDiskProviderBindingDiesWhenEveryOwnerEnds(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	// Record the runner-minted root so this test can drive the real
	// execution-owner release trigger below.
	runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
	env.install.Runtime.cfg.Runner = runner
	opts, release := bindTestCallDiskProvider(t, env, diskRefusalProvider{values: []string{"ownerless"}})
	ctx := WithCallOptions(env.ctx, opts)
	inv := diskRefusalInvocation(env, opts.DiskBindingID, true)
	resp, err := env.install.Runtime.Invoke(ctx, inv, env.leaf,
		fantasy.ToolCall{ID: "disk-owners", Name: "view", Input: `{}`})
	require.NoError(t, err)
	require.False(t, resp.IsError, "leaf response: %s", resp.Content)
	_, err = env.install.DiskBindings.Lookup(opts.DiskBindingID)
	require.NoError(t, err, "the binding is live while the call owner holds it")

	// Release EXACTLY this test's own Call owner — never a descendant's.
	release()
	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2,
	// round-2 automatic disposal): the root settled (Batch.Done released the
	// Root owner) and the ingress's adopt-armed Done watcher already invoked
	// Runner.Release → EventRelease → handle.giveUp → maybeDispose — the
	// physical-Dispose wrapper drops the Execution owner. A manual
	// Runner.Release here would now be a double release ("jobbatch: root
	// released"), so the binding must die from the automatic disposal alone.
	require.Equal(t, 1, len(runner.recordedRoots()), "one bound root for the one-item call")
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(opts.DiskBindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond,
		"binding must die when every owner (call, root, execution) is gone")
}

func TestDiskProviderSupportedThroughModelFacingIngressRoutes(t *testing.T) {
	for _, route := range []string{"coordinator", "cli_bridge", "agentic_fetch"} {
		t.Run(route, func(t *testing.T) {
			env := newBatchInventoryEnv(t, "view")
			inventoryBindProcess(t, env)
			attach := &diskRefusalAttach{batchAttachStore: env.install.Runtime.cfg.Attach}
			runner := &diskRefusalRunner{batchRunnerHandle: env.install.Runtime.cfg.Runner}
			env.install.Runtime.cfg.Attach = attach
			env.install.Runtime.cfg.Runner = runner
			var ctx context.Context
			if route == "coordinator" {
				// The custom provider goes through the trusted call state
				// (BindCallDiskProvider) on the coordinator build route.
				opts, release := bindTestCallDiskProvider(t, env, diskRefusalProvider{values: []string{"routed"}})
				defer release()
				ctx = WithCallOptions(batchCallCtx(env.sessionID), opts)
			} else {
				// Explicit OSDisk is not custom-provider loss: it runs
				// without any binding on the bridge and private-fetch
				// routes.
				ctx = WithCallOptions(batchCallCtx(env.sessionID), &CallOptions{DiskProvider: tools.OSDisk()})
			}
			ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-disk")
			var resp fantasy.ToolResponse
			var err error
			switch route {
			case "coordinator":
				built, buildErr := newBatchWorkToolBuild(processCoordinatorBatchBinding(), []fantasy.AgentTool{env.leaf}, newBuildResourceScope(), false)
				require.NoError(t, buildErr)
				resp, err = built.tools[0].Run(ctx, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{}`})
			case "cli_bridge":
				stamped := withBatchBridgeInvocation(ctx, batchBridgeInvocation{RegistryID: env.registryID, MessageID: "msg-disk"})
				bridge := coordinatorBridgeFallback(stamped)
				require.NotNil(t, bridge)
				resp, err = bridge.Call(stamped, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{"path":"x"}`})
			case "agentic_fetch":
				built := armPrivateFetchIngress(newPolicyToolBuild([]fantasy.AgentTool{env.leaf}), processCoordinatorBatchBinding().ingress, newBuildResourceScope())
				resp, err = built.tools[0].Run(ctx, fantasy.ToolCall{ID: "disk-route", Name: "view", Input: `{}`})
			}
			require.NoError(t, err)
			require.False(t, resp.IsError, "route %s response: %s", route, resp.Content)
			require.Positive(t, attach.probes, "route %s crossed the real attach probe", route)
			require.Positive(t, runner.submits, "route %s reached the real Runner Submit", route)
			if route == "coordinator" {
				require.Positive(t, runner.submitsBound, "the bound coordinator route submitted through SubmitBound")
			}
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
			require.ErrorIs(t, err, ErrDiskProviderUnavailable,
				"executor admission must fail closed on a provider-marked payload without a binding handle")
		})
	}
}
