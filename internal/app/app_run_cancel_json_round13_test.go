package app

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestFinishMarksUnconfirmedDrainEndTurnCanceledInJSON_R13_2(t *testing.T) {
	h := newCycle6RunApp(t)

	aMsg, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "A's superseded text"}},
	})
	require.NoError(t, err)
	aMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), aMsg))

	aRecorder := agent.NewCallResultRecorder()
	aRecorder.RecordConfirmed(aMsg.ID)
	bRecorder := agent.NewCallResultRecorder()
	hookExitReason := new(string)
	loop := &executeRunLoop{
		app:                   h.app,
		sess:                  h.sess,
		ctx:                   context.Background(),
		mode:                  RunModeJSON,
		stdout:                io.Discard,
		stderr:                io.Discard,
		stopSpinner:           func() {},
		runStart:              time.Now().Add(-time.Second),
		hookExitReason:        hookExitReason,
		baselineIDs:           map[string]struct{}{},
		baselineKnown:         true,
		toolCallCounts:        map[string]int{},
		overrides:             RunOverrides{StripJSONFences: true},
		finalReason:           string(message.FinishReasonEndTurn),
		finalText:             `B's live partial text {"ok":true}`,
		invocationToolCalls:   map[string]int{},
		invocationToolCallIDs: map[string]struct{}{},
		callResultRec:         aRecorder,
		eventOwner:            bRecorder,
	}

	result, err := loop.finish(context.Canceled)
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.Equal(t, "canceled", result.ExitReason)
	require.NotEmpty(t, result.Error)
	require.Equal(t, "canceled", *hookExitReason)
	require.Equal(t, `B's live partial text {"ok":true}`, result.FinalText, "unconfirmed text may remain visible as partial output without JSON extraction")

	wire, err := json.Marshal(result)
	require.NoError(t, err)
	var outcome RunResult
	require.NoError(t, json.Unmarshal(wire, &outcome))
	require.Equal(t, "canceled", outcome.ExitReason)
	require.NotEmpty(t, outcome.Error)
}
