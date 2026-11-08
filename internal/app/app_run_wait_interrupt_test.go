package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRunLoopInterruptWhileWaitingForLiveChild(t *testing.T) {
	for _, duringWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("during_wait=%v", duringWait), func(t *testing.T) {
			runLoopInterruptWhileChildLive(t, duringWait)
		})
	}
}

func runLoopInterruptWhileChildLive(t *testing.T, duringWait bool) {
	t.Helper()
	const marker = "OPERATOR_INTERRUPT_WHILE_CHILD_HELD"
	var mu sync.Mutex
	var events []string
	started := time.Now()
	record := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(events) == 64 {
			events = events[1:]
		}
		events = append(events, fmt.Sprintf("+%s %s", time.Since(started), fmt.Sprintf(format, args...)))
	}
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			defer mu.Unlock()
			t.Log("wait-interrupt timeline:\n" + strings.Join(events, "\n"))
		}
	})
	var handled atomic.Int32
	var childStillLive atomic.Bool
	var copies atomic.Int32
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		record("provider request=%d interrupt=%v", n, strings.Contains(string(body), marker))
		if n == 1 {
			admissionWriteSSE(w, []string{admissionSSEToolCall("wait", "call-wait", "await_tasks", `{"until":"all"}`), admissionSSEStop("wait", "tool_calls")})
			return
		}
		if strings.Contains(string(body), marker) {
			handled.Add(1)
			copies.Store(int32(strings.Count(string(body), marker)))
			childStillLive.Store(delegationRunning(t, h.app, h.sessionID, "held-child"))
			loopText(w, "answer", "operator resumed", 11, 3)
			completeDelegation(t, t.Context(), h.app, h.sessionID, "held-child", "child released by fixture", false)
			record("operator turn served while child live=%v", childStillLive.Load())
			return
		}
		loopText(w, "unexpected", "unexpected turn", 11, 3)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	child, err := h.app.Sessions.Create(ctx, "held child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "held-child", child.ID)
	inject := func() {
		record("between turns; child still live=%v", delegationRunning(t, h.app, h.sessionID, "held-child"))
		msg, err := h.app.Messages.Create(ctx, h.sessionID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: marker}}})
		require.NoError(t, err)
		require.NoError(t, h.app.Sessions.CreatePendingInject(ctx, session.PendingInject{SessionID: h.sessionID, MessageID: msg.ID, Content: marker, Interrupt: true}))
		record("external interrupt persisted: message=%s", msg.ID)
	}
	if duringWait {
		var once sync.Once
		cliLoopInterruptWaitSeam = func() { once.Do(inject) }
		t.Cleanup(func() { cliLoopInterruptWaitSeam = nil })
	} else {
		h.afterFirstTurn(inject)
	}
	result, _, err := h.run(ctx, RunOverrides{})
	record("CLI returned: err=%v handled=%d", err, handled.Load())
	require.NoError(t, err)
	require.EqualValues(t, 1, handled.Load(), "interrupt must execute once, without waiting for child completion")
	require.True(t, childStillLive.Load(), "operator turn must not require child settlement")
	require.EqualValues(t, 1, copies.Load(), "persisted user message must enter the provider prompt once")
	pending, err := h.app.Sessions.PeekInterruptInject(t.Context(), h.sessionID)
	require.NoError(t, err)
	require.Nil(t, pending)
	outstanding, err := h.app.Sessions.HasOutstandingRunQueueEntriesForSession(t.Context(), h.sessionID)
	require.NoError(t, err)
	require.False(t, outstanding)
	require.NotNil(t, result)
	require.Contains(t, result.FinalText, "operator resumed", "CLI must not return the stale pre-interrupt answer")
}
