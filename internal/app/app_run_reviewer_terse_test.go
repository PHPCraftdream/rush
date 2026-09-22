package app

// R5-3 (2026-09-22 audit): terse mode must carry exactly the single
// final result, printed once after ExecuteRun's reviewer gate has
// decided which phase owns the run's final answer. Before the fix,
// handleMessageEvent published every finished assistant message to
// stdout the moment it arrived, so an auto-reviewed run emitted the
// primary's text first and the reviewer's text right after it —
// "PRIMARYREVIEW\n" concatenated on stdout although the reviewer's
// conclusion is THE output.

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

// TestExecuteRunTerseReviewerPassPrintsOnlyFinalResult pins the R5-3
// stdout contract for an auto-reviewed run: stdout carries exactly the
// reviewer's conclusion followed by the trailing newline — no primary
// chain fragments, no concatenation. Before the fix this buffer held
// the primary chain fragments followed by the reviewer text.
func TestExecuteRunTerseReviewerPassPrintsOnlyFinalResult(t *testing.T) {
	application := newContinuationApp(t, true)

	var output bytes.Buffer
	_, err := application.ExecuteRun(context.Background(), terseContinuationRequest(&output))
	require.NoError(t, err)

	require.Equal(t, contReviewText+"\n", output.String(),
		"terse stdout must be exactly the reviewer's final text printed once")
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
