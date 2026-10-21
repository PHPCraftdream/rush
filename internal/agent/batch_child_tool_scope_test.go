package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type pctScopeIngress struct {
	mu       sync.Mutex
	caps     *DiskProviderRegistry
	regs     map[string]*policyLeafRegistry
	next     int
	fail     bool
	accepted bool
	claim    bool
	handoff  *pctDelegationHandoff
}

func (i *pctScopeIngress) PinLeafRegistry(r *BatchLeafRegistry) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.next++
	id := fmt.Sprintf("pct-registry-%d", i.next)
	if i.regs == nil {
		i.regs = make(map[string]*policyLeafRegistry)
	}
	i.regs[id] = r
	return id, nil
}

func (i *pctScopeIngress) Registry(id string) (*BatchLeafRegistry, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.regs[id]
	if r == nil {
		return nil, errors.New("missing registry")
	}
	return r, nil
}

func (i *pctScopeIngress) ResolveLeaf(id, name string) (fantasy.AgentTool, error) {
	r, err := i.Registry(id)
	if err != nil {
		return nil, err
	}
	return r.resolve(name)
}

// Invoke models delivery only: it is not the production batch Submit/executor.
func (i *pctScopeIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if inv.RegistryID != "" {
		key := diskOwnerKey{kind: diskOwnerRoot, instance: "pct-root-" + call.ID, registryID: inv.RegistryID}
		if err := i.caps.acquire(key); err != nil {
			return fantasy.ToolResponse{}, err
		}
		defer i.caps.release(key)
	}
	i.mu.Lock()
	fail, accepted, claim := i.fail, i.accepted, i.claim
	i.mu.Unlock()
	if fail {
		return fantasy.ToolResponse{}, errors.New("simulated admission rejection")
	}
	if call.Name == AgentToolName {
		h := pctHandoffFrom(ctx)
		if h == nil {
			return fantasy.ToolResponse{}, errors.New("missing invocation handoff")
		}
		i.mu.Lock()
		i.handoff = h
		i.mu.Unlock()
		if accepted {
			h.pctAccepted()
		}
		if claim {
			if pctCapturedToolFrom(leaf) == nil || pctCapturedToolFrom(leaf).pctClaim(pctInvocationKey(tools.GetSessionFromContext(ctx), call.ID)) != h {
				return fantasy.ToolResponse{}, errors.New("captured handoff not claimable")
			}
		}
		return fantasy.NewTextResponse("simulated accepted start"), nil
	}
	return leaf.Run(ctx, call)
}

type pctScopeFixture struct {
	caps     *DiskProviderRegistry
	ingress  *pctScopeIngress
	child    *pctChildToolScope
	parent   *buildResourceScope
	view     *batchIngressTool
	captured *pctCapturedAgentTool
	outer    *batchIngressTool
	registry string
}

func pctNewScopeFixture(t *testing.T, accepted, claim bool) *pctScopeFixture {
	t.Helper()
	caps := NewDiskProviderRegistry()
	ingress := &pctScopeIngress{caps: caps, accepted: accepted, claim: claim}
	caps.setRegistryDrop(func(id string) {
		ingress.mu.Lock()
		delete(ingress.regs, id)
		ingress.mu.Unlock()
	})
	bind := &batchCoordinatorBinding{ingress: ingress, diskBindings: caps}
	parent := newBuildResourceScope()
	child := pctNewChildToolScope()
	parent.register(child.pctProvisionalRelease())
	viewTool := &countingTool{name: "view"}
	viewBuild, err := newBatchWorkToolBuild(bind, []fantasy.AgentTool{viewTool}, child.scope, false)
	require.NoError(t, err, "build the inner child tool")
	require.NotEmpty(t, viewBuild.registryID, "inner tool registry is pinned")
	view, ok := viewBuild.tools[0].(*batchIngressTool)
	require.True(t, ok, "inner tool is armed through the batch ingress")
	captured := &pctCapturedAgentTool{AgentTool: &countingTool{name: AgentToolName}, scope: child}
	outerBuild, err := newBatchWorkToolBuild(bind, []fantasy.AgentTool{captured}, parent, false)
	require.NoError(t, err, "build outer tool through captured wrapper")
	require.NotEmpty(t, outerBuild.registryID, "outer registry is pinned")
	outer, ok := outerBuild.tools[0].(*batchIngressTool)
	require.True(t, ok, "outer tool is armed through the batch ingress")
	require.Equal(t, child, outer.pctChildScope, "outer assembly captures the actual child scope")
	t.Cleanup(func() { parent.close() })
	return &pctScopeFixture{caps: caps, ingress: ingress, child: child, parent: parent, view: view, captured: captured, outer: outer, registry: viewBuild.registryID}
}

