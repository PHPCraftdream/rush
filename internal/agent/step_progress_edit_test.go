package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type noWriteEditTracker struct{}

func (noWriteEditTracker) RecordRead(context.Context, string, string) {}

func (noWriteEditTracker) LastReadTime(context.Context, string, string) time.Time {
	return time.Now().Add(time.Second)
}

func (noWriteEditTracker) ListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func nativeNoWriteEditResult(t *testing.T, equal bool) fantasy.ToolResultContent {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("original\n"), 0o600))
	old, replacement := "missing", "replacement"
	if equal {
		old, replacement = "original", "original"
	}
	input, err := json.Marshal(tools.EditParams{FilePath: path, OldString: old, NewString: replacement})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "no-write-session")
	tool := tools.NewEditTool(nil, nil, noWriteEditTracker{}, dir)
	model := &guardStepModel{toolName: tools.EditToolName, toolInput: string(input)}
	runner := fantasy.NewAgent(model,
		fantasy.WithTools(&progressGuardTool{inner: tool, guard: stepGuard{}}),
		fantasy.WithStopConditions(fantasy.StepCountIs(3)))
	var got fantasy.ToolResultContent
	_, err = runner.Stream(ctx, fantasy.AgentStreamCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage("edit fixture")},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			got = result
			return nil
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.ToolCallID)
	require.Equal(t, fantasy.ToolResultContentTypeError, got.Result.GetType())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original\n", string(content))
	return got
}

func TestNativeNoWriteEditMetadataSurvivesFantasyAndStopsRereadCycle(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	missing := nativeNoWriteEditResult(t, false)
	equal := nativeNoWriteEditResult(t, true)
	call := fantasy.ToolCallContent{ToolName: tools.EditToolName}
	for _, result := range []fantasy.ToolResultContent{missing, equal} {
		require.Equal(t, stepCallNoWrite, classifyStepCall(call, result, stepClassifyCtx{}), "real native edit metadata was lost")
	}

	view := fantasy.ToolCallContent{ToolName: tools.ViewToolName, Input: `{"file_path":"fixture.txt","offset":0,"limit":10}`}
	var progress turnProgress
	apply := func(call fantasy.ToolCallContent, result fantasy.ToolResultContent) stepCallClass {
		t.Helper()
		class := classifyStepCall(call, result, stepClassifyCtx{seen: progress.seen.snapshot()})
		progress.applyStep([]stepCallClass{class}, [][]namedSpan{readSpans(call)})
		return class
	}
	require.Equal(t, stepCallRead, apply(view, fantasy.ToolResultContent{}))
	require.Equal(t, stepCallNoWrite, apply(call, missing))
	require.Equal(t, 1, progress.streak)
	require.Equal(t, stepCallReread, apply(view, fantasy.ToolResultContent{}), "a rejected edit must not erase prior read coverage")
	require.Equal(t, 2, progress.streak)
	require.True(t, stepGuardFor(&progress, false, false).refuseRereadCall(readSpans(view)))
	require.Equal(t, stepCallNoWrite, apply(call, equal))
	require.Equal(t, stepCallNoWrite, apply(call, missing))
	require.Equal(t, stepCallNoWrite, apply(call, equal))
	require.Equal(t, inTurnStopAfter, progress.streak)
	require.True(t, progress.stopped)
	require.Equal(t, 4, progress.noWrites)
	require.Contains(t, guardStopDetails(progress), "4 native edit/multiedit call(s) made no file content write")
	require.True(t, progress.seen.covered("fixture.txt", readSpan{start: 0, end: 10}))

	ts := &turnStream{att: &drainAttempt{}}
	call.ToolCallID = missing.ToolCallID
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{call, missing}}})
	require.False(t, ts.att.chainProgress, "a no-write edit must not reset reaction-chain evidence")
	require.Empty(t, ts.att.chainIdleClaims)
}

func TestNoWriteEditProofIsNarrowAndConservative(t *testing.T) {
	proof := `{"edit_effect":{"no_write":true,"reason":"missing_old"}}`
	for _, tc := range []struct {
		name, tool, metadata string
		textResult           bool
	}{
		{"absent proof", tools.EditToolName, "", false},
		{"malformed proof", tools.EditToolName, `{"edit_effect":`, false},
		{"false proof", tools.EditToolName, `{"edit_effect":{"no_write":false,"reason":"missing_old"}}`, false},
		{"unknown reason", tools.EditToolName, `{"edit_effect":{"no_write":true,"reason":"write_failed"}}`, false},
		{"wrong type", tools.EditToolName, `{"edit_effect":{"no_write":"true","reason":"missing_old"}}`, false},
		{"shell error", tools.BashToolName, proof, false},
		{"MCP error", "mcp_edit", proof, false},
		{"multiedit partial error", "multiedit", proof, false},
		{"successful result", tools.EditToolName, proof, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := fantasy.ToolResultContent{ClientMetadata: tc.metadata, Result: fantasy.ToolResultOutputContentError{Error: errors.New("old_string not found")}}
			if tc.textResult {
				result.Result = fantasy.ToolResultOutputContentText{Text: "changed"}
			}
			require.Equal(t, stepCallAct, classifyStepCall(fantasy.ToolCallContent{ToolName: tc.tool}, result, stepClassifyCtx{}))
		})
	}
}

func TestNoWriteEditOnlyStepsStopAndRealProgressStillResets(t *testing.T) {
	var progress turnProgress
	for i := range inTurnStopAfter {
		progress.applyStep([]stepCallClass{stepCallNoWrite}, [][]namedSpan{nil})
		require.Equal(t, i+1, progress.streak)
		require.Equal(t, i+1 == inTurnStopAfter, progress.stopped)
	}
	for _, classes := range [][]stepCallClass{{stepCallAct}, {stepCallRead}, {stepCallNoWrite, stepCallAct}, {stepCallNoWrite, stepCallRead}} {
		progress = turnProgress{streak: 3, seen: readCoverage{}}
		progress.seen.add("unchanged.txt", readSpan{start: 0, end: 10})
		progress.applyStep(classes, make([][]namedSpan, len(classes)))
		require.Zero(t, progress.streak, "genuine progress must win over no-write attempts")
		require.False(t, progress.stopped)
		if hasClass(classes, stepCallAct) {
			require.Empty(t, progress.seen, "potential writes must still invalidate coverage")
		} else {
			require.True(t, progress.seen.covered("unchanged.txt", readSpan{start: 0, end: 10}))
		}
	}
}
