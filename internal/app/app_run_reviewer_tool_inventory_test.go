package app

// F8 (2026-09-22 audit): the run-wide tool-call inventory must span BOTH
// phases of an auto-reviewed run. finish() used to REPLACE toolCallCounts
// with the current phase's reconciled counts after resetForReviewerPass
// had zeroed them, so a primary that made tool calls plus a reviewer that
// made none produced a final envelope with an EMPTY tool_calls list (and
// the sub-agent reduction warning lost the primary phase's delegation
// calls). These tests pin the fix: the primary phase's calls survive into
// the reviewed run's envelope.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestExecuteRunReviewerPassToolCallsSpanBothPhases is the F8 acceptance
// test: the primary turn makes one view call (after a continuation
// chain), then the reviewer pass answers with no tool calls at all. The
// FINAL envelope's tool_calls must still report the primary phase's
// single view call — the run-wide inventory must survive the phase
// boundary instead of being replaced by the review turn's (empty)
// reconciled counts.
func TestExecuteRunReviewerPassToolCallsSpanBothPhases(t *testing.T) {
	viewTarget := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(viewTarget, []byte("some notes"), 0o644))
	toolArgs, err := json.Marshal(map[string]string{"file_path": viewTarget})
	require.NoError(t, err)

	app := newContinuationAppWithStub(t, true, func(n int, w http.ResponseWriter, r *http.Request) {
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, sseChunk(contPartialText))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(300 * time.Millisecond)
			panic(http.ErrAbortHandler)
		case 2, 3:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"transient stub failure","type":"server_error"}}`)
		case 4:
			// The primary turn's continuation attempt calls view once
			// — the tool call that must survive into the final
			// envelope across the reviewer pass.
			_, _ = fmt.Fprint(w, sseToolCallChunk("view", string(toolArgs)))
			_, _ = fmt.Fprint(w, sseToolCallsFinishChunk())
			_, _ = fmt.Fprint(w, sseDone)
		default:
			// The round after the view tool result: clean finish.
			_, _ = fmt.Fprint(w, sseChunk(contFinalText))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	})

	result, err := app.ExecuteRun(context.Background(), RunRequest{
		Prompt:      "write a long report",
		Overrides:   RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:        RunModeJSON,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, contReviewText, result.FinalText,
		"the reviewer pass's own response must remain the final output")
	// The key F8 assertion: the primary phase's single view call must
	// appear in the FINAL envelope even though the reviewer phase made
	// no tool calls.
	require.Equal(t, []ToolCallStat{{Name: "view", Count: 1}}, result.ToolCalls)
}

// TestMergeReconciledToolCallsDeduplicatesAcrossPhases pins the merge
// requirement at unit level: merge tool IDs without duplicates. A call
// whose ID was already folded in by an earlier phase must not
// double-count, ID-less calls are added as reported, and toolCallCounts
// aliases the run-wide inventory so the summary reads from it.
func TestMergeReconciledToolCallsDeduplicatesAcrossPhases(t *testing.T) {
	loop := &executeRunLoop{}

	// Phase one: one ID-bearing view call plus two ID-less bash calls.
	loop.mergeReconciledToolCalls(terminalReconciliation{
		toolCallByID:  map[string]string{"c1": "view"},
		toolCallsNoID: map[string]int{"bash": 2},
	})
	// Phase two repeats the same view call ID and adds an edit.
	loop.mergeReconciledToolCalls(terminalReconciliation{
		toolCallByID: map[string]string{"c1": "view", "c2": "edit"},
	})

	require.Equal(t, map[string]int{"view": 1, "bash": 2, "edit": 1}, loop.invocationToolCalls,
		"the duplicate c1 must NOT double-count view")
	require.Equal(t, map[string]struct{}{"c1": {}, "c2": {}}, loop.invocationToolCallIDs)
	require.Equal(t, loop.invocationToolCalls, loop.toolCallCounts,
		"toolCallCounts must alias the run-wide inventory")
}
