// Re-homed test helpers from the deleted legacy async-job test files
// (work_ledger_delegation_test.go, work_ledger_timeout_test.go,
// delegation_question.go): the batch-world tests still need these shapes.
// COMPILE-ONLY IN THIS WORKTREE.
package agent

import (
	"fmt"
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

// childQuestionNoticeText builds the child_question notice body: the same
// "SUB-AGENT QUESTION (session ...)" frame the parent model already knows,
// followed by the paused-state paragraph with the exact answer and give-up
// calls. ownJobs is the child's own still-running job count; jobID is the
// held delegation X the answer's result will arrive with.
func childQuestionNoticeText(preamble, questionBlock, childID, jobID string, ownJobs int) string {
	frame := fmt.Sprintf("SUB-AGENT QUESTION (session %s): %s", childID, questionBlock)
	body := subAgentQuestionWithPreamble(preamble, frame)
	return fmt.Sprintf(
		"%s\n\nThe sub-agent is paused on this question. Its own %d background job(s) are still running, so job %s stays open -- waiting will not produce a result.\n"+
			"Answer: call `agent` with resume_session_id=%q and your answer as prompt; the result then arrives with job %s.\n"+
			"Give up: stop_agent(child_session_id=%q) -- this also stops its running jobs.",
		body, ownJobs, jobID, childID, jobID, childID,
	)
}
