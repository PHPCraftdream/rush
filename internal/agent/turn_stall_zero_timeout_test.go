package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// TestTurnStallReasoningDeltaHookLeavesProgressPinned guards
// agent_turn_stream.go's onReasoningDelta: it bumps lastByte
// (via bumpActivity) and the reasoningChars counter, but must never
// move lastProgress. Mixed-root test against the LOWER drainctl clock
// (the one production arms); the pure-root zero-timeout and sampler-race
// tests were retired with the legacy root stall subsystem.
func TestTurnStallReasoningDeltaHookLeavesProgressPinned(t *testing.T) {
	t0 := time.Now()
	set := drainctl.InstallFakeStallNow(t)
	set(t0)

	sc := drainctl.NewStallClockForTest(t, "sess-reason", &CallOptions{TurnStallTimeout: time.Minute})
	sc.MarkProgress()
	progressBefore, byteBefore, _ := drainctl.StallSnapshot(sc)

	// Begin the step's assistant row exactly like prepareStep does, against
	// a real message service (same helper as the retry fixture).
	conn := batchFixtureConnNoFK(t)
	svc := message.NewService(db.New(conn))
	msg, err := svc.Create(context.Background(), "sess-reason", message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)
	ts := &turnStream{
		a:            &sessionAgent{messages: svc},
		ctx:          t.Context(),
		genCtx:       t.Context(),
		call:         SessionAgentCall{SessionID: "sess-reason"},
		stall:        sc,
		bumpActivity: sc.MarkByte,
		notifyUI:     func() error { return nil },
	}
	ts.beginAssistantStep(&msg)

	set(t0.Add(5 * time.Minute))
	require.NoError(t, ts.onReasoningDelta("r1", "thinking hard"))
	require.NoError(t, ts.onReasoningDelta("r1", " still thinking"))

	progressAfter, byteAfter, reasoningChars := drainctl.StallSnapshot(sc)
	require.Equal(t, progressBefore, progressAfter, "reasoning delta moved lastProgress")
	require.Greater(t, byteAfter, byteBefore, "reasoning delta did not move lastByte")
	require.Equal(t, int64(len("thinking hard")+len(" still thinking")), reasoningChars)
}
