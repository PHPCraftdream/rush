package app

// R5-3 pinned that terse mode prints one final text, once, after the
// reviewer gate decides. A10/C9-21 settles WHICH text that is: always the
// executor's — the reviewer's verdict is an additive envelope field, never
// the run's stdout answer.

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func terseContinuationRequest(output *bytes.Buffer) RunRequest {
	return RunRequest{
		Prompt:      "write a long report",
		Overrides:   RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:        RunModeTerse,
		Stdout:      output,
		Stderr:      io.Discard,
		HideSpinner: true,
	}
}

// TestExecuteRunTerseReviewerPassPrintsOnlyFinalResult pins the A10/C9-21
// stdout contract for an auto-reviewed run: stdout carries exactly the
// EXECUTOR's final text (the continuation chain, combined) plus the
// trailing newline — never the reviewer's verdict, never both concatenated.
// REVERT CHECK: restoring `reviewResult != nil && primaryResult != nil` in
// the reviewer switch's first case fails this test with the reviewer's
// text printed instead of the run's answer.
func TestExecuteRunTerseReviewerPassPrintsOnlyFinalResult(t *testing.T) {
	application := newContinuationApp(t, true)

	var output bytes.Buffer
	_, err := application.ExecuteRun(context.Background(), terseContinuationRequest(&output))
	require.NoError(t, err)

	require.Equal(t, contPartialText+contFinalText+"\n", output.String(),
		"terse stdout must be exactly the executor's final text printed once")
	require.NotContains(t, output.String(), contReviewText,
		"the reviewer's verdict must never be printed as the run's answer")
}

// TestExecuteRunTerseWithoutReviewerPassPrintsSingleFinalText pins the
// other half of R5-3: without a reviewer pass the terse run prints the
// single final text of its own phase — finish() selected the combined
// continuation-chain text — byte-for-byte the same output the old
// per-message printing produced for this chain, now emitted once.
func TestExecuteRunTerseWithoutReviewerPassPrintsSingleFinalText(t *testing.T) {
	application := newContinuationApp(t, false)

	var output bytes.Buffer
	_, err := application.ExecuteRun(context.Background(), terseContinuationRequest(&output))
	require.NoError(t, err)

	require.Equal(t, contPartialText+contFinalText+"\n", output.String(),
		"terse stdout must be exactly the run's combined final text printed once")
}
