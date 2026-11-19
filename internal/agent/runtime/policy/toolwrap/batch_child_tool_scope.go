// Package toolwrap holds the pct delegation-handoff and child build-scope
// plumbing shared by the batch ingress tool assembly (ported from package
// agent's batch_child_tool_scope.go). The captured agent tool itself and the
// captured-wrapper traversal stay with the batch-inst leaf wrappers in
// package agent; this package speaks to them only through the narrow
// capturedRemover / CapturedClaimer capability interfaces and the resolver
// callback on ClaimCapturedHandoff.
package toolwrap

import (
	"context"
	"sync"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

// pct backlog: production integration regressions are still wanted for batch Submit acceptance/rejection, Done watcher cleanup, executor claim timing, and driver transfer/disposal.
type delegationHandoffKey struct{}

type PctChildToolScope struct {
	mu     sync.Mutex
	refs   int
	closed bool
	scope  *agentcall.BuildResourceScope
}

func newChildToolScope() *PctChildToolScope {
	return &PctChildToolScope{refs: 1, scope: agentcall.NewBuildResourceScope()}
}

func (s *PctChildToolScope) Retain() func() {
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
	return func() { once.Do(s.Release) }
}

func (s *PctChildToolScope) Release() {
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
		s.scope.Close()
	}
}

func (s *PctChildToolScope) provisionalRelease() func() {
	var once sync.Once
	return func() { once.Do(s.Release) }
}

// ChildToolBuildContext isolates the child's captured tool build scope: a
// fresh PctChildToolScope borrows the parent call's options and re-stamps a
// copied CallOptions onto the child context.
func ChildToolBuildContext(ctx context.Context) (context.Context, *PctChildToolScope) {
	if agentcall.CallOptionsFrom(ctx) == nil {
		return ctx, nil
	}
	parent := childCallBuildScope(ctx)
	if parent == nil {
		return ctx, nil
	}
	scope := newChildToolScope()
	opts := *agentcall.CallOptionsFrom(ctx)
	opts.BuildScope = scope.scope
	parent.Register(scope.provisionalRelease())
	ctx = agentcall.WithCallOptions(ctx, &opts)
	return ctx, scope
}

// childCallBuildScope is the agentcall read side of the parent's build scope
// (CallOptionsFrom + EnsureCallBuildScope), mirroring package agent's
// callBuildScopeFrom.
func childCallBuildScope(ctx context.Context) *agentcall.BuildResourceScope {
	if opts := agentcall.CallOptionsFrom(ctx); opts != nil {
		return agentcall.EnsureCallBuildScope(opts)
	}
	return nil
}

// capturedRemover is the narrow captured-tool capability the handoff needs:
// dropping one key from the captured tool's registration map.
type capturedRemover interface {
	RemoveCapturedHandoff(key string, h *PctDelegationHandoff)
}

// CapturedClaimer is the narrow captured-tool capability the executor-side
// lookup needs: claiming one registered handoff by invocation key.
type CapturedClaimer interface {
	ClaimCapturedHandoff(key string) *PctDelegationHandoff
}

// CapturedHandoffResolver resolves the batch-inst-only leaf lookup (call
// ingress registry resolve) to the captured claimer; it is invoked exactly
// once, lazily, by ClaimCapturedHandoff. Any error or nil claimer resolves to
// no handoff, matching the original straight-line nil returns.
type capturedHandoffResolver func() (CapturedClaimer, error)

// ClaimCapturedHandoff claims the captured outer handoff for one batch
// executor invocation.
func ClaimCapturedHandoff(resolve capturedHandoffResolver, sessionID, callID string) *PctDelegationHandoff {
	captured, err := resolve()
	if err != nil {
		return nil
	}
	if captured == nil {
		return nil
	}
	h := captured.ClaimCapturedHandoff(invocationKey(sessionID, callID))
	return h
}

func invocationKey(sessionID, callID string) string { return sessionID + "\x00" + callID }

// capturedLeafUnwrap is the narrow credential-escrow capability: the root
// batchCredentialLeaf wrapper (batch_credentials_escrow.go stays root) hands
// its wrapped agent leaf to the captured walk. It is deliberately NOT the
// generic role introspection seam (policyLeafWrapped) so the captured walk
// only recognizes the exact prior wrapper set.
type capturedLeafUnwrap interface {
	UnwrapCapturedCredentialLeaf() fantasy.AgentTool
}

// capturedScoper is the narrow scope capability the captured walk exposes
// for pctCapturedScopeFromTool semantics.
type capturedScoper interface {
	CapturedScope() *PctChildToolScope
}

