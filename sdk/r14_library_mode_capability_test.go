package sdk_test

// Regression tests for R14-1/R14-2/R14-3 (SDK review round 14,
// 2026-09-03).
//
// R14-1: an ephemeral sdk.ModeLibrary session's config carries
// Options.NoRealWorkspace = true. internal/agent's buildTools now
// applies a FINAL floor (applyNoRealWorkspaceToolFloor) AFTER worker
// sub-agent toolset layering (which appends workerToolNames
// edit/multiedit/write/bash/todos/download/fetch/ask_question to a
// sub-agent) and AFTER folder-scope re-adds, stripping
// bash/run_command/download/edit/multiedit/write/view/glob/grep/ls
// always, plus the whole fs_* family unless the call carries a custom
// DiskProvider. So a worker sub-agent delegated to via the "agent"
// tool must not see write/bash/download in its ACTUAL tool schema even
// though the worker layering asked for them.
//
// R14-2: ExecuteRun hard-refuses any Run on an ephemeral client with
// RunOverrides.FolderScopes non-empty and DiskProvider nil BEFORE any
// provider traffic -- even when the scope Dir is an absolute real host
// directory (the older sentinel guard only caught relative/lexical
// sentinel paths with the "refusing real-disk access" error text).
//
// R14-3: sdk.LibraryVirtualRoot is now a function, not an assignable
// var. The old exported var was a plain init-time COPY of the internal
// value and could be legally reassigned, diverging the SDK-side root
// from the internal guard's root. The real correctness proof is
// compile-time ("sdk.LibraryVirtualRoot = \"x\"" no longer compiles);
// TestSDKLibraryVirtualRootStableAndAbsolute pins the runtime
// contract: concurrent calls return one identical, non-empty,
// platform-natively absolute value.

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

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// r14ToolNamesFromBody decodes one captured request body's offered
// tool names WITHOUT *testing.T, so it is safe to call from an
// httptest handler goroutine (no t.Fatal across goroutines, no races).
// Returns nil for a body with no "tools" field at all.
func r14ToolNamesFromBody(body []byte) []string {
	var req r6_1ToolRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

// TestSDKLibraryModeEphemeralWorkerSubAgentToolsetFloored is R14-1's
// headline regression. The top-level ephemeral coder (which still
// legitimately carries the "agent" delegation tool) delegates to a
// worker sub-agent. The worker layering appends
// edit/multiedit/write/bash/todos/download/fetch/ask_question, but the
// R14-1 no-real-workspace floor must strip the host-disk/command tools
// from the worker's FINAL tool schema too. The fake provider drives
// three worker turns: the model asks for "write", then "bash", then
// "download" against a caller-chosen absolute host path. Each attempt
// must be refused (tool not offered -> tool not found) and nothing may
// ever be created on the real disk.
func TestSDKLibraryModeEphemeralWorkerSubAgentToolsetFloored(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		marker      = "R14_1_WORKER_FLOOR_OK"
		sessionID   = "sdk-r14-worker-floor"
		delegateSig = `"call_delegate"`
	)
	hostTarget := filepath.Join(t.TempDir(), "worker-should-never-create.txt")
	hostDir := filepath.Dir(hostTarget)

	var (
		mu           sync.Mutex
		workerTurn   int
		workerTurns  int
		firstWorker  []string
		firstTopLvl  []string
		topLevelSeen bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("Generate a concise title")) {
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
			return
		}
		names := r14ToolNamesFromBody(body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case containsName(names, "agent"):
			// Top-level coder turn. The first one issues the
			// delegation; once the delegation result is in the
			// transcript it finishes with the marker.
			if !topLevelSeen {
				topLevelSeen = true
				firstTopLvl = names
				sseChunks(t, w, []map[string]any{
					toolCallChunkNamed("probe", "call_delegate", "agent", map[string]any{
						"prompt": "attempt a write, a bash command, and a download to an absolute host path",
					}),
					finishChunk("probe", "tool_calls"),
				})
				return
			}
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		case len(names) == 0:
			// Defensive: an unexpected empty-toolset request just
			// finishes the turn.
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		default:
			// The worker sub-agent: worker toolsets never contain
			// "agent" (that exclusion is the recursion guard).
			workerTurns++
			if workerTurns == 1 {
				firstWorker = names
			}
			workerTurn = workerTurns
			switch workerTurn {
			case 1:
				sseChunks(t, w, []map[string]any{
					toolCallChunkNamed("probe", "call_w", "write", map[string]any{
						"file_path": hostTarget, "content": "SHOULD_NEVER_BE_WRITTEN",
					}),
					finishChunk("probe", "tool_calls"),
				})
			case 2:
				sseChunks(t, w, []map[string]any{
					toolCallChunkNamed("probe", "call_b", "bash", map[string]any{
						"command": "echo should-never-run", "description": "R14-1 probe",
					}),
					finishChunk("probe", "tool_calls"),
				})
			case 3:
				sseChunks(t, w, []map[string]any{
					toolCallChunkNamed("probe", "call_d", "download", map[string]any{
						"url": "https://example.invalid/payload", "file_path": hostTarget,
					}),
					finishChunk("probe", "tool_calls"),
				})
			default:
				sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
			}
		}
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-r14-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "delegate the write to your worker",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	mu.Lock()
	turns, workerNames, topNames := workerTurns, firstWorker, firstTopLvl
	mu.Unlock()
	require.GreaterOrEqual(t, turns, 4, "the worker must have run all three denied turns plus its final text turn")
	require.NotNil(t, workerNames, "no worker request was captured")
	require.NotNil(t, topNames, "no top-level request was captured")

	// Positive control that this is really the worker build (worker
	// layering ran) and not a trivially empty toolset.
	require.True(t, containsName(workerNames, "todos") || containsName(workerNames, "ask_question"),
		"worker toolset must contain the layered non-disk tools, got %v", workerNames)

	// The R14-1 floor: none of the host-disk/command tools the worker
	// layering asked for (or folder-scope re-adds might bring back) may
	// survive into the worker's final schema.
	for _, dangerous := range []string{
		"write", "bash", "download", "edit", "multiedit", "run_command",
		"view", "glob", "grep", "ls",
		"fs_read", "fs_write", "fs_list", "fs_delete",
	} {
		require.NotContains(t, workerNames, dangerous,
			"the no-real-workspace floor must strip %q from a worker sub-agent of an ephemeral session", dangerous)
	}

	// Positive control on the top level: the delegation tool must still
	// be offered there.
	require.Contains(t, topNames, "agent",
		"the top-level ephemeral coder must still offer the delegation tool")

	// The write attempt never reached the real disk, and the temp dir
	// hosting the target is completely empty.
	_, statErr := os.Stat(hostTarget)
	require.True(t, os.IsNotExist(statErr),
		"the worker write attempt must never reach the real disk (%s)", hostTarget)
	entries, readErr := os.ReadDir(hostDir)
	require.NoError(t, readErr)
	require.Empty(t, entries, "nothing may be created in the target's directory: %v", entries)
}

