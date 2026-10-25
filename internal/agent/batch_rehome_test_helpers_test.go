// Re-homed test helpers from the deleted legacy async-job test files
// (work_ledger_delegation_test.go, work_ledger_timeout_test.go,
// delegation_question.go): the batch-world tests still need these shapes.
// COMPILE-ONLY IN THIS WORKTREE.
package agent

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

// writeParkedChildTurn records a finished assistant turn on a child session,
// which is what refreshSubAgentCompletion reads at release time.
func writeParkedChildTurn(t *testing.T, env fakeEnv, sessionID, text string, reason message.FinishReason) {
	t.Helper()
	x2WriteParkedChildTurn(t, env.messages, sessionID, text, reason)
}

// busyStubAgent is a SessionAgent stub whose only meaningful behavior is a
// settable IsSessionBusy, used to drive childScopeDrained's negative busy
// gate.
type busyStubAgent struct {
	mockSessionAgent
	mu   sync.Mutex
	busy bool
}

func newBusyStubAgent() *busyStubAgent {
	return &busyStubAgent{}
}

func (s *busyStubAgent) IsSessionBusy(string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *busyStubAgent) setBusy(busy bool) {
	s.mu.Lock()
	s.busy = busy
	s.mu.Unlock()
}

// goroutineBaseline is runtime.NumGoroutine after a GC, for a delta check.
func goroutineBaseline() int {
	runtime.GC()
	return runtime.NumGoroutine()
}

// settledGoroutineDelta is the smallest goroutine delta over base seen within
// 2s. NumGoroutine is process-wide: a goroutine left by an earlier test exits
// within the window, a goroutine per armed item does not.
func settledGoroutineDelta(base int) int {
	best := runtime.NumGoroutine() - base
	for deadline := time.Now().Add(2 * time.Second); best > 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if d := runtime.NumGoroutine() - base; d < best {
			best = d
		}
	}
	return best
}
