package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Revert-check: removing decidePhase's quota branch starts a root Drain after child completion.
func TestCLIQuotaLiveChildAndBash(t *testing.T) {
	t.Run("finished", func(t *testing.T) { testCLIQuotaLiveChildAndBash(t, false) })
	t.Run("question", func(t *testing.T) { testCLIQuotaLiveChildAndBash(t, true) })
}

func testCLIQuotaLiveChildAndBash(t *testing.T, question bool) {
	var resumed atomic.Bool
	var resumeSawQuestion atomic.Bool
	var resumeChildID string
	var rootRequests, childRequests atomic.Int32
	var childStarted atomic.Bool
	var workerModel atomic.Bool
	application, sid := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			loopText(w, "title", "quota child", 1, 1)
			return
		}
		if resumed.Load() {
			if strings.Contains(system, "an agent for Rush") {
				loopText(w, "answered", "child used port 8080", 1, 1)
				return
			}
			if strings.Contains(string(body), "which port?") {
				resumeSawQuestion.Store(true)
			}
			if !strings.Contains(string(body), "child used port 8080") {
				admissionWriteSSE(w, []string{admissionSSEToolCall("answer", "answer-child", "agent", `{"resume_session_id":`+jsonString(resumeChildID)+`,"prompt":"use 8080"}`), admissionSSEStop("answer", "tool_calls")})
			} else {
				loopText(w, "resumed", "answered child", 1, 1)
			}
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			childStarted.Store(true)
			workerModel.Store(strings.Contains(string(body), `"model":"worker-probe"`))
			childRequests.Add(1)
			time.Sleep(8 * time.Second)
			if question {
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("child", "ask", "ask_question", `{"question":"which port?","options":["8080"]}`),
					admissionSSEStop("child", "tool_calls"),
				})
			} else {
				loopText(w, "child", "different-model child finished", 1, 1)
			}
			return
		}
		switch rootRequests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("root", "bash-job", "bash", `{"command":"sleep 10 && echo quota-bash-output","description":"quota job","run_in_background":true}`),
				admissionSSEStop("root", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("root", "delegation", "agent", `{"prompt":"finish a child task"}`),
				admissionSSEStop("root", "tool_calls"),
			})
		case 3, 4, 5:
			require.Eventually(t, childStarted.Load, 10*time.Second, 10*time.Millisecond)
			quotaResponse(w)
		default:
			t.Errorf("unexpected own request after quota")
			loopText(w, "extra", "unexpected", 1, 1)
		}
	})
	cfg := application.config.Config()
	p, _ := cfg.Providers.Get("openaicompat")
	p.Models = append(p.Models, catwalk.Model{ID: "worker-probe", Name: "worker-probe", ContextWindow: 200000, DefaultMaxTokens: 1000})
	cfg.Providers.Set("openaicompat", p)
	application.config.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{Provider: "openaicompat", Model: "worker-probe"})
	grantChildBackgroundShellTools(t, application)
	grantChildWakeTools(t, application)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var out syncBuffer
	res, err := application.RunNonInteractiveWithResult(ctx, &out, "delegate and run bash", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sid, false)
	require.Error(t, err)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.EqualValues(t, 5, rootRequests.Load())
	require.EqualValues(t, 1, childRequests.Load())
	require.True(t, workerModel.Load())
	require.Contains(t, out.String(), "quota-bash-output")
	if question {
		require.Len(t, res.PendingChildQuestions, 1)
		require.Contains(t, out.String(), "which port?")
	} else {
		require.Contains(t, out.String(), "different-model child finished")
	}
	rows, listErr := application.asyncJobStore.ListAsyncJobsForOwner(ctx, sid)
	require.NoError(t, listErr)
	require.NotEmpty(t, rows)
	if question {
		resumeChildID = res.PendingChildQuestions[0].SessionID
		store, dataDir := application.config, application.dataDir
		application.Shutdown()
		conn, e := db.Connect(ctx, dataDir)
		require.NoError(t, e)
		t.Cleanup(func() { _ = db.ReleaseConn(conn) })
		fresh, e := New(ctx, conn, store)
		require.NoError(t, e)
		t.Cleanup(fresh.Shutdown)
		resumed.Store(true)
		var resumedOut syncBuffer
		again, e := fresh.RunNonInteractiveWithResult(ctx, &resumedOut, "answer the pending child question", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sid, false)
		require.NoError(t, e)
		require.Equal(t, "answered child", again.FinalText)
		require.True(t, resumeSawQuestion.Load())
	}
}
