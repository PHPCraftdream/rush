package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
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

// quotaRetryWait mirrors fantasy's exponential backoff (retry.go) over the
// pinned retry count: 5s + 10s = 15s with ProviderMaxRetries = 2.
func quotaRetryWait() time.Duration {
	retry := fantasy.DefaultRetryOptions()
	retryWait := time.Duration(0)
	delay := retry.InitialDelayIn
	for range agent.ProviderMaxRetries {
		retryWait += delay
		delay = time.Duration(float64(delay) * retry.BackoffFactor)
	}
	return retryWait
}

// workMargin keeps each live-work fixture comfortably past the quota latch.
const workMargin = 4 * time.Second

// quotaBashSeconds sizes the root bash job to span the latch (rounded up).
var quotaBashSeconds = int((quotaRetryWait() + workMargin) / time.Second)

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
			// The latch fires only after the root turn exhausts its
			// pinned retries, so the child must outlive retryWait to be
			// collected as live work after the latch.
			time.Sleep(quotaRetryWait() + workMargin)
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
				admissionSSEToolCall("root", "bash-job", "bash", fmt.Sprintf(`{"command":"sleep %d && echo quota-bash-output","description":"quota job","run_in_background":true}`, quotaBashSeconds)),
				admissionSSEStop("root", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("root", "delegation", "agent", `{"prompt":"finish a child task"}`),
				admissionSSEStop("root", "tool_calls"),
			})
		case 3, 4, 5:
			// Requests 1–2 start bash/delegation; requests 3..5 = 2 setup
			// + initial attempt + 2 pinned retries, all quota-latched.
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
	// retryWait = InitialDelayIn * (BackoffFactor^i - 1) summed over the
	// pinned retries = 5s+10s = 15s with ProviderMaxRetries = 2.
	retryWait := quotaRetryWait()
	// Bash runs workMargin longer than the child handler sleep, so the
	// budget is the pre-latch wait plus the longest live work plus a
	// scheduling margin.
	const schedulingMargin = 15 * time.Second
	timeout := retryWait + (retryWait + workMargin) + schedulingMargin
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var out syncBuffer
	res, err := application.RunNonInteractiveWithResult(ctx, &out, "delegate and run bash", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sid, false)
	require.Error(t, err)
	require.Equal(t, "provider_limit", res.ExitReason)
	// 2 setup + initial attempt + 2 pinned retries.
	require.EqualValues(t, int32(2+agent.ProviderMaxRetries+1), rootRequests.Load())
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
