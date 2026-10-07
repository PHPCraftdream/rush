package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check doc: each test names the production line it must catch.
//
//   - TestStallDetachPanicCompletesJob guards the entry.complete call in the
//     recover branch of the detach goroutine in stallDetacherTool.Run: without
//     it a panicking detached tool reports "still running" forever.
//   - TestStallDetachOutputServesOnceThenDeletes guards the deleteStallDetachJob
//     call in StallDetachOutput's done branch for a job finished WITHOUT an
//     explicit job_kill (kill-requested jobs keep their final output readable,
//     see TestTurnStallDetachReturnsImmediateJobResult): without the delete the
//     entry would leak after serving the final output.
//   - TestStallDetachKillFinishedDeletes guards the deleteStallDetachJob call in
//     StallDetachKill's already-finished branch.
//   - TestStallDetachCleanupSession guards stallDetachCleanupSession's pending
//     and finished-job deletion (and still-running retention).

// panicTool blocks until released, then panics inside Run.
type panicTool struct {
	release chan struct{}
}

func (p panicTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "panic_tool", Description: "test"}
}

func (p panicTool) ProviderOptions() fantasy.ProviderOptions { return nil }

func (p panicTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (p panicTool) RestrictedRunAction() string { return "" }

func (p panicTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	select {
	case <-p.release:
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}
	panic("boom in panic_tool")
}

func TestStallDetachPanicCompletesJob(t *testing.T) {
	const sessionID, callID = "det-cleanup-panic-1", "call-p1"
	t.Cleanup(func() {
		stallDetachPending.Delete(stallDetachKey(sessionID, callID))
		stallDetachJobs.Delete(stallDetachKey(sessionID, callID))
	})

	tool := &stallDetacherTool{inner: panicTool{release: make(chan struct{})}}
	runCtx, cancel := context.WithCancel(stallSessionCtx(context.Background(), sessionID))
	defer cancel()

	go func() {
		tool.Run(runCtx, fantasy.ToolCall{ID: callID, Name: "panic_tool", Input: `{}`})
	}()

	// The wrapper registers the pending entry before its goroutine starts;
	// wait for it so the policy fire cannot race the registration.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := stallDetachPending.Load(stallDetachKey(sessionID, callID)); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending detach entry was never registered")
		}
		time.Sleep(time.Millisecond)
	}

	jobID := stallToolStalled(sessionID, callID, time.Second)
	if jobID == "" {
		t.Fatal("stallToolStalled did not detach the pending call")
	}

	// The wrapper's select moves the entry pending->jobs asynchronously;
	// wait for the job to become visible before polling its output.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached job was never registered")
		}
		time.Sleep(time.Millisecond)
	}

	// First output must show the job still running.
	_, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
	if !ok || done {
		t.Fatalf("job_output ok=%v done=%v, want true/false while running", ok, done)
	}

	close(tool.inner.(panicTool).release)
	// Poll the entry's done flag directly: the single done-serve must be
	// left intact for the job_output assertions below.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
			v.(*stallDetachEntry).mu.Lock()
			completed := v.(*stallDetachEntry).done
			v.(*stallDetachEntry).mu.Unlock()
			if completed {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("panicked detached job never completed")
		}
		time.Sleep(time.Millisecond)
	}

	text, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
	if !ok || !done {
		t.Fatalf("job_output after panic ok=%v done=%v, want true/true", ok, done)
	}
	if !strings.Contains(text, "panicked") {
		t.Fatalf("final output %q missing \"panicked\"", text)
	}
}

