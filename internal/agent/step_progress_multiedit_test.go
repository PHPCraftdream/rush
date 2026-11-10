package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func nativeGuardEffectResult(t *testing.T, tool fantasy.AgentTool, params any) (fantasy.ToolCallContent, fantasy.ToolResultContent) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	name := tool.Info().Name
	model := &guardStepModel{toolName: name, toolInput: string(input)}
	runner := fantasy.NewAgent(model,
		fantasy.WithTools(&progressGuardTool{inner: tool, guard: stepGuard{}}),
		fantasy.WithStopConditions(fantasy.StepCountIs(3)))
	var got fantasy.ToolResultContent
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "native-effect-transport")
	_, err = runner.Stream(ctx, fantasy.AgentStreamCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage("edit fixture")},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			got = result
			return nil
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.ToolCallID)
	return fantasy.ToolCallContent{ToolCallID: got.ToolCallID, ToolName: name, Input: string(input)}, got
}

func TestNativeMultiEditProofAndFailuresSurviveFantasy(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\r\nbeta\r\n"), 0o600))
	tool := tools.NewMultiEditTool(nil, nil, noWriteEditTracker{}, dir)
	edits := []tools.MultiEditOperation{
		{OldString: "alpha", NewString: "new"},
		{OldString: "missing", NewString: "bad"},
		{OldString: "new", NewString: "alpha"},
	}
	call, result := nativeGuardEffectResult(t, tool, tools.MultiEditParams{FilePath: path, Edits: edits})
	require.Equal(t, fantasy.ToolResultContentTypeError, result.Result.GetType())
	var proof tools.EditEffectMetadata
	require.NoError(t, json.Unmarshal([]byte(result.ClientMetadata), &proof))
	require.True(t, proof.EditEffect.NoWrite)
	require.Equal(t, "unchanged_content", proof.EditEffect.Reason)
	var metadata tools.MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(result.ClientMetadata), &metadata))
	require.Zero(t, metadata.EditsApplied)
	require.Len(t, metadata.EditsFailed, 1)
	require.Equal(t, 2, metadata.EditsFailed[0].Index)
	require.Equal(t, edits[1], metadata.EditsFailed[0].Edit)
	require.Equal(t, "old_string not found in file. Make sure it matches exactly, including whitespace and line breaks", metadata.EditsFailed[0].Error)
	require.Equal(t, stepCallNoWrite, classifyStepCall(call, result, stepClassifyCtx{}))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "alpha\r\nbeta\r\n", string(content))
	ts := &turnStream{att: &drainAttempt{}}
	ts.recordChainEvidence(fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{call, result}}})
	require.False(t, ts.att.chainProgress)
	require.Empty(t, ts.att.chainIdleClaims)
}

