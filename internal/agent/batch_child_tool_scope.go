package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
)

// pct backlog: production integration regressions are still wanted for batch Submit acceptance/rejection, Done watcher cleanup, executor claim timing, and driver transfer/disposal.
type pctDelegationHandoffKey struct{}

type pctChildToolScope struct {
	mu     sync.Mutex
	refs   int
	closed bool
	scope  *buildResourceScope
}

func pctNewChildToolScope() *pctChildToolScope {
	return &pctChildToolScope{refs: 1, scope: newBuildResourceScope()}
}

func (s *pctChildToolScope) pctRetain() func() {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.refs++
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(s.pctRelease) }
}

func (s *pctChildToolScope) pctRelease() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.refs--
	closeScope := s.refs == 0
	if closeScope {
		s.closed = true
	}
	s.mu.Unlock()
	if closeScope {
		s.scope.close()
	}
}

func (s *pctChildToolScope) pctProvisionalRelease() func() {
	var once sync.Once
	return func() { once.Do(s.pctRelease) }
}

func pctChildToolBuildContext(ctx context.Context) (context.Context, *pctChildToolScope) {
	if callOptionsFrom(ctx) == nil {
		return ctx, nil
	}
	parent := callBuildScopeFrom(ctx)
	if parent == nil {
		return ctx, nil
	}
	scope := pctNewChildToolScope()
	opts := *callOptionsFrom(ctx)
	opts.buildScope = scope.scope
	parent.register(scope.pctProvisionalRelease())
	ctx = WithCallOptions(ctx, &opts)
	return ctx, scope
}

type pctCapturedAgentTool struct {
	fantasy.AgentTool
	scope    *pctChildToolScope
	mu       sync.Mutex
	handoffs map[string]*pctDelegationHandoff
}

func (t *pctCapturedAgentTool) pctRegister(key string, h *pctDelegationHandoff) bool {
	t.mu.Lock()
	if t.handoffs == nil {
		t.handoffs = make(map[string]*pctDelegationHandoff)
	}
	if _, exists := t.handoffs[key]; exists {
		t.mu.Unlock()
		return false
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		t.mu.Unlock()
		return false
	}
	h.captured, h.key = t, key
	h.mu.Unlock()
	t.handoffs[key] = h
	t.mu.Unlock()
	return true
}

func (t *pctCapturedAgentTool) pctRemove(key string, expected *pctDelegationHandoff) {
	t.mu.Lock()
	if t.handoffs[key] == expected {
		delete(t.handoffs, key)
	}
	t.mu.Unlock()
}

func (t *pctCapturedAgentTool) pctClaim(key string) *pctDelegationHandoff {
	t.mu.Lock()
	h := t.handoffs[key]
	if h != nil && !h.pctClaimExecution() {
		h = nil
	}
	if h != nil {
		delete(t.handoffs, key)
	}
	t.mu.Unlock()
	return h
}

func pctClaimCapturedHandoff(env batchLeafEnv, p batchLeafPayload) *pctDelegationHandoff {
	ingress := callIngress(env)
	if ingress == nil {
		return nil
	}
	registry, err := ingress.Registry(p.RegistryID)
	if err != nil {
		return nil
	}
	leaf, err := registry.resolve(p.Tool)
	if err != nil {
		return nil
	}
	captured := pctCapturedToolFrom(leaf)
	if captured == nil {
		return nil
	}
	h := captured.pctClaim(pctInvocationKey(p.SessionID, p.ToolCallID))
	return h
}

func pctCapturedToolFrom(tool fantasy.AgentTool) *pctCapturedAgentTool {
	for tool != nil {
		switch wrapped := tool.(type) {
		case *pctCapturedAgentTool:
			return wrapped
		case *restrictedTool:
			tool = wrapped.AgentTool
		case restrictedTool:
			tool = wrapped.AgentTool
		case *hookedTool:
			tool = wrapped.inner
		case *loggedTool:
			tool = wrapped.inner
		case *batchCredentialLeaf:
			// pct: BatchLeafRegistryStore.Register wraps every agent leaf.
			tool = wrapped.AgentTool
		default:
			return nil
		}
	}
	return nil
}

func pctCapturedScopeFromTool(tool fantasy.AgentTool) *pctChildToolScope {
	if t := pctCapturedToolFrom(tool); t != nil {
		return t.scope
	}
	return nil
}

func pctInvocationKey(sessionID, callID string) string { return sessionID + "\x00" + callID }

func pctReleaseScopeRefs(releases []func()) {
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

type pctDelegationHandoff struct {
	mu       sync.Mutex
	releases []func()
	pctChild *pctChildToolScope
	captured *pctCapturedAgentTool
	key      string
	claimed  bool
	released bool
	accepted bool
	once     sync.Once
}

func pctNewDelegationHandoff(releases []func()) *pctDelegationHandoff {
	return &pctDelegationHandoff{releases: releases}
}

func (h *pctDelegationHandoff) pctAccepted() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.accepted = true
	h.mu.Unlock()
}

func (h *pctDelegationHandoff) pctClaimExecution() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.claimed || h.released {
		return false
	}
	h.claimed = true
	return true
}

func (h *pctDelegationHandoff) pctClaimDriver() []func() {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || h.pctChild == nil {
		return nil
	}
	release := h.pctChild.pctRetain()
	if release == nil {
		return nil
	}
	return []func(){release}
}

func (h *pctDelegationHandoff) pctReleasePending() {
	h.pctReleasePendingState(false)
}

func (h *pctDelegationHandoff) pctReleaseUnstarted() {
	h.pctReleasePendingState(true)
}

func (h *pctDelegationHandoff) pctReleasePendingState(includeAccepted bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.claimed || h.released || (!includeAccepted && h.accepted) {
		h.mu.Unlock()
		return
	}
	h.released = true
	captured, key := h.captured, h.key
	h.mu.Unlock()
	if captured != nil {
		captured.pctRemove(key, h)
	}
	h.once.Do(func() { pctReleaseScopeRefs(h.releases) })
}

func (h *pctDelegationHandoff) pctRelease() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return
	}
	h.released = true
	captured, key := h.captured, h.key
	h.mu.Unlock()
	if captured != nil {
		captured.pctRemove(key, h)
	}
	h.once.Do(func() { pctReleaseScopeRefs(h.releases) })
}

func pctHandoffFrom(ctx context.Context) *pctDelegationHandoff {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(pctDelegationHandoffKey{}).(*pctDelegationHandoff)
	return h
}

type pctDriverDisposer struct {
	SessionAgent
	releases []func()
}

func (d *pctDriverDisposer) CancelAll() bool {
	busy := d.SessionAgent.CancelAll()
	if !busy {
		pctReleaseScopeRefs(d.releases)
	}
	return busy
}
