// Package N e2e: the selected notice's content must be the job's real result.
// An ordinary direct tool call settles a structural root whose OWN
// OutcomeSummary is empty while the sole task child carries the output; the
// pull-side projection (internal/session/batch_pull.go) must surface that
// output to the model exactly once.
package app

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pnCountSelectedCompletions counts only the selected terminal completion
// notices. The batch runtime's started announcement is ALSO a background-job
// user message with an empty notice kind (batch_install_adapters.go
// ElectStarted), so counting "" would see two messages; only the selected
// completion carries notice kind "root" (internal/jobbatch/notice.go
// selectNotices names the root kind).
func pnCountSelectedCompletions(t *testing.T, application *App, sessionID string) int {
	t.Helper()
	return countBackgroundNotices(t, application, sessionID, "root")
}

// TestBatchNoticeResultE2E_EscapedBashMarkerReachesModelOnce pins the batch
// pull-side projection. Reverting the
// projection reproduces "Async job call-1 () finished.\n\n(no output)" and
// the marker assertions fail.
func TestBatchNoticeResultE2E_EscapedBashMarkerReachesModelOnce(t *testing.T) {
	command, release := recoveryJobCommand(t)
	// The marker is COMPUTED by the shell, so its literal never appears in the
	// command text: the completion notice quotes the command,
	// and counting a literal marker would see it twice (command + output).
	command += "; echo PN-ESCAPED-$((40+2))"
	completionText := make(chan string, 1)
	started := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	var releaseErr atomic.Pointer[error]
	var launches, completions, duplicates atomic.Int32
	var yielded sync.Once
	// pnUnblock is goroutine-safe (pure file write, error recorded, no
	// require): callable from the run goroutine's seam and from cleanup.
	pnUnblock := func() {
		releaseOnce.Do(func() {
			err := os.WriteFile(release, nil, 0o644)
			releaseErr.Store(&err)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	old := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		yielded.Do(func() {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		})
		pnUnblock()
	}
	defer func() { cliLoopTurnDoneSeam = old }()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "call-1") && strings.Contains(lastUser, "finished"):
			completions.Add(1)
			// A duplicate completion request is a double-delivery
			// failure, not a stall: expose it, never block.
			select {
			case completionText <- lastUser:
			default:
				duplicates.Add(1)
			}
			admissionWriteSSE(w, []string{admissionSSEText("final", "PN final"), admissionSSEStop("final", "stop")})
		case strings.Contains(lastTool, "started"):
			startedOnce.Do(func() { close(started) })
			admissionWriteSSE(w, []string{admissionSSEText("yield", "job started"), admissionSSEStop("yield", "stop")})
		case strings.Contains(lastUser, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "PN test"), admissionSSEStop("title", "stop")})
		default:
			launches.Add(1)
			admissionWriteSSE(w, []string{admissionSSEToolCall("start", "call-1", "bash", `{"command":`+jsonString(command)+`,"description":"held job"}`), admissionSSEStop("start", "tool_calls")})
		}
	})
	done := make(chan struct{})
	// Cleanup order: unblock, join the run goroutine bounded, check the
	// recorded release error, then shut the app down.
	t.Cleanup(func() {
		pnUnblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if p := releaseErr.Load(); p != nil {
			require.NoError(t, *p)
		}
		application.Shutdown()
	})
	var runErr error
	var result *RunResult
	go func() {
		defer close(done)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard, "run held job", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The run goroutine is joined (bounded) by the cleanup above, so
		// t.Fatal here leaves nothing dangling.
		t.Fatal("held job run did not finish")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "PN final", result.FinalText)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, completions.Load())
	require.EqualValues(t, 0, duplicates.Load(), "a duplicate completion request is a double-delivery failure")
	select {
	case text := <-completionText:
		require.Contains(t, text, "PN-ESCAPED-42")
		require.Equal(t, 1, strings.Count(text, "PN-ESCAPED-42"), "the result must reach the model exactly once")
	case <-ctx.Done():
		t.Fatal("completion text not observed")
	}
	require.Equal(t, 1, pnCountSelectedCompletions(t, application, sessionID))
}

// TestBatchNoticeResultE2E_DurableRowAnswerReachesProviderOnce is a
// durable-row-to-provider plumbing oracle: a completed delegation row with an
// empty root summary and an answer-bearing task child is seeded through the
// constraint-valid fixture, and the pull-side projection must carry that
// answer into the parent's next provider request exactly once. A REAL
// detached agent child e2e is owned by package F
// (TestRunNonInteractiveWaitsForAsyncSubAgentResult); batchFamilyDelivery
// returns batchDeliveryDetached for agent tools, so children never execute
// inline.
func TestBatchNoticeResultE2E_DurableRowAnswerReachesProviderOnce(t *testing.T) {
	var seenMax int
	var seenMu sync.Mutex
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Track the MAXIMUM marker occurrences within a single request:
		// the projection must never duplicate the answer inside one request.
		if n := strings.Count(string(body), "PN-CHILD-ANSWER"); n > 0 {
			seenMu.Lock()
			if n > seenMax {
				seenMax = n
			}
			seenMu.Unlock()
		}
		admissionWriteSSE(w, []string{admissionSSEText("done", "say done"), admissionSSEStop("done", "stop")})
	})
	t.Cleanup(func() { application.Shutdown() })
	root, node := f6ReserveDelegation(t, application, sessionID, "call-pn-det", "", "agent", "completed")
	proj := session.BatchProjection{
		Host: f6HostID(t, application), RunnerID: f6BatchRunner, Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: sessionID, Root: root, Updates: []db.ProjectBatchNodeParams{
			{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "", ControlState: "none"},
			{NodeID: node, State: "completed", OutcomeState: "completed", OutcomeSummary: "PN-CHILD-ANSWER", ControlState: "none"},
		}, Notices: []db.SelectBatchNodeNoticeParams{{
			NodeID:     root,
			NoticeKind: sql.NullString{String: "root", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
			NoticeOrdinal: sql.NullInt64{Int64: 0, Valid: true}, NoticeCompleted: 1, Wake: 1,
		}}}},
	}
	require.NoError(t, session.NewBatchStore(application.DB()).Project(context.Background(), proj))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "say done", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	seenMu.Lock()
	max := seenMax
	seenMu.Unlock()
	require.Equal(t, 1, max, "the answer must never be duplicated within one provider request")
	require.Equal(t, 1, pnCountSelectedCompletions(t, application, sessionID))
	msgs, err := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, err)
	var noticeText string
	for _, m := range msgs {
		if m.BackgroundJobNotice && m.Role == message.User && strings.Contains(m.FullText(), "PN-CHILD-ANSWER") {
			noticeText = m.FullText()
		}
	}
	require.NotEmpty(t, noticeText, "the persisted notice must carry the projected answer")
}