func TestNativeEffectToolReasonWhitelistAndPrecedence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{tools.EditToolName, tools.MultiEditToolName, tools.BashToolName, "mcp_edit"} {
		for _, reason := range []string{"missing_old", "equal_content", "multiple_matches", "unchanged_content", "write_failed"} {
			t.Run(name+"/"+reason, func(t *testing.T) {
				t.Parallel()
				metadata := fmt.Sprintf(`{"edit_effect":{"no_write":true,"reason":%q}}`, reason)
				result := fantasy.ToolResultContent{ClientMetadata: metadata, Result: fantasy.ToolResultOutputContentError{Error: errors.New("native rejection")}}
				want := stepCallAct
				if name == tools.EditToolName && (reason == "missing_old" || reason == "equal_content" || reason == "multiple_matches") || name == tools.MultiEditToolName && reason == "unchanged_content" {
					want = stepCallNoWrite
				}
				call := fantasy.ToolCallContent{ToolName: name}
				require.Equal(t, want, classifyStepCall(call, result, stepClassifyCtx{}))
				result.Result = fantasy.ToolResultOutputContentText{Text: "success"}
				require.Equal(t, stepCallAct, classifyStepCall(call, result, stepClassifyCtx{}))
				result.Result = nil
				require.Equal(t, stepCallAct, classifyStepCall(call, result, stepClassifyCtx{}))
			})
		}
	}
	for _, metadata := range []string{
		``, `null`, `{`, `{"edit_effect":null}`, `{"edit_effect":{}}`,
		`{"edit_effect":{"no_write":false,"reason":"unchanged_content"}}`,
		`{"edit_effect":{"no_write":"true","reason":"unchanged_content"}}`,
		`{"edit_effect":{"no_write":true,"reason":42}}`,
	} {
		t.Run("malformed/"+metadata, func(t *testing.T) {
			t.Parallel()
			result := fantasy.ToolResultContent{ClientMetadata: metadata, Result: fantasy.ToolResultOutputContentError{Error: errors.New("no changes made")}}
			require.Equal(t, stepCallAct, classifyStepCall(fantasy.ToolCallContent{ToolName: tools.MultiEditToolName}, result, stepClassifyCtx{}))
		})
	}
	for _, tc := range []struct {
		name, metadata, message string
		want                    stepCallClass
	}{
		{"guard refusal", `{"progress_guard":{"refused":"reread"},"edit_effect":{"no_write":true,"reason":"unchanged_content"}}`, "refused", stepCallRefused},
		{"unknown tool", `{"edit_effect":{"no_write":true,"reason":"unchanged_content"}}`, "tool not found: multiedit", stepCallNeutral},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := fantasy.ToolResultContent{ClientMetadata: tc.metadata, Result: fantasy.ToolResultOutputContentError{Error: errors.New(tc.message)}}
			require.Equal(t, tc.want, classifyStepCall(fantasy.ToolCallContent{ToolName: tools.MultiEditToolName}, result, stepClassifyCtx{}))
		})
	}
}

func TestNativeVaryingNoWriteStepsStopWithoutSignatureLoop(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	for _, mode := range []string{"pure multiedit", "pure equal multiedit", "alternating", "read edit multiedit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte("alpha\nalpha\nbeta\n"), 0o600))
			edit := tools.NewEditTool(nil, nil, noWriteEditTracker{}, dir)
			multi := tools.NewMultiEditTool(nil, nil, noWriteEditTracker{}, dir)
			var progress turnProgress
			view := fantasy.ToolCallContent{ToolName: tools.ViewToolName, Input: fmt.Sprintf(`{"file_path":%q,"offset":0,"limit":10}`, path)}
			apply := func(call fantasy.ToolCallContent, result fantasy.ToolResultContent) stepCallClass {
				t.Helper()
				class := classifyStepCall(call, result, stepClassifyCtx{seen: progress.seen.snapshot()})
				progress.applyStep([]stepCallClass{class}, [][]namedSpan{readSpans(call)})
				return class
			}
			require.Equal(t, stepCallRead, apply(view, fantasy.ToolResultContent{}))
			signatures := make(map[string]bool)
			for i := range inTurnStopAfter {
				if mode == "read edit multiedit" && i == 1 {
					require.Equal(t, stepCallReread, apply(view, fantasy.ToolResultContent{}))
				} else {
					var call fantasy.ToolCallContent
					var result fantasy.ToolResultContent
					old, replacement := fmt.Sprintf("missing-%d", i), fmt.Sprintf("replacement-%d", i)
					if mode == "pure equal multiedit" {
						edits := make([]tools.MultiEditOperation, i+1)
						for j := range edits {
							edits[j] = tools.MultiEditOperation{OldString: "beta", NewString: "beta"}
						}
						call, result = nativeGuardEffectResult(t, multi, tools.MultiEditParams{FilePath: path, Edits: edits})
					} else if mode != "pure multiedit" && i%2 == 0 && !(mode == "read edit multiedit" && i == 0) {
						if i == 2 {
							old = "alpha" // Multiple matches still reject changed replacements.
						}
						call, result = nativeGuardEffectResult(t, edit, tools.EditParams{FilePath: path, OldString: old, NewString: replacement})
					} else {
						call, result = nativeGuardEffectResult(t, multi, tools.MultiEditParams{FilePath: path, Edits: []tools.MultiEditOperation{{OldString: old, NewString: replacement}}})
					}
					signature := call.ToolName + call.Input
					require.False(t, signatures[signature], "the progress guard must not depend on repeated exact arguments")
					signatures[signature] = true
					require.Equal(t, stepCallNoWrite, apply(call, result))
				}
				require.Equal(t, i+1, progress.streak)
				require.Equal(t, i+1 == inTurnStopAfter, progress.stopped)
				require.True(t, progress.seen.covered(path, readSpan{start: 0, end: 10}))
			}
			expectedCount := inTurnStopAfter
			if mode == "read edit multiedit" {
				expectedCount--
			}
			require.Equal(t, expectedCount, progress.noWrites)
			require.Contains(t, guardStopDetails(progress), fmt.Sprintf("%d native edit/multiedit call(s) made no file content write", expectedCount))
			require.True(t, IsInTurnGuardStop(InTurnGuardStopTitle+"\n"+guardStopDetails(progress)))
			require.True(t, stepGuardFor(&progress, false, false).refuseRereadCall(readSpans(view)))
		})
	}
}