// CapturedToolFrom is the moved pctCapturedToolFrom walk: exact prior
// recognized wrapper forms only (role guard, restricted, hooked, logged,
// root credential escrow leaf, captured tool itself). The root
// *pctCapturedAgentTool is recognized structurally through CapturedClaimer;
// unknown wrappers resolve to no captured tool.
func CapturedToolFrom(tool fantasy.AgentTool) CapturedClaimer {
	for tool != nil {
		switch wrapped := tool.(type) {
		case roleGuardTool:
			tool = wrapped.AgentTool
		case *roleGuardTool:
			tool = wrapped.AgentTool
		case CapturedClaimer:
			return wrapped
		case restrictedTool:
			tool = wrapped.AgentTool
		case *restrictedTool:
			tool = wrapped.AgentTool
		case *hookedTool:
			tool = wrapped.inner
		case *loggedTool:
			tool = wrapped.inner
		case capturedLeafUnwrap:
			// pct: BatchLeafRegistryStore.Register wraps every agent leaf.
			tool = wrapped.UnwrapCapturedCredentialLeaf()
		default:
			return nil
		}
	}
	return nil
}

// CapturedScopeFromTool is the moved pctCapturedScopeFromTool walk.
func CapturedScopeFromTool(tool fantasy.AgentTool) *PctChildToolScope {
	if t := CapturedToolFrom(tool); t != nil {
		if s, ok := t.(capturedScoper); ok {
			return s.CapturedScope()
		}
	}
	return nil
}

// DriverDisposer is the moved pctDriverDisposer: a session agent whose idle
// CancelAll releases the driver's retained scope references.
type driverDisposer struct {
	agentcall.SessionAgent
	releases []func()
}

func NewDriverDisposer(agent agentcall.SessionAgent, releases []func()) *driverDisposer {
	return &driverDisposer{SessionAgent: agent, releases: releases}
}

func (d *driverDisposer) CancelAll() bool {
	busy := d.SessionAgent.CancelAll()
	if !busy {
		ReleaseScopeRefs(d.releases)
	}
	return busy
}

func ReleaseScopeRefs(releases []func()) {
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

type PctDelegationHandoff struct {
	mu       sync.Mutex
	releases []func()
	pctChild *PctChildToolScope
	captured capturedRemover
	key      string
	claimed  bool
	released bool
	accepted bool
	once     sync.Once
}

func newDelegationHandoff(releases []func()) *PctDelegationHandoff {
	return &PctDelegationHandoff{releases: releases}
}

// BindCaptured registers the captured tool and invocation key while the
// caller holds its own map lock (the original pctRegister critical section:
// the caller's map mutex is acquired before the handoff mutex, and this
// binding refuses already-released handoffs).
func (h *PctDelegationHandoff) BindCaptured(captured capturedRemover, key string) bool {
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return false
	}
	h.captured, h.key = captured, key
	h.mu.Unlock()
	return true
}

func (h *PctDelegationHandoff) Accepted() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.accepted = true
	h.mu.Unlock()
}

func (h *PctDelegationHandoff) ClaimExecution() bool {
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

func (h *PctDelegationHandoff) ClaimDriver() []func() {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || h.pctChild == nil {
		return nil
	}
	release := h.pctChild.Retain()
	if release == nil {
		return nil
	}
	return []func(){release}
}

func (h *PctDelegationHandoff) ReleasePending() {
	h.releasePendingState(false)
}

// HasReleases is the lock-free releases count: the slice is immutable after
// newDelegationHandoff, so the count can be read without the handoff mutex.
func (h *PctDelegationHandoff) HasReleases() int {
	return len(h.releases)
}

func (h *PctDelegationHandoff) ReleaseUnstarted() {
	h.releasePendingState(true)
}

func (h *PctDelegationHandoff) releasePendingState(includeAccepted bool) {
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
		captured.RemoveCapturedHandoff(key, h)
	}
	h.once.Do(func() { ReleaseScopeRefs(h.releases) })
}

func (h *PctDelegationHandoff) Release() {
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
		captured.RemoveCapturedHandoff(key, h)
	}
	h.once.Do(func() { ReleaseScopeRefs(h.releases) })
}

func WithDelegationHandoff(ctx context.Context, h *PctDelegationHandoff) context.Context {
	return context.WithValue(ctx, delegationHandoffKey{}, h)
}

func HandoffFrom(ctx context.Context) *PctDelegationHandoff {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(delegationHandoffKey{}).(*PctDelegationHandoff)
	return h
}