func (f *pctScopeFixture) pctStart(t *testing.T, id string) *pctDelegationHandoff {
	t.Helper()
	resp, err := f.outer.Run(batchCallCtx("pct-parent"), fantasy.ToolCall{ID: id, Name: AgentToolName, Input: `{}`})
	require.NoError(t, err, "simulate outer agent admission")
	require.False(t, resp.IsError)
	f.ingress.mu.Lock()
	h := f.ingress.handoff
	f.ingress.mu.Unlock()
	require.NotNil(t, h)
	return h
}

func (f *pctScopeFixture) pctRunView(t *testing.T, id string) {
	t.Helper()
	resp, err := f.view.Run(batchCallCtx("pct-child"), fantasy.ToolCall{ID: id, Name: "view", Input: `{}`})
	require.NoError(t, err, "child tool invocation completes through ingress")
	require.False(t, resp.IsError)
}

func TestBatchChildToolScope_ParentCloseBeforeFirstTool(t *testing.T) {
	f := pctNewScopeFixture(t, true, true)
	h := f.pctStart(t, "start-before-first")
	t.Cleanup(h.pctRelease)
	f.parent.close()
	_, err := f.ingress.Registry(f.registry)
	require.NoError(t, err, "outer execution lease preserves registry after parent close")
	f.pctRunView(t, "first")
	f.captured.mu.Lock()
	defer f.captured.mu.Unlock()
	require.Empty(t, f.captured.handoffs, "execution claim removes the handoff from the captured map")
}

func TestBatchChildToolScope_SecondReaction(t *testing.T) {
	f := pctNewScopeFixture(t, true, true)
	h := f.pctStart(t, "start-reactions")
	t.Cleanup(h.pctRelease)
	f.parent.close()
	f.pctRunView(t, "reaction-1")
	f.pctRunView(t, "reaction-2")
	f.ingress.mu.Lock()
	defer f.ingress.mu.Unlock()
	require.Equal(t, 2, f.ingress.next, "second reaction uses the same pinned child registry")
}

func TestBatchChildToolScope_RejectedAdmissionAndFailedLaunch(t *testing.T) {
	t.Run("rejected admission", func(t *testing.T) {
		f := pctNewScopeFixture(t, false, false)
		f.ingress.fail = true
		_, err := f.outer.Run(batchCallCtx("pct-parent"), fantasy.ToolCall{ID: "rejected", Name: AgentToolName, Input: `{}`})
		require.Error(t, err)
		f.parent.close()
		_, count := f.caps.ownerCounts()
		require.Zero(t, count, "deferred pending cleanup releases a rejected handoff")
	})
	t.Run("handoff helper release", func(t *testing.T) {
		f := pctNewScopeFixture(t, true, false)
		h := f.pctStart(t, "launch-failed")
		t.Cleanup(h.pctRelease)
		h.pctRelease() // Unit helper only; this fake does not launch a child executor.
		f.parent.close()
		_, count := f.caps.ownerCounts()
		require.Zero(t, count, "explicit failed-launch cleanup releases the retained child reference")
	})
}