func TestMultipleNativeNoWritesCountOncePerStepAndFreshReadWins(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))
	editCall, editResult := nativeGuardEffectResult(t, tools.NewEditTool(nil, nil, noWriteEditTracker{}, dir), tools.EditParams{FilePath: path, OldString: "alpha", NewString: "alpha"})
	multiCall, multiResult := nativeGuardEffectResult(t, tools.NewMultiEditTool(nil, nil, noWriteEditTracker{}, dir), tools.MultiEditParams{FilePath: path, Edits: []tools.MultiEditOperation{{OldString: "missing", NewString: "new"}}})
	classes := []stepCallClass{classifyStepCall(editCall, editResult, stepClassifyCtx{}), classifyStepCall(multiCall, multiResult, stepClassifyCtx{})}
	var progress turnProgress
	progress.applyStep(classes, [][]namedSpan{nil, nil})
	require.Equal(t, 1, progress.streak)
	require.Equal(t, 2, progress.noWrites)
	progress.applyStep(classes, [][]namedSpan{nil, nil})
	require.Equal(t, 2, progress.streak)
	require.False(t, progress.stopped)
	view := fantasy.ToolCallContent{ToolName: tools.ViewToolName, Input: fmt.Sprintf(`{"file_path":%q,"offset":10,"limit":10}`, path)}
	fresh := classifyStepCall(view, fantasy.ToolResultContent{}, stepClassifyCtx{seen: progress.seen.snapshot()})
	require.Equal(t, stepCallRead, fresh)
	progress.applyStep(append(classes, fresh), [][]namedSpan{nil, nil, readSpans(view)})
	require.Zero(t, progress.streak)
	require.True(t, progress.seen.covered(path, readSpan{start: 10, end: 20}))
}

type nativeEffectRefreshTracker struct {
	noWriteEditTracker
	lastRead, refreshed time.Time
	reads               int
}

func (t *nativeEffectRefreshTracker) LastReadTime(context.Context, string, string) time.Time {
	return t.lastRead
}

func (t *nativeEffectRefreshTracker) RecordRead(context.Context, string, string) {
	t.lastRead = t.refreshed
	t.reads++
}