func TestStallDetachOutputServesOnceThenDeletes(t *testing.T) {
	const sessionID, callID = "det-cleanup-output-2", "call-o2"
	t.Cleanup(func() { stallDetachJobs.Delete(stallDetachKey(sessionID, callID)) })

	e := &stallDetachEntry{sessionID: sessionID, callID: callID, done: true, content: "final output here"}
	stallDetachJobs.Store(stallDetachKey(sessionID, callID), e)

	out, done, total, ok := stallDetachJobController{}.StallDetachOutput(sessionID, "stall-"+callID, 0)
	if !ok || !done || total != int64(len(e.content)) {
		t.Fatalf("first output ok=%v done=%v total=%d", ok, done, total)
	}
	if out == "" || !strings.Contains(out, "final output") {
		t.Fatalf("first output = %q", out)
	}
	if _, _, _, ok2 := (stallDetachJobController{}).StallDetachOutput(sessionID, "stall-"+callID, 0); ok2 {
		t.Fatal("entry not deleted after the final output was served once")
	}
}

func TestStallDetachKillFinishedDeletes(t *testing.T) {
	const sessionID, callID = "det-cleanup-kill-3", "call-k3"
	t.Cleanup(func() { stallDetachJobs.Delete(stallDetachKey(sessionID, callID)) })

	e := &stallDetachEntry{sessionID: sessionID, callID: callID, done: true, content: "finished"}
	stallDetachJobs.Store(stallDetachKey(sessionID, callID), e)

	text, ok := stallDetachJobController{}.StallDetachKill(sessionID, "stall-"+callID)
	if !ok || !strings.Contains(text, "already finished") {
		t.Fatalf("kill text %q ok=%v", text, ok)
	}
	if _, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
		t.Fatal("finished entry not deleted by StallDetachKill")
	}
}

func TestStallDetachCleanupSession(t *testing.T) {
	const sessionID = "det-cleanup-session-4"
	finishedKey := stallDetachKey(sessionID, "call-s4a")
	runningKey := stallDetachKey(sessionID, "call-s4b")
	pendingKey := stallDetachKey(sessionID, "call-s4c")
	otherKey := stallDetachKey("det-cleanup-session-4-other", "call-x")
	t.Cleanup(func() {
		stallDetachJobs.Delete(finishedKey)
		stallDetachJobs.Delete(runningKey)
		stallDetachJobs.Delete(otherKey)
		stallDetachPending.Delete(pendingKey)
	})

	stallDetachJobs.Store(finishedKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4a", done: true, content: "done"})
	stallDetachJobs.Store(runningKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4b"})
	stallDetachJobs.Store(otherKey, &stallDetachEntry{sessionID: "det-cleanup-session-4-other", callID: "call-x", done: true})
	stallDetachPending.Store(pendingKey, &stallDetachEntry{sessionID: sessionID, callID: "call-s4c"})

	stallDetachCleanupSession(sessionID)

	if _, ok := stallDetachPending.Load(pendingKey); ok {
		t.Fatal("pending entry survived stallDetachCleanupSession")
	}
	if _, ok := stallDetachJobs.Load(finishedKey); ok {
		t.Fatal("finished detached job survived stallDetachCleanupSession")
	}
	if _, ok := stallDetachJobs.Load(runningKey); !ok {
		t.Fatal("still-running detached job was dropped by stallDetachCleanupSession")
	}
	if _, ok := stallDetachJobs.Load(otherKey); !ok {
		t.Fatal("another session's finished job was dropped by stallDetachCleanupSession")
	}
}

// TestRunInternal_CleansFinishedDetachedJobs: a finished detached job of
// the session is dropped when its run ends.
// Revert check: deleting `defer stallDetachCleanupSession(sessionID)` in
// runInternal.
func TestRunInternal_CleansFinishedDetachedJobs(t *testing.T) {
	var key string
	_, _, _, err := rateLimitLoopRun(t, t.Context(), "test-detach-cleanup", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			key = stallDetachKey(sess.ID, "call-done")
			stallDetachJobs.Store(key, &stallDetachEntry{sessionID: sess.ID, callID: "call-done", done: true})
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	_, ok := stallDetachJobs.Load(key)
	require.False(t, ok, "finished detached job must be dropped at run end")
}