func TestBatchChildToolScope_ClosureGeneration(t *testing.T) {
	f := pctNewScopeFixture(t, true, true)
	h := f.pctStart(t, "driver-generation")
	t.Cleanup(h.pctRelease)
	oldRelease := h.pctClaimDriver()
	require.Len(t, oldRelease, 1, "driver retain can be claimed before execution handoff release")
	newRelease := f.child.pctRetain()
	require.NotNil(t, newRelease)
	f.parent.close()
	drivers := newSubAgentDriverRegistry()
	oldGeneration := drivers.register("child", subAgentDriver{pctReleases: oldRelease})
	currentGeneration := drivers.register("child", subAgentDriver{pctReleases: []func(){newRelease}})
	require.False(t, drivers.releaseIfCurrent("child", oldGeneration), "stale generation cannot release current driver")
	driver, ok := drivers.get("child")
	require.True(t, ok)
	require.Equal(t, currentGeneration, driver.generation)
	require.Len(t, driver.pctReleases, 2, "replacement preserves prior driver references")
	require.True(t, drivers.releaseIfCurrent("child", currentGeneration))
	require.False(t, drivers.releaseIfCurrent("child", currentGeneration), "generation release is idempotent")
	h.pctRelease()
	_, count := f.caps.ownerCounts()
	require.Zero(t, count)
}

func TestBatchChildToolScope_UnrelatedScope(t *testing.T) {
	a := pctNewScopeFixture(t, true, true)
	b := pctNewScopeFixture(t, true, true)
	ha := a.pctStart(t, "unrelated-a")
	hb := b.pctStart(t, "unrelated-b")
	t.Cleanup(ha.pctRelease)
	t.Cleanup(hb.pctRelease)
	a.parent.close()
	_, err := b.ingress.Registry(b.registry)
	require.NoError(t, err, "closing scope A does not close independently managed scope B")
	b.parent.close()
	b.pctRunView(t, "unrelated-b-view")
}

func TestBatchChildToolScope_Disposal(t *testing.T) {
	f := pctNewScopeFixture(t, true, true)
	h := f.pctStart(t, "dispose")
	t.Cleanup(h.pctRelease)
	driverReleases := h.pctClaimDriver()
	f.parent.close()
	adapter := &pctDriverDisposer{SessionAgent: &mockSessionAgent{}, releases: driverReleases}
	require.False(t, adapter.CancelAll(), "mock session agent is idle")
	h.pctRelease()
	_, count := f.caps.ownerCounts()
	require.Zero(t, count, "handoff and driver disposal release their references")
}

func TestBatchChildToolScope_AcceptedWithoutDriver(t *testing.T) {
	f := pctNewScopeFixture(t, true, false)
	h := f.pctStart(t, "accepted-no-driver")
	t.Cleanup(h.pctReleaseUnstarted)
	f.parent.close()
	_, count := f.caps.ownerCounts()
	require.Equal(t, 1, count, "accepted but unstarted handoff retains the child scope")
	h.pctReleaseUnstarted()
	h.pctReleaseUnstarted()
	_, count = f.caps.ownerCounts()
	require.Zero(t, count, "root-Done-style cleanup releases accepted unstarted handoff once")
	f.captured.mu.Lock()
	defer f.captured.mu.Unlock()
	require.Empty(t, f.captured.handoffs, "cleanup removes the expected handoff map entry")
}

// Production registration (BatchLeafRegistryStore.Register) wraps every agent
// leaf in *batchCredentialLeaf, so the executor-side lookup must see through
// it; a nil here leaves the handoff unclaimed in production.
func TestBatchChildToolScope_LookupSeesThroughCredentialLeaf(t *testing.T) {
	captured := &pctCapturedAgentTool{}
	wrapped := &batchCredentialLeaf{AgentTool: captured}
	require.Same(t, captured, pctCapturedToolFrom(wrapped), "credential wrapper is unwrapped")
	require.Same(t, captured, pctCapturedToolFrom(&batchCredentialLeaf{AgentTool: &hookedTool{inner: captured}}), "wrapper over a hooked tool is unwrapped")
	require.Nil(t, pctCapturedToolFrom(&batchCredentialLeaf{}), "wrapper over nothing resolves to no captured tool")
}
