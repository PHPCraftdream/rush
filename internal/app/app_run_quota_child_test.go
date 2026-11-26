package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// Revert-check: removing decidePhase's quota branch starts a root Drain after child completion.
// Revert-check of the gate: a fixed sleep could finish before the latch under load, dropping the job from finished_work via snapshotQuotaTerminal.
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
	for range agentcall.ProviderMaxRetries {
		retryWait += delay
		delay = time.Duration(float64(delay) * retry.BackoffFactor)
	}
	return retryWait
}

// workMargin keeps the child handler alive past the quota latch.
const workMargin = 4 * time.Second

type quotaDiagnosticWriter struct {
	out    io.Writer
	record func(string)
}

func (w quotaDiagnosticWriter) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, "hard provider quota exhausted") {
		w.record("stderr latch: " + strings.TrimSpace(line))
	}
	if strings.Contains(line, "database is closed") {
		w.record("stderr db closed: " + strings.TrimSpace(line))
	}
	if w.out == nil {
		return len(p), nil
	}
	return w.out.Write(p)
}

func testCLIQuotaLiveChildAndBash(t *testing.T, question bool) {
	var rootRequests, childRequests atomic.Int32
	var childStarted atomic.Bool
	var workerModel atomic.Bool
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "quota-bash-gate"))
	var timelineMu sync.Mutex
	var timeline []string
	record := func(event string) {
		timelineMu.Lock()
		timeline = append(timeline, time.Now().Format(time.RFC3339Nano)+" "+event)
		timelineMu.Unlock()
	}
	var out syncBuffer
	origStderr := cliLoopStderr
	cliLoopStderr = quotaDiagnosticWriter{out: origStderr, record: record}
	t.Cleanup(func() { cliLoopStderr = origStderr })
	var application *App
	var sid string
	dumpFailure := func() {
		if !t.Failed() {
			return
		}
		timelineMu.Lock()
		defer timelineMu.Unlock()
		t.Logf("failure diagnostics: output:\n%s", out.String())
		for _, event := range timeline {
			t.Logf("timeline: %s", event)
		}
		if application != nil {
			rows, err := application.DB().QueryContext(context.Background(),
				`SELECT node_id, root_id, kind, state, announced, outcome_summary, announce_message_id, datetime(updated_at, 'unixepoch'), delivery FROM batch_nodes WHERE owner_session_id = ?`, sid)
			if err != nil {
				t.Logf("batch nodes query: %v", err)
			} else {
				defer rows.Close()
				for rows.Next() {
					var nodeID, rootID, kind, state, outcome, updatedAt, delivery string
					var announced bool
					var announceID sql.NullString
					if err := rows.Scan(&nodeID, &rootID, &kind, &state, &announced, &outcome, &announceID, &updatedAt, &delivery); err != nil {
						t.Logf("batch node scan: %v", err)
						break
					}
					t.Logf("batch node: node=%s root=%s kind=%s state=%s announced=%t announce_msg=%v updated_at=%s delivery=%s outcome=%q", nodeID, rootID, kind, state, announced, announceID, updatedAt, delivery, outcome)
				}
				if err := rows.Err(); err != nil {
					t.Logf("batch nodes rows: %v", err)
				}
			}
		}
		// Shell job registry state is not exposed to this package's tests.
	}
	application, sid = newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			loopText(w, "title", "quota child", 1, 1)
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			record("child request served")
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
		requestNo := rootRequests.Add(1)
		record(fmt.Sprintf("root request %d served", requestNo))
		switch requestNo {
		case 1:
			command := fmt.Sprintf(`i=0; while [ "$i" -lt 150 ]; do if [ -f '%s' ]; then break; fi; sleep 0.2; i=$((i+1)); done; echo quota-bash-output`, marker)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("root", "bash-job", "bash", fmt.Sprintf(`{"command":%q,"description":"quota job","run_in_background":true}`, command)),
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
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			if requestNo == 5 {
				// The final response triggers the quota latch. Releasing the
				// bash gate only after this delay makes its completion causally
				// later than the latch, independent of scheduler load.
				time.Sleep(2 * time.Second)
				if err := os.WriteFile(filepath.FromSlash(marker), []byte("released"), 0o644); err != nil {
					t.Errorf("write bash gate marker: %v", err)
				} else {
					record("bash gate marker written")
				}
			}
		default:
			t.Errorf("unexpected own request after quota")
			loopText(w, "extra", "unexpected", 1, 1)
		}
	})
	t.Cleanup(dumpFailure)
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
	// The gate removes bash duration from the budget; allow the child sleep,
	// post-latch wait, and generous scheduling/cleanup margin.
	const schedulingMargin = 30 * time.Second
	timeout := retryWait + (retryWait + workMargin) + schedulingMargin
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	res, err := application.RunNonInteractiveWithResult(ctx, &out, "delegate and run bash", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sid, false)
	require.Error(t, err)
	require.Equal(t, "provider_limit", res.ExitReason)
	// 2 setup + initial attempt + 2 pinned retries.
	require.EqualValues(t, int32(2+agentcall.ProviderMaxRetries+1), rootRequests.Load())
	require.EqualValues(t, 1, childRequests.Load())
	require.True(t, workerModel.Load())
	require.Contains(t, out.String(), "quota-bash-output")
	if question {
		require.Len(t, res.PendingChildQuestions, 1)
		require.Contains(t, out.String(), "which port?")
	} else {
		require.Contains(t, out.String(), "different-model child finished")
	}
	if question {
		// The run must have exited provider_limit with the pending
		// question (shared assertions above), and the HELD identity must
		// survive the host: the fenced LiveHeldQuestions row, its marker
		// notice, and a child still inside its held Ask (no finished
		// assistant message). No cross-host continuation is claimed: the
		// design requires interruption after host shutdown.
		facts := agent.NewBatchCutoverNodeFacts(session.NewBatchStore(application.DB()), nil, application.DB())
		held, heldErr := facts.LiveHeldQuestions(ctx, sid)
		require.NoError(t, heldErr)
		require.Len(t, held, 1)
		require.Equal(t, res.PendingChildQuestions[0].SessionID, held[0].ChildSessionID)
		var noticeText string
		require.NoError(t, application.DB().QueryRowContext(ctx,
			`SELECT text FROM session_notices WHERE id = ? AND owner = ?`, held[0].ID, sid).Scan(&noticeText))
		parsed, ok := sessionnotice.ParseBatchChildQuestionNotice(noticeText)
		require.True(t, ok)
		require.Equal(t, held[0].NodeID, parsed.NodeID)
		require.Equal(t, held[0].ChildSessionID, parsed.ChildSessionID)
		childMsgs, msgErr := application.Messages.List(ctx, held[0].ChildSessionID)
		require.NoError(t, msgErr)
		for _, msg := range childMsgs {
			require.False(t, msg.Role == message.Assistant && msg.IsFinished(),
				"the child remains inside its held Ask")
		}
	} else {
		// The finished subtest only: the delegation COMPLETED here, so its
		// root is terminal (the question subtest's delegation is HELD/live
		// and must never be asserted completed). The two logical jobs are
		// terminal in the batch tables -- one completed root per tool call id.
		e3BatchJobIdentityPresent(t, application, sid, "bash-job", "quota-bash-output")
		e3BatchJobIdentityPresent(t, application, sid, "delegation", "")
	}
}
