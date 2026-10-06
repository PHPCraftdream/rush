package agent

// REVERT-CHECKS:
//   - TestTurnStallToolDetachesAtWarnThreshold: gating the tool branch of
//     turnStallAbortPolicy behind abortTimeout again (no detach with 0).
//   - TestTurnStallProviderRetriesAreSpaced: dropping the
//     lastStallRetryNanos window (retries 15 s apart, abort in ~45 s).

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestTurnStallToolDetachesAtWarnThreshold(t *testing.T) {
	prev := turnStallPolicy.Load()
	installTurnStallPolicy()
	t.Cleanup(func() { turnStallPolicy.Store(prev) })
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)

	started := make(chan struct{})
	inner := fantasy.NewAgentTool("hung_glob", "test", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(started)
		<-ctx.Done()
		return fantasy.NewTextResponse("late"), nil
	})
	tool := &stallDetacherTool{inner: inner}
	sessionID, callID := "sess-warn-detach", "call-warn-1"
	ctx, cancel := context.WithCancel(stallSessionCtx(context.Background(), sessionID))
	t.Cleanup(cancel)
	got := make(chan string, 1)
	go func() {
		resp, _ := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: "hung_glob", Input: `{}`})
		got <- resp.Content
	}()
	<-started

	// Abort disabled (--stall-timeout 0): the detach must still wake the model.
	sc := &stallClock{abortTimeout: 0}
	sc.markProgress()
	sc.setPhase("tool", "hung_glob", callID)
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() {
		armedStallClocks.Delete(sessionID)
		if v, ok := stallDetachJobs.Load(stallDetachKey(sessionID, callID)); ok {
			v.(*stallDetachEntry).cancel()
		}
	})
	set(t0.Add(streamIdleTimeoutDefault + time.Second))
	stallSamplerTickOnce()

	select {
	case text := <-got:
		require.True(t, strings.Contains(text, "still running"), text)
	case <-time.After(5 * time.Second):
		t.Fatal("a tool stalled past the warn threshold must be detached even with the abort off")
	}
}

func TestTurnStallProviderRetriesAreSpaced(t *testing.T) {
	sc, _, aborts, set := newStallPolicyClock(t, 5*time.Minute)
	sc.setPhase("provider", "test-model", "")
	t0 := time.Now()

	fireStallSampler(t, set, t0, 11*time.Minute)
	require.Equal(t, int32(1), sc.stallRetries.Load())

	// The next sampler tick 15 s later must leave the re-issued request alone.
	fireStallSampler(t, set, t0, 11*time.Minute+stallSamplerTick)
	require.Equal(t, int32(1), sc.stallRetries.Load(), "a retry needs a full warn window before the next one")
	require.Empty(t, *aborts)

	fireStallSampler(t, set, t0, 11*time.Minute+streamIdleTimeoutDefault+stallSamplerTick)
	require.Equal(t, int32(2), sc.stallRetries.Load())
	require.Empty(t, *aborts)
}
