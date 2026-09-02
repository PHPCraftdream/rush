package sdk_test

// SDK-level tests for RunOverrides.FolderScopes (scoped-file-tools task
// T10). Each test drives a real sdk.Client against a scripted
// openai-compat provider (httptest SSE server, the
// sdk_concurrent_policy_test.go pattern) and asserts on REAL tool
// results and REAL files on disk:
//
//   - a scoped run's fs_read batch: the in-scope item succeeds while the
//     out-of-scope sibling is denied PER ITEM (the batch is not a
//     whole-call error and the turn continues to a clean end_turn);
//   - a scoped run and an unscoped run proceed concurrently on two
//     DIFFERENT sessions with no cross-contamination in either
//     direction (the scoped session is scope-checked per item; the
//     unscoped session keeps its legacy tools and is unrestricted);
//   - RestrictedRun + FolderScopes + zero AllowBash/AllowTools: a file
//     write the scope grants SUCCEEDS — pinning the mandatory footgun
//     fix that appends the granted fs_* names to the restricted-run
//     AllowTools table (an empty table denies every plain tool, so
//     without the append this exact scenario denies the write);
//   - a malformed scope entry makes Run fail with "invalid folder
//     scopes" BEFORE any provider traffic.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// toolCallChunkNamed emits one tool-call chunk for an arbitrary tool
// name with arbitrary JSON arguments (the shared toolCallChunk helper
// is bash-specific).
func toolCallChunkNamed(model, callID, toolName string, args any) map[string]any {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index":    0,
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": toolName, "arguments": string(b)},
			}}},
			"finish_reason": nil,
		}},
	}
}

// fsRoundServer is a scripted two-round provider: a request whose
// history carries no "call_1" yet (round 1) is answered with one tool
// call; every later round is answered with the marker text and stops.
// requests counts every request the server saw, for provider-traffic
// assertions.
type fsRoundServer struct {
	srv      *httptest.Server
	requests atomic.Int64
}

func newFSRoundServer(t *testing.T, toolName string, args any, marker string) *fsRoundServer {
	t.Helper()
	fs := &fsRoundServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.requests.Add(1)
		if bytes.Contains(body, []byte(`"call_1"`)) {
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
			return
		}
		sseChunks(t, w, []map[string]any{
			toolCallChunkNamed("probe", "call_1", toolName, args),
			finishChunk("probe", "tool_calls"),
		})
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

// evalTempDir returns the test working directory with symlinks resolved
// (t.TempDir can sit behind a symlink on some CI runners; the scope
// matcher compares resolved paths, so every path handed to it must come
// from the same resolved root).
func evalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// fsToolResultOf returns the single tool result named toolName from the
// session history, failing the test when there is not exactly one.
func fsToolResultOf(t *testing.T, msgs []sdk.Message, toolName string) message.ToolResult {
	t.Helper()
	var hits []message.ToolResult
	for _, tr := range toolResultsOf(msgs) {
		if tr.Name == toolName {
			hits = append(hits, tr)
		}
	}
	require.Len(t, hits, 1, "expected exactly one %s tool result", toolName)
	return hits[0]
}

// TestSDKRunScopedBatchDeniesOutOfScopePerItem: a scoped run's fs_read
// batch must deny the out-of-scope item per item (with the matcher's own
// reason) while the in-scope sibling succeeds, the mixed batch must NOT
// be a whole-call error, and the turn must continue to a clean end_turn.
func TestSDKRunScopedBatchDeniesOutOfScopePerItem(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "scoped", "in_scope.txt"), []byte("IN_SCOPE_SENTINEL_10a\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "out_of_scope.txt"), []byte("OUT_OF_SCOPE_SENTINEL_10b\n"), 0o644))

	srv := newFSRoundServer(t, "fs_read", map[string]any{"items": []map[string]any{
		{"path": "scoped/in_scope.txt"},
		{"path": "out_of_scope.txt"},
	}}, "SCOPED_BATCH_OK")
	writeProbeRushJSON(t, workDir, srv.srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-scoped-batch"
	seedTenantSession(t, workDir, sessionID)

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "read both files",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpRead}},
			},
		},
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, "SCOPED_BATCH_OK", res.FinalText)

	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	toolResult := fsToolResultOf(t, msgs, "fs_read")
	require.False(t, toolResult.IsError, "a batch with one success is not a whole-call error: %q", toolResult.Content)
	require.Contains(t, toolResult.Content, "fs_read: 1 of 2 items ok")
	require.Contains(t, toolResult.Content, "denied")
	require.Contains(t, toolResult.Content, "outside every folder scope")
	require.Contains(t, toolResult.Content, "IN_SCOPE_SENTINEL_10a", "the in-scope read must carry the file content")
	require.NotContains(t, toolResult.Content, "OUT_OF_SCOPE_SENTINEL_10b", "the denied item must not leak content")

	// Read-only run: both files unchanged on disk.
	inScope, err := os.ReadFile(filepath.Join(workDir, "scoped", "in_scope.txt"))
	require.NoError(t, err)
	require.Equal(t, "IN_SCOPE_SENTINEL_10a\n", string(inScope))
	require.EqualValues(t, 2, srv.requests.Load(), "exactly one tool round plus one final round expected")
}

