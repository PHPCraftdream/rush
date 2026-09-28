package app

// Supervision (task #1043, docs/plans/2026-09-27-async-structured-
// concurrency.md §7): the periodic root-session check-in must never hold
// `rush run` open by itself. internal/agent/supervision.go's own design
// keeps a supervision timer entirely outside workLedger's job map, so
// next()/the CLI exit path are unaffected by it by construction -- this
// black-box test proves the OBSERVABLE consequence at the layer `rush run`
// actually runs at: a run that starts a background command (which arms
// supervision, on by default) still exits promptly once that command and
// the resulting turn finish, nowhere near the 5-minute default supervision
// interval.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestRunNonInteractiveExitsPromptlyWithSupervisionArmedAndNoOtherWork: a
// root run that starts one background command (arming supervision the
// instant it starts) must exit as soon as that command's own work drains --
// not wait for, or be delayed by, the supervision timer in any way. The
// 5-second context bound is far tighter than the 5-minute default
// supervision interval: only a supervision-shaped hang (or its absence)
// could plausibly separate a pass from a timeout here, unlike a looser
// bound that would also pass for unrelated reasons.
//
// Revert-check: not performed by neutering production code (that would
// require literally making supervision hold the ledger's job map open,
// which supervision.go's design makes structurally impossible without a
// much larger, unrelated rewrite -- see that file's own doc for why). The
// property this test checks is instead already covered by
// internal/agent's TestSupervision_StateRemovedOnScopeClose (revert-checked
// there against deliverLocked's cleanup hook) at the exact mechanism
// (workLedger.next()) this test's CLI-level pass depends on; this test adds
// the black-box confirmation at the layer the task specifically asked for.
func TestRunNonInteractiveExitsPromptlyWithSupervisionArmedAndNoOtherWork(t *testing.T) {
	var requests atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("sup-start", "call-sup", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("sup-start", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("sup-initial", "job launched"), admissionSSEStop("sup-initial", "stop")})
		case 3:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "Async job call-sup") {
				http.Error(w, "missing completion notice", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("sup-final", "work complete"), admissionSSEStop("sup-final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &stdout, "start a background command", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err, "the run must not time out waiting on anything supervision-shaped")
	require.NotNil(t, result)
	require.Equal(t, "work complete", result.FinalText)
	require.EqualValues(t, 3, requests.Load())

	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "work complete", wire.FinalText)
}