func TestNativeExternalRefreshThroughFantasyPermitsFreshRead(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	for _, name := range []string{tools.EditToolName, tools.MultiEditToolName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))
			mtime := time.Unix(1700000010, 0)
			require.NoError(t, os.Chtimes(path, mtime, mtime))
			tracker := &nativeEffectRefreshTracker{lastRead: mtime.Add(-time.Hour), refreshed: mtime.Add(time.Second)}
			var tool fantasy.AgentTool
			var params any
			if name == tools.EditToolName {
				tool = tools.NewEditTool(nil, nil, tracker, dir)
				params = tools.EditParams{FilePath: path, OldString: "alpha", NewString: "alpha"}
			} else {
				tool = tools.NewMultiEditTool(nil, nil, tracker, dir)
				params = tools.MultiEditParams{FilePath: path, Edits: []tools.MultiEditOperation{{OldString: "alpha", NewString: "alpha"}}}
			}
			progress := turnProgress{streak: 3, seen: readCoverage{}}
			progress.seen.add(path, readSpan{start: 0, end: 10})
			call, result := nativeGuardEffectResult(t, tool, params)
			require.NotContains(t, result.ClientMetadata, "edit_effect")
			require.Equal(t, fantasy.ToolResultContentTypeError, result.Result.GetType())
			class := classifyStepCall(call, result, stepClassifyCtx{seen: progress.seen.snapshot()})
			require.Equal(t, stepCallAct, class)
			progress.applyStep([]stepCallClass{stepCallNoWrite, class}, [][]namedSpan{nil, nil})
			require.Zero(t, progress.streak, "external refresh must outrank a no-write in the same step")
			require.Empty(t, progress.seen)
			view := fantasy.ToolCallContent{ToolName: tools.ViewToolName, Input: fmt.Sprintf(`{"file_path":%q,"offset":0,"limit":10}`, path)}
			require.Equal(t, stepCallRead, classifyStepCall(view, fantasy.ToolResultContent{}, stepClassifyCtx{seen: progress.seen.snapshot()}))
			require.False(t, stepGuardFor(&progress, false, false).refuseRereadCall(readSpans(view)))
			call, result = nativeGuardEffectResult(t, tool, params)
			require.Equal(t, stepCallNoWrite, classifyStepCall(call, result, stepClassifyCtx{}), "a later unchanged call should certify after refresh")
			require.Equal(t, 1, tracker.reads)
		})
	}
}

type nativeEffectPermissions struct{ permission.Service }

func (nativeEffectPermissions) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

type nativeEffectHistory struct{ history.Service }

func (nativeEffectHistory) GetByPathAndSession(context.Context, string, string) (history.File, error) {
	return history.File{Content: "alpha\n"}, nil
}

func (nativeEffectHistory) CreateVersion(context.Context, string, string, string) (history.File, error) {
	return history.File{}, nil
}

func TestNativeChangedPartialWriteThroughFantasyWinsMixedStep(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))
	tool := tools.NewMultiEditTool(nativeEffectPermissions{}, nativeEffectHistory{}, noWriteEditTracker{}, dir)
	call, result := nativeGuardEffectResult(t, tool, tools.MultiEditParams{FilePath: path, Edits: []tools.MultiEditOperation{
		{OldString: "alpha", NewString: "beta"}, {OldString: "missing", NewString: "bad"},
	}})
	require.Equal(t, fantasy.ToolResultContentTypeText, result.Result.GetType())
	require.NotContains(t, result.ClientMetadata, "edit_effect")
	var metadata tools.MultiEditResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(result.ClientMetadata), &metadata))
	require.Equal(t, 1, metadata.EditsApplied)
	require.Len(t, metadata.EditsFailed, 1)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "beta\n", string(content))
	class := classifyStepCall(call, result, stepClassifyCtx{})
	require.Equal(t, stepCallAct, class)
	progress := turnProgress{streak: inTurnStopAfter - 1, seen: readCoverage{}}
	progress.seen.add(path, readSpan{start: 0, end: 10})
	progress.applyStep([]stepCallClass{stepCallNoWrite, class}, [][]namedSpan{nil, nil})
	require.Zero(t, progress.streak)
	require.False(t, progress.stopped)
	require.Empty(t, progress.seen, "actual writes must invalidate coverage despite failed edits in the same call/step")
}