// TestSDKRunScopedAndUnscopedConcurrentSessionsIsolated mirrors
// TestSDKConcurrentRunsPerCallPolicyIsolation: a scoped run and an
// UNSCOPED run proceed concurrently on two DIFFERENT sessions of one
// client with no cross-contamination. The scoped session's fs_read is
// scope-checked per item; the unscoped session still has its legacy
// `view` tool and reads a file INSIDE the other call's scope unrestricted
// — proving neither the scope leaked onto the unscoped call nor the
// scope's absence leaked onto the scoped one.
func TestSDKRunScopedAndUnscopedConcurrentSessionsIsolated(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	targetFile := filepath.Join(workDir, "scoped", "shared_target.txt")
	require.NoError(t, os.WriteFile(targetFile, []byte("ISOLATION_TARGET_10c\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "outside.txt"), []byte("OUTSIDE_ISO_10d\n"), 0o644))

	const (
		sessionScoped   = "sdk-scope-iso-scoped"
		sessionUnscoped = "sdk-scope-iso-unscoped"
		scopedMarker    = "SCOPED_ISO_OK"
		unscopedMarker  = "UNSCOPED_ISO_OK"
	)

	// One provider serves BOTH sessions (a single rush.json can only
	// name one base URL): round 1 is dispatched by the tool the model
	// calls — fs_read for the scoped session, view for the unscoped one.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits.Add(1)
		if bytes.Contains(body, []byte(`"call_1"`)) {
			marker := scopedMarker
			if bytes.Contains(body, []byte(`"unscoped probe"`)) {
				marker = unscopedMarker
			}
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
			return
		}
		toolName, args := "fs_read", any(map[string]any{"items": []map[string]any{
			{"path": "scoped/shared_target.txt"},
			{"path": "outside.txt"},
		}})
		if bytes.Contains(body, []byte(`"unscoped probe"`)) {
			toolName, args = "view", any(map[string]any{"file_path": "scoped/shared_target.txt"})
		}
		sseChunks(t, w, []map[string]any{
			toolCallChunkNamed("probe", "call_1", toolName, args),
			finishChunk("probe", "tool_calls"),
		})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	seedTenantSession(t, workDir, sessionScoped)
	seedTenantSession(t, workDir, sessionUnscoped)

	var (
		wg      sync.WaitGroup
		startCh = make(chan struct{})
		resS    *sdk.RunResult
		resU    *sdk.RunResult
		errS    error
		errU    error
		bufS    bytes.Buffer
		bufU    bytes.Buffer
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		resS, errS = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "scoped probe",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionScoped,
			Stdout:            &bufS,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				FolderScopes: []sdk.FolderScope{
					{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpRead}},
				},
			},
		})
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resU, errU = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "unscoped probe",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionUnscoped,
			Stdout:            &bufU,
			HideSpinner:       true,
		})
	}()
	close(startCh)
	wg.Wait()

	require.NotErrorIs(t, errS, agent.ErrSessionBusy, "scoped output %q", bufS.String())
	require.NotErrorIs(t, errU, agent.ErrSessionBusy, "unscoped output %q", bufU.String())
	require.NoError(t, errS, "scoped output %q", bufS.String())
	require.NoError(t, errU, "unscoped output %q", bufU.String())
	require.Equal(t, "end_turn", resS.ExitReason)
	require.Equal(t, "end_turn", resU.ExitReason)
	require.Equal(t, scopedMarker, resS.FinalText)
	require.Equal(t, unscopedMarker, resU.FinalText)

	// Scoped session: per-item enforcement held.
	msgsS, err := client.Messages(context.Background(), sessionScoped)
	require.NoError(t, err)
	scopedResult := fsToolResultOf(t, msgsS, "fs_read")
	require.False(t, scopedResult.IsError, "content %q", scopedResult.Content)
	require.Contains(t, scopedResult.Content, "fs_read: 1 of 2 items ok")
	require.Contains(t, scopedResult.Content, "outside every folder scope")
	require.Contains(t, scopedResult.Content, "ISOLATION_TARGET_10c")
	require.NotContains(t, scopedResult.Content, "OUTSIDE_ISO_10d")

	// Unscoped session: legacy toolset intact and unrestricted — the view
	// succeeded on a path INSIDE the other call's scope. If the scoped
	// call's FolderScope had leaked onto this call, `view` would have
	// been stripped from the toolset and this result would not exist.
	msgsU, err := client.Messages(context.Background(), sessionUnscoped)
	require.NoError(t, err)
	unscopedResult := fsToolResultOf(t, msgsU, "view")
	require.False(t, unscopedResult.IsError, "content %q", unscopedResult.Content)
	require.Contains(t, unscopedResult.Content, "ISOLATION_TARGET_10c")

	// The single shared provider served both runs: one tool round plus
	// one final round per session.
	require.EqualValues(t, 4, hits.Load())
}

