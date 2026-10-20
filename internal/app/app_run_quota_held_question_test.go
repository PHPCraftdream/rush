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

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCLIQuotaBusyHeldChildQuestion(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	const question = "Which port should the build use?\nPlease preserve this complete multiline question beyond the display limit: " +
		"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
	var application *App
	var sessionID string
	var rootRequests atomic.Int32
	var childRequests atomic.Int32
	var unexpectedRequests atomic.Int32
	questionReady := make(chan struct{})
	var notified atomic.Bool

	application, sessionID = newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			loopText(w, "title", "quota child", 1, 1)
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			childRequests.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child", "ask", "ask_question", `{"question":`+jsonString(question)+`,"options":["8080"]}`),
				admissionSSEStop("child", "tool_calls"),
			})
			return
		}
		if rootRequests.Add(1) == 1 {
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("parent", "delegate", "agent", `{"prompt":"Inspect the build and ask which port to use."}`),
				admissionSSEStop("parent", "tool_calls"),
			})
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if application != nil && sessionID != "" {
				var count int
				err := application.DB().QueryRowContext(r.Context(), `SELECT count(*) FROM session_notices WHERE owner = ? AND kind = ? AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0 AND text LIKE '[batch-question %'`, sessionID, "child_question").Scan(&count)
				if err == nil && count > 0 {
					if notified.CompareAndSwap(false, true) {
						close(questionReady)
					}
					quotaResponse(w)
					return
				}
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		unexpectedRequests.Add(1)
		quotaResponse(w)
	})
	grantChildWakeTools(t, application)
	ctx, cancel := context.WithTimeout(t.Context(), quotaRetryWait()+40*time.Second)
	defer cancel()
	var out syncBuffer
	result, runErr := application.RunNonInteractiveWithResult(ctx, &out, "delegate to ask the build question", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.Error(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "provider_limit", result.ExitReason)
	require.EqualValues(t, 1, childRequests.Load())
	require.EqualValues(t, agent.ProviderMaxRetries+2, rootRequests.Load())
	require.Zero(t, unexpectedRequests.Load())
	select {
	case <-questionReady:
	default:
		t.Fatal("quota response was not gated on the retained child question")
	}
	require.Len(t, result.PendingChildQuestions, 1)
	pending := result.PendingChildQuestions[0]
	require.NotEmpty(t, pending.SessionID)
	require.Equal(t, question, pending.Details)
	require.Equal(t, question, pending.Title)
	var decoded RunResult
	require.NoError(t, json.Unmarshal([]byte(out.String()), &decoded))
	require.Len(t, decoded.PendingChildQuestions, 1)
	require.Equal(t, question, decoded.PendingChildQuestions[0].Details)

	facts := agent.NewBatchCutoverNodeFacts(session.NewBatchStore(application.DB()), nil, application.DB())
	held, err := facts.LiveHeldQuestions(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.Equal(t, pending.SessionID, held[0].ChildSessionID)
	var text string
	require.NoError(t, application.DB().QueryRowContext(ctx, `SELECT text FROM session_notices WHERE id = ? AND owner = ?`, held[0].ID, sessionID).Scan(&text))
	questionNotice, ok := session.ParseBatchChildQuestionNotice(text)
	require.True(t, ok)
	require.Equal(t, held[0].NodeID, questionNotice.NodeID)
	require.Equal(t, held[0].ChildSessionID, questionNotice.ChildSessionID)

	var finished bool
	messages, err := application.Messages.List(ctx, pending.SessionID)
	require.NoError(t, err)
	for _, msg := range messages {
		if msg.Role == message.Assistant && msg.IsFinished() {
			finished = true
		}
	}
	require.False(t, finished, "the child remains inside its held Ask")
}

func TestQuotaQuestionTextPreservesEmbeddedGuidancePhrase(t *testing.T) {
	question := "first line\n" + strings.Repeat("x", 220) + "\nAnswer with inject_agent in the question itself."
	notice := "[batch-question node=n token=3 sequence=1 depth=1 child=c]\nSUB-AGENT QUESTION: " + question + "\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.Equal(t, question, quotaQuestionText(notice))
}
