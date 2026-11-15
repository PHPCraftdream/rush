package permission

import "sync"

// SessionDenyManager is an optional session-scoped denial control.
type SessionDenyManager interface {
	DenySession(sessionID, ownerToken string)
	ReleaseSessionDeny(sessionID, ownerToken string)
	SessionDenied(sessionID string) bool
}

type sessionDenyState struct {
	mu          sync.RWMutex
	bySession   map[string]string
	rootByToken map[string]string
	policies    map[string]sessionPolicyEntry
	policyRoots map[string]string
}

func (s *permissionService) DenySession(sessionID, ownerToken string) {
	if sessionID == "" || ownerToken == "" {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	state.initLocked()
	state.bySession[sessionID] = ownerToken
	state.rootByToken[ownerToken] = sessionID
	state.mu.Unlock()
}

func (s *permissionService) ReleaseSessionDeny(sessionID, ownerToken string) {
	if sessionID == "" || ownerToken == "" {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	if state.rootByToken[ownerToken] == sessionID {
		for id, token := range state.bySession {
			if token == ownerToken {
				delete(state.bySession, id)
			}
		}
		delete(state.rootByToken, ownerToken)
	} else if state.bySession[sessionID] == ownerToken {
		delete(state.bySession, sessionID)
	}
	state.mu.Unlock()
}

func (s *permissionService) SessionDenied(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	state := &s.sessionDenies
	state.mu.RLock()
	_, denied := state.bySession[sessionID]
	state.mu.RUnlock()
	return denied
}

// initLocked allocates the maps; reading a nil map is safe, so only writers call it.
func (st *sessionDenyState) initLocked() {
	if st.bySession == nil {
		st.bySession = make(map[string]string)
	}
	if st.rootByToken == nil {
		st.rootByToken = make(map[string]string)
	}
	if st.policies == nil {
		st.policies = make(map[string]sessionPolicyEntry)
	}
	if st.policyRoots == nil {
		st.policyRoots = make(map[string]string)
	}
}

func (s *permissionService) inheritSessionDeny(parentID, childID string) {
	if parentID == "" || childID == "" || parentID == childID {
		return
	}
	state := &s.sessionDenies
	state.mu.Lock()
	if policy, ok := state.policies[parentID]; ok {
		state.initLocked()
		state.policies[childID] = policy
	}
	if token, ok := state.bySession[parentID]; ok {
		state.initLocked()
		state.bySession[childID] = token
	}
	state.mu.Unlock()
}
