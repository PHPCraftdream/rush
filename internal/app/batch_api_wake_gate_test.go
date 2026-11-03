package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// runHeldQuestionBatch submits a batch whose child asks a question under an
// owner session of the given origin and returns how many provider requests the
// owner session itself made within the observation window.
func runHeldQuestionBatch(t *testing.T, origin message.Origin) int32 {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	var ownerID string
	var ownerCalls atomic.Int32
	childAsked := make(chan struct{})
	var childAskOnce atomic.Bool
	application, _ := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			admissionWriteSSE(w, []string{admissionSSEText("title", "Gate"), admissionSSEStop("title", "stop")})
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			if childAskOnce.CompareAndSwap(false, true) {
				close(childAsked)
			}
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-ask", "call-ask", "ask_question", `{"question":"Which port?","options":["8080"]}`),
				admissionSSEStop("child-ask", "tool_calls"),
			})
			return
		}
		if ownerID != "" && r.Header.Get("x-session-id") == session.HashID(ownerID) {
			ownerCalls.Add(1)
		}
		admissionWriteSSE(w, []string{admissionSSEText("owner", "owner turn"), admissionSSEStop("owner", "stop")})
	})
	grantChildWakeTools(t, application)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	creator, ok := application.Sessions.(session.OriginCreator)
	require.True(t, ok, "the session service must create sessions with an origin")
	owner, err := creator.CreateWithOrigin(ctx, "batch owner", origin)
	require.NoError(t, err)
	require.Equal(t, origin, owner.Origin)
	ownerID = owner.ID
	payload, err := json.Marshal(map[string]any{"tool": "agent", "input": map[string]any{"prompt": "ask a question"}})
	require.NoError(t, err)
	_, err = application.BatchSubmit(ctx, owner.ID, jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode:  jobbatch.ModeSequential,
		Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Payload: payload}}},
	}})
	require.NoError(t, err)
	select {
	case <-childAsked:
	case <-ctx.Done():
		require.FailNow(t, "child did not reach ask_question", "%v", ctx.Err())
	}
	// Bounded observation window for a wake-driven owner turn.
	time.Sleep(pwWakeTimeout)
	return ownerCalls.Load()
}

func TestBatchHeldQuestion_APIServiceOwnerIsNeverWoken(t *testing.T) {
	require.Zero(t, runHeldQuestionBatch(t, message.OriginAPIService), "an API service session must not run a model turn for a held child question")
}

func TestBatchHeldQuestion_ConversationalOwnerIsStillWoken(t *testing.T) {
	require.Positive(t, runHeldQuestionBatch(t, message.OriginCLI), "the gate must not silence conversational owners")
}
