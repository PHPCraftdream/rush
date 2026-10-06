package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// Each test names the single production behavior it must catch (revert-check
// doc):
//
//   - TestTurnStallDetachReturnsImmediateJobResult guards the chunk-3
//     detach: a stalled synchronous tool's wrapper Run returns the
//     "still running ... job <id>" result (so fantasy's sequential
//     executeTools proceeds), the tool is NOT cancelled by the detach, and
//     the controller serves job_output output and job_kill on that id.
//     Full fantasy/ledger end-to-end integration is not exercised here —
//     the unit test asserts the result text, non-cancellation and the
//     job-id plumbing through the same controller job_output/job_kill call.
//   - TestTurnStallDetachExcludesDelegationAndBackground guards the
//     exclusion set: delegation (agent/agentic_fetch) and
//     background/async (bash/run_command) kinds run inline with no
//     goroutine, no detach and no cancel.
//   - TestTurnStallPolicyFiresEveryCrossing guards the PART-1 fix: the
//     policy seam is invoked on CONSECUTIVE threshold crossings inside the
//     30-minute warn window (only the warn+dump is rate-limited).

func stallSessionCtx(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, tools.SessionIDContextKey, sessionID)
}

func TestTurnStallDetachReturnsImmediateJobResult(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)

	var (
		started = make(chan struct{})
		mu      sync.Mutex
		killed  bool
	)
	inner := fantasy.NewAgentTool("slow_tool", "test", func(ctx context.Context, _ struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		<-ctx.Done() // block until job_kill cancels the run
		mu.Lock()
		killed = true
		mu.Unlock()
		return fantasy.NewTextResponse("partial output from slow_tool"), nil
	})
	tool := &stallDetacherTool{inner: inner}

	sessionID, callID := "sess-detach", "call-detach-1"
	runCtx, cancel := context.WithCancel(stallSessionCtx(context.Background(), sessionID))
	defer cancel()
	type runResult struct {
		resp fantasy.ToolResponse
		err  error
	}
	results := make(chan runResult, 1)
	go func() {
		resp, err := tool.Run(runCtx, fantasy.ToolCall{ID: callID, Name: "slow_tool", Input: `{"target":"db"}`})
		results <- runResult{resp, err}
	}()
	<-started

	// The stall policy fires for this phase: the call detaches and the
	// wrapper returns immediately with the job-id notice.
	jobID := stallToolStalled(sessionID, callID, 15*time.Minute)
	if jobID == "" {
		t.Fatal("stallToolStalled did not detach the pending synchronous call")
	}
	if jobID != "stall-"+callID {
		t.Fatalf("job id = %q, want stall-%s", jobID, callID)
	}
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("detach returned an error: %v", r.err)
		}
		for _, want := range []string{"slow_tool", "still running", jobID, "job_output", "job_kill"} {
			if !strings.Contains(r.resp.Content, want) {
				t.Fatalf("result text %q missing %q", r.resp.Content, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detach did not return an immediate tool result")
	}

	// The detach itself must NOT cancel the tool: it is still running.
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	wasKilled := killed
	mu.Unlock()
	if wasKilled {
		t.Fatal("detach cancelled the still-running tool")
	}

	// job_output sees a running job (controller is the one the tool calls).
	text, done, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
	if !ok || done {
		t.Fatalf("job_output ok=%v done=%v, want true/false on a live job", ok, done)
	}
	if text == "" {
		t.Fatal("job_output returned empty text for a live detached job")
	}

	// job_kill stops it; the final output stays readable via job_output.
	killText, kok := stallDetachJobController{}.StallDetachKill(sessionID, jobID)
	if !kok {
		t.Fatal("job_kill did not claim the detached job id")
	}
	if killText == "" {
		t.Fatal("job_kill returned empty text")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, d, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
		if ok && d {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job_kill did not stop the detached job")
		}
		time.Sleep(5 * time.Millisecond)
	}
	final, d, _, ok := stallDetachJobController{}.StallDetachOutput(sessionID, jobID, 0)
	if !ok || !d || !strings.Contains(final, "partial output from slow_tool") {
		t.Fatalf("job_output after kill = %q done=%v ok=%v", final, d, ok)
	}

	// A second policy fire for a detached (or absent) call must no-op.
	if again := stallToolStalled(sessionID, callID, 16*time.Minute); again != "" {
		t.Fatalf("re-detach returned %q, want empty", again)
	}
}

func TestTurnStallDetachExcludesDelegationAndBackground(t *testing.T) {
	for _, name := range []string{AgentToolName, tools.AgenticFetchToolName, tools.BashToolName, tools.RunCommandToolName} {
		inner := fantasy.NewAgentTool(name, "test", func(ctx context.Context, _ struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			// Excluded kinds run INLINE: the flag proves no wrapper
			// goroutine is between us and the inner Run.
			return fantasy.NewTextResponse("inline:" + name), nil
		})
		tool := &stallDetacherTool{inner: inner}
		sessionID, callID := "sess-excl", "call-"+name
		resp, err := tool.Run(stallSessionCtx(context.Background(), sessionID), fantasy.ToolCall{ID: callID, Name: name, Input: `{}`})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.Content != "inline:"+name {
			t.Fatalf("%s: content = %q, want inline run", name, resp.Content)
		}
		if _, pending := stallDetachPending.Load(stallDetachKey(sessionID, callID)); pending {
			t.Fatalf("%s: registered a pending detach entry", name)
		}
		if jobID := stallToolStalled(sessionID, callID, time.Hour); jobID != "" {
			t.Fatalf("%s: stallToolStalled returned %q, want empty", name, jobID)
		}
	}
}

func TestTurnStallPolicyFiresEveryCrossing(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	sc := &stallClock{abortTimeout: time.Minute}
	sc.markProgress()
	sc.setPhase("tool", "slow_tool", "call-x")
	sessionID := "sess-crossing"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	var fires atomic.Int64
	prev, _ := turnStallPolicy.Load().(func(string, turnPhase, time.Duration, time.Duration))
	t.Cleanup(func() {
		turnStallPolicy.Store(prev)
	})
	SetTurnStallPolicy(func(sid string, phase turnPhase, sp, sb time.Duration) {
		if phase.kind != "tool" || phase.callID != "call-x" {
			t.Errorf("policy phase = %s/%s, want tool/call-x", phase.kind, phase.callID)
		}
		fires.Add(1)
	})

	// Two consecutive crossings INSIDE the 30-min warn window must both
	// reach the policy (only warn+dump is rate-limited).
	set(t0.Add(11 * time.Minute))
	stallSamplerTickOnce()
	firstWarn := sc.lastWarnNanos.Load()
	set(t0.Add(11*time.Minute + 5*time.Second))
	stallSamplerTickOnce()
	if fires.Load() != 2 {
		t.Fatalf("policy fires = %d, want 2 inside the warn window", fires.Load())
	}
	if sc.lastWarnNanos.Load() != firstWarn {
		t.Fatal("warn+dump was not rate-limited inside the window")
	}
	// Outside the window the warn fires again alongside the policy.
	set(t0.Add(45 * time.Minute))
	stallSamplerTickOnce()
	if fires.Load() != 3 {
		t.Fatalf("policy fires = %d, want 3", fires.Load())
	}
	if sc.lastWarnNanos.Load() == firstWarn {
		t.Fatal("warn did not fire after the window opened")
	}
}
