package agent

import (
	"sync"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

// The delegation-handoff and child-scope plumbing moved to the toolwrap
// policy leaf (internal/agent/runtime/policy/toolwrap); this file keeps only
// the batchinst-targeted captured agent tool (type/fields/receiver methods),
// the exact captured-wrapper traversal over the recognized package-agent
// wrappers, and the driver disposer.
type pctCapturedAgentTool struct {
	fantasy.AgentTool
	scope    *toolwrap.PctChildToolScope
	mu       sync.Mutex
	handoffs map[string]*toolwrap.PctDelegationHandoff
}

// pctRegister keeps the original mutex order: the captured map lock is held
// across the handoff binding (handoff mutex inside captured mutex).
func (t *pctCapturedAgentTool) pctRegister(key string, h *toolwrap.PctDelegationHandoff) bool {
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

func (t *pctCapturedAgentTool) pctRemove(key string, expected *toolwrap.PctDelegationHandoff) {
	t.mu.Lock()
	if t.handoffs[key] == expected {
		delete(t.handoffs, key)
	}
	t.mu.Unlock()
}

// RegisterCapturedHandoff is the captured-registration capability the moved
// toolwrap ingress calls; it delegates exactly to pctRegister.
func (t *pctCapturedAgentTool) RegisterCapturedHandoff(key string, h *toolwrap.PctDelegationHandoff) bool {
	return t.pctRegister(key, h)
}

func (t *pctCapturedAgentTool) pctClaim(key string) *toolwrap.PctDelegationHandoff {
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
func (t *pctCapturedAgentTool) RemoveCapturedHandoff(key string, h *toolwrap.PctDelegationHandoff) {
	t.pctRemove(key, h)
}

// ClaimCapturedHandoff is the narrow captured capability the executor-side
// lookup (toolwrap.CapturedClaimer) calls once per invocation.
func (t *pctCapturedAgentTool) ClaimCapturedHandoff(key string) *toolwrap.PctDelegationHandoff {
	return t.pctClaim(key)
}

// CapturedScope is the narrow scope capability behind the moved
// toolwrap.CapturedScopeFromTool walk.
func (t *pctCapturedAgentTool) CapturedScope() *toolwrap.PctChildToolScope {
	return t.scope
}

// PolicyLeafTool is the production role-introspection seam
// (toolwrap.policyLeafWrapped): role contract traversal descends through the
// captured wrapper without copying it.
func (t *pctCapturedAgentTool) PolicyLeafTool() fantasy.AgentTool {
	return t.AgentTool
}

// pctCapturedAgentToolFromTool is the NEW root shim (non-manifest name) over
// the moved toolwrap.CapturedToolFrom walk: it converts the returned captured
// capability back to the root concrete type for the staying root callers
// (executor resolver and root integration tests).
func pctCapturedAgentToolFromTool(tool fantasy.AgentTool) *pctCapturedAgentTool {
	if c := toolwrap.CapturedToolFrom(tool); c != nil {
		if t, ok := c.(*pctCapturedAgentTool); ok {
			return t
		}
	}
	return nil
}