// TestSDKRunRestrictedRunWithFolderScopesGrantsScopedWrite pins the
// mandatory restricted-run companion of FolderScopes: under
// RestrictedRun with ZERO AllowTools/AllowBash patterns, a file write
// the scope grants must SUCCEED (the granted fs_* names are appended to
// the run allowlist's AllowTools table, whose empty state would
// otherwise deny every plain tool on the first call). The on-disk
// assertion is the discriminator: before the append existed, the
// whole-call permission request was denied and this file never appeared.
func TestSDKRunRestrictedRunWithFolderScopesGrantsScopedWrite(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "scoped"), 0o755))
	createdPath := filepath.Join(workDir, "scoped", "created_under_restriction.txt")
	const fileBody = "RESTRICTED_SCOPED_WRITE_OK"

	srv := newFSRoundServer(t, "fs_write", map[string]any{"items": []map[string]any{
		{"path": "scoped/created_under_restriction.txt", "content": fileBody, "create_only": true},
	}}, "RESTRICTED_SCOPED_OK")
	writeProbeRushJSON(t, workDir, srv.srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-restricted-scoped"
	seedTenantSession(t, workDir, sessionID)

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "create the file",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			RestrictedRun: true,
			// Deliberately NO AllowTools and NO AllowBash: the empty
			// restricted-run tables are exactly the footgun this
			// scenario proves fixed.
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
		},
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, "RESTRICTED_SCOPED_OK", res.FinalText)

	// The scoped write really landed on disk under RestrictedRun.
	got, err := os.ReadFile(createdPath)
	require.NoError(t, err, "the scoped write must land on disk even under RestrictedRun")
	require.Contains(t, string(got), fileBody)

	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	toolResult := fsToolResultOf(t, msgs, "fs_write")
	require.False(t, toolResult.IsError, "content %q", toolResult.Content)
	require.Contains(t, toolResult.Content, "fs_write: 1 of 1 items ok")
	require.Contains(t, toolResult.Content, "create")
	require.NotContains(t, toolResult.Content, "denied")
	require.NotContains(t, toolResult.Content, "User denied permission")
}

// TestSDKRunInvalidFolderScopeEntryFailsBeforeProviderTraffic: one
// malformed entry (an unknown operation) fails the WHOLE run with an
// "invalid folder scopes" error before the provider is contacted — the
// deliberate asymmetry with the run-allowlist's log-and-drop behaviour.
func TestSDKRunInvalidFolderScopeEntryFailsBeforeProviderTraffic(t *testing.T) {
	isolateGlobalConfig(t)

	workDir := evalTempDir(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		sseChunks(t, w, []map[string]any{textChunk("probe", "MUST_NOT_BE_HIT"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	const sessionID = "sdk-invalid-scope"
	seedTenantSession(t, workDir, sessionID)

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "this must never reach the provider",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOp("readd")}}, // typo'd op
			},
		},
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "invalid folder scopes")
	require.Contains(t, err.Error(), `unknown operation "readd"`)
	require.EqualValues(t, 0, hits.Load(), "the provider must not be contacted when the scope fails to compile")
}
