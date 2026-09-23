// Durable-drain assistant identity reconciliation helpers, split out of
// app_run_reviewer.go (2026-09-23, file-size ratchet) as a pure move -- no
// behavior change, same package.

package app

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
)

type drainCompletion struct {
	result              session.DrainResult
	err                 error
	recorder            *agent.CallResultRecorder
	originalOwner       *agent.CallResultRecorder
	assistantIDs        map[string]struct{}
	terminalAssistantID string
}

func cloneAssistantIDSet(ids map[string]struct{}) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	clone := make(map[string]struct{}, len(ids))
	for id := range ids {
		clone[id] = struct{}{}
	}
	return clone
}

func recordDrainAssistantIDs(
	mu *sync.Mutex,
	recorder *agent.CallResultRecorder,
	confirmed map[string]struct{},
	baseline map[string]struct{},
	ids map[string]struct{},
	terminalID string,
) string {
	mu.Lock()
	defer mu.Unlock()
	for id := range ids {
		if id == "" {
			continue
		}
		if _, existed := baseline[id]; existed {
			continue
		}
		confirmed[id] = struct{}{}
		recorder.RecordConfirmed(id)
	}
	if terminalID == "" {
		return ""
	}
	if _, exists := ids[terminalID]; !exists {
		return ""
	}
	if _, existed := baseline[terminalID]; existed {
		return ""
	}
	if _, confirmedTerminal := confirmed[terminalID]; !confirmedTerminal {
		return ""
	}
	recorder.RecordConfirmed(terminalID)
	return terminalID
}

func (s *executeRunLoop) adoptDrainIdentity(completion drainCompletion) bool {
	if completion.result != session.DrainComplete || completion.recorder == nil {
		s.eventOwner = completion.originalOwner
		return false
	}
	identity := completion.recorder.Resolve()
	if identity == "" || len(completion.assistantIDs) == 0 {
		s.eventOwner = completion.originalOwner
		return false
	}
	if _, confirmedTerminal := completion.assistantIDs[completion.terminalAssistantID]; !confirmedTerminal {
		s.eventOwner = completion.originalOwner
		return false
	}
	if identity != completion.terminalAssistantID || !completion.recorder.Owns(identity) {
		s.eventOwner = completion.originalOwner
		return false
	}
	recordedIDs := completion.recorder.IDs()
	if len(recordedIDs) != len(completion.assistantIDs) {
		s.eventOwner = completion.originalOwner
		return false
	}
	for id := range completion.assistantIDs {
		if id == "" || !completion.recorder.Owns(id) {
			s.eventOwner = completion.originalOwner
			return false
		}
	}
	for id := range recordedIDs {
		if _, confirmed := completion.assistantIDs[id]; !confirmed {
			s.eventOwner = completion.originalOwner
			return false
		}
	}
	s.callResultRec = completion.recorder
	s.eventOwner = completion.recorder
	return true
}
