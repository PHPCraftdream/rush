package permission

import "slices"

// SessionPolicyManager arms a run-scoped synchronous permission policy.
type SessionPolicyManager interface {
	SetSessionReadonly(sessionID, ownerToken string)
	SetSessionAuto(sessionID, ownerToken string)
	ReleaseSessionPolicy(sessionID, ownerToken string)
	SessionPolicyDecision(sessionID, toolName, action string) (active, allowed bool)
	InheritSessionPolicy(parentID, childID string)
}

type sessionPolicyMode uint8

const (
	sessionPolicyReadonly sessionPolicyMode = iota + 1
	sessionPolicyAuto
)

type sessionPolicyEntry struct {
	owner string
	mode  sessionPolicyMode
}

var ReadonlySessionTools = []string{
	"fs_find",
	"fs_grep",
	"fs_list",
	"fs_read",
	"git_read",
	"glob",
	"grep",
	"ls",
	"rush_info",
	"rush_logs",
	"sourcegraph",
	"view",
}

func (s *permissionService) SetSessionReadonly(sessionID, ownerToken string) {
	s.setSessionPolicy(sessionID, ownerToken, sessionPolicyReadonly)
}

func (s *permissionService) SetSessionAuto(sessionID, ownerToken string) {
	s.setSessionPolicy(sessionID, ownerToken, sessionPolicyAuto)
}

func (s *permissionService) setSessionPolicy(sessionID, ownerToken string, mode sessionPolicyMode) {
	if sessionID == "" || ownerToken == "" {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	state.initLocked()
	state.policies[sessionID] = sessionPolicyEntry{owner: ownerToken, mode: mode}
	state.policyRoots[ownerToken] = sessionID
	state.mu.Unlock()
}

func (s *permissionService) ReleaseSessionPolicy(sessionID, ownerToken string) {
	if sessionID == "" || ownerToken == "" {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	if state.policyRoots[ownerToken] == sessionID {
		for id, entry := range state.policies {
			if entry.owner == ownerToken {
				delete(state.policies, id)
			}
		}
		delete(state.policyRoots, ownerToken)
	} else if entry, ok := state.policies[sessionID]; ok && entry.owner == ownerToken {
		delete(state.policies, sessionID)
	}
	state.mu.Unlock()
}

func (s *permissionService) SessionPolicyDecision(sessionID, toolName, action string) (active, allowed bool) {
	state := &s.sessionDenies
	state.mu.RLock()
	entry, active := state.policies[sessionID]
	state.mu.RUnlock()
	if !active {
		return false, false
	}
	if entry.mode == sessionPolicyAuto {
		return true, true
	}
	return true, slices.Contains(ReadonlySessionTools, toolName) && actionReadOnly(action)
}

func actionReadOnly(action string) bool {
	switch action {
	case "", "read", "list":
		return true
	default:
		return false
	}
}

func (s *permissionService) InheritSessionPolicy(parentID, childID string) {
	if parentID == "" || childID == "" || parentID == childID {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	if entry, ok := state.policies[parentID]; ok {
		state.initLocked()
		state.policies[childID] = entry
	}
	state.mu.Unlock()
}

var _ SessionPolicyManager = (*permissionService)(nil)
