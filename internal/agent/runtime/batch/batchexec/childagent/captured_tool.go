package childagent

import (
	"sync"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

// PctCapturedAgentTool binds a child tool scope to an agent tool without changing its policy leaf.
type PctCapturedAgentTool struct {
	fantasy.AgentTool
	scope    *toolwrap.PctChildToolScope
	mu       sync.Mutex
	handoffs map[string]*toolwrap.PctDelegationHandoff
}

// NewPctCapturedAgentTool wraps a child leaf with its build scope.
func NewPctCapturedAgentTool(tool fantasy.AgentTool, scope *toolwrap.PctChildToolScope) *PctCapturedAgentTool {
	return &PctCapturedAgentTool{AgentTool: tool, scope: scope}
}

// pctRegister keeps the original mutex order: the captured map lock is held
// across the handoff binding (handoff mutex inside captured mutex).
func (t *PctCapturedAgentTool) pctRegister(key string, h *toolwrap.PctDelegationHandoff) bool {
	t.mu.Lock()
	if t.handoffs == nil {
		t.handoffs = make(map[string]*toolwrap.PctDelegationHandoff)
	}
	if _, exists := t.handoffs[key]; exists {
		t.mu.Unlock()
		return false
	}
	if !h.BindCaptured(t, key) {
		t.mu.Unlock()
		return false
	}
	t.handoffs[key] = h
	t.mu.Unlock()
	return true
}

func (t *PctCapturedAgentTool) pctRemove(key string, expected *toolwrap.PctDelegationHandoff) {
	t.mu.Lock()
	if t.handoffs[key] == expected {
		delete(t.handoffs, key)
	}
	t.mu.Unlock()
}

// RegisterCapturedHandoff is the captured-registration capability the moved
// toolwrap ingress calls; it delegates exactly to pctRegister.
func (t *PctCapturedAgentTool) RegisterCapturedHandoff(key string, h *toolwrap.PctDelegationHandoff) bool {
	return t.pctRegister(key, h)
}

func (t *PctCapturedAgentTool) pctClaim(key string) *toolwrap.PctDelegationHandoff {
	t.mu.Lock()
	h := t.handoffs[key]
	if h != nil && !h.ClaimExecution() {
		h = nil
	}
	if h != nil {
		delete(t.handoffs, key)
	}
	t.mu.Unlock()
	return h
}

// RemoveCapturedHandoff is the narrow captured capability the moved handoff
// (toolwrap.capturedRemover) calls on release paths.
func (t *PctCapturedAgentTool) RemoveCapturedHandoff(key string, h *toolwrap.PctDelegationHandoff) {
	t.pctRemove(key, h)
}

// ClaimCapturedHandoff is the narrow captured capability the executor-side
// lookup (toolwrap.CapturedClaimer) calls once per invocation.
func (t *PctCapturedAgentTool) ClaimCapturedHandoff(key string) *toolwrap.PctDelegationHandoff {
	return t.pctClaim(key)
}

// CapturedScope is the narrow scope capability behind the moved
// toolwrap.CapturedScopeFromTool walk.
func (t *PctCapturedAgentTool) CapturedScope() *toolwrap.PctChildToolScope {
	return t.scope
}

// PolicyLeafTool is the production role-introspection seam
// (toolwrap.policyLeafWrapped): role contract traversal descends through the
// captured wrapper without copying it.
func (t *PctCapturedAgentTool) PolicyLeafTool() fantasy.AgentTool {
	return t.AgentTool
}