// containsName reports whether names contains tool (tiny local helper
// so handler-goroutine code needs no testify access).
func containsName(names []string, tool string) bool {
	for _, n := range names {
		if n == tool {
			return true
		}
	}
	return false
}

// TestSDKLibraryModeEphemeralAbsoluteFolderScopeWithoutDiskProviderFailsClosed
// is R14-2's regression: the no-real-workspace precondition now fires
// BEFORE any canonicalization, so even an ABSOLUTE, real, pre-existing
// host directory used as a FolderScope Dir without a DiskProvider is
// refused up front -- with the new precondition error text, zero
// provider traffic, and the directory provably untouched.
func TestSDKLibraryModeEphemeralAbsoluteFolderScopeWithoutDiskProviderFailsClosed(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	dir := evalTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("UNTOUCHED_BY_R14_2"), 0o644))
	snapshotDir := func(t *testing.T) map[string]string {
		t.Helper()
		out := map[string]string{}
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)
			out[e.Name()] = string(data)
		}
		return out
	}
	before := snapshotDir(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		sseChunks(t, w, []map[string]any{textChunk("probe", "R14_2_ABSOLUTE_SCOPE_DENIED_OK"), finishChunk("probe", "stop")})
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-r14-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:      "this must never reach the provider",
		Mode:        sdk.RunModeJSON,
		HideSpinner: true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: dir, Ops: []sdk.FileOp{sdk.FileOpRead, sdk.FileOpCreate, sdk.FileOpOverwrite}},
			},
			// DiskProvider deliberately nil: the exact gap R14-2
			// closed for absolute host directories.
		},
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "require a custom DiskProvider",
		"the failure must come from the R14-2 no-real-workspace precondition")
	require.NotContains(t, err.Error(), "refusing real-disk access",
		"the new earlier precondition must fire, not the old sentinel guard")
	require.EqualValues(t, 0, hits.Load(), "the provider must not be contacted when this precondition fails")
	require.Equal(t, before, snapshotDir(t), "the scoped directory must be provably untouched")
}

// TestSDKLibraryVirtualRootStableAndAbsolute pins the R14-3 runtime
// contract. The real correctness proof is compile-time: the exported
// mutable var was a plain init-time COPY of the internal value and
// could be legally reassigned, diverging the SDK-side root from the
// internal guard's root -- R14-3 made LibraryVirtualRoot a function so
// assignment no longer compiles. At runtime the function must return
// one identical, non-empty, platform-natively absolute value from many
// concurrent goroutines.
func TestSDKLibraryVirtualRootStableAndAbsolute(t *testing.T) {
	first := sdk.LibraryVirtualRoot()
	require.NotEmpty(t, first)
	require.True(t, filepath.IsAbs(first),
		"LibraryVirtualRoot must satisfy the platform-native filepath.IsAbs, got %q", first)

	const goroutines = 16
	results := make([]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(slot int) {
			defer wg.Done()
			results[slot] = sdk.LibraryVirtualRoot()
		}(i)
	}
	wg.Wait()
	for i, v := range results {
		require.Equal(t, first, v, "goroutine %d saw a diverged LibraryVirtualRoot", i)
	}
}
