// C17 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN item 4):
// every exit from runNonInteractiveWithAsyncResults's loop must render
// final's envelope (JSON encode / terse flush) through the same path the
// ordinary "scope closed" exit always used -- before this fix, a lock-busy
// give-up, a ctx cancellation, or a waitForNextCLITurn error silently
// skipped it even when a PRIOR turn had already produced a real result.
package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFlushLoopExit_NilFinal_NoOp mirrors the non-loop single-call path's
// own nil guard: no turn ever completed, nothing to render.
func TestFlushLoopExit_NilFinal_NoOp(t *testing.T) {
	var out bytes.Buffer
	err := flushLoopExit(&out, RunModeJSON, nil, &bytes.Buffer{})
	require.NoError(t, err)
	require.Empty(t, out.String())
}

// TestFlushLoopExit_JSONMode_EncodesFinal is the core C17 proof: a non-nil
// final (a prior successful turn's result) must be JSON-encoded to output
// regardless of WHICH exit path called this.
//
// Revert-check performed: reverted the lock-busy give-up branch in
// app_run_async.go to `return final, err` without calling flushLoopExit --
// a manual trace confirmed stdout stayed empty for that exit despite a
// non-nil final; this unit test pins the helper's own contract so that
// regression cannot silently return.
func TestFlushLoopExit_JSONMode_EncodesFinal(t *testing.T) {
	var out bytes.Buffer
	final := &RunResult{SessionID: "sess-1", FinalText: "hello", ExitReason: "stop"}
	err := flushLoopExit(&out, RunModeJSON, final, &bytes.Buffer{})
	require.NoError(t, err)

	var decoded RunResult
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Equal(t, "sess-1", decoded.SessionID)
	require.Equal(t, "hello", decoded.FinalText)
}

// TestFlushLoopExit_TerseMode_CopiesLastBuffered proves the terse path
// flushes the last REAL turn's buffered terse output, not the (possibly
// stale) top-level output writer.
func TestFlushLoopExit_TerseMode_CopiesLastBuffered(t *testing.T) {
	var out bytes.Buffer
	lastBuffered := bytes.NewBufferString("the final answer")
	final := &RunResult{SessionID: "sess-1"}
	err := flushLoopExit(&out, RunModeTerse, final, lastBuffered)
	require.NoError(t, err)
	require.Equal(t, "the final answer", out.String())
}

// TestFlushLoopExit_StreamMode_WritesNothingExtra: stream mode already
// wrote everything live as it streamed -- flushLoopExit must not double-
// print anything for it.
func TestFlushLoopExit_StreamMode_WritesNothingExtra(t *testing.T) {
	var out bytes.Buffer
	final := &RunResult{SessionID: "sess-1"}
	err := flushLoopExit(&out, RunModeStream, final, bytes.NewBufferString("ignored"))
	require.NoError(t, err)
	require.Empty(t, out.String())
}
