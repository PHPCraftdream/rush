package sdk_test

// Regression test for R14-4 (P2, SDK review round 14): agentic_fetch
// was missing from libraryEphemeralDisabledTools
// (sdk/library_mode.go), so an ephemeral sdk.ModeLibrary session -- the
// README's "NO real-disk or command-execution tools by default" shape
// -- still handed the model the agentic_fetch tool. When called, it
// builds its sub-agent's workspace with
// os.MkdirTemp(c.cfg.Config().Options.DataDirectory, "rush-fetch-*")
// (internal/agent/agentic_fetch_tool.go); for an ephemeral session
// Options.DataDirectory is "", and os.MkdirTemp("") falls back to the
// REAL OS temp directory -- real disk I/O behind an "ephemeral" label.
// folderScopeEscapeHatchTools (internal/agent/coordinator_tools.go)
// already classifies agentic_fetch as a filesystem escape hatch for
// exactly this reason; the ephemeral default filter just never picked
// that classification up (R6-1 added download the same way).
//
// Like the R6-1 download regression, agentic_fetch has NO opt-in path
// once removed: it is not in folderScopeOpForTool (so
// applyCallFolderScope only ever strips it, via
// folderScopeEscapeHatchTools), and it is not in workerToolNames (so
// the R14-1 worker toolset layering cannot re-add it either). The test
// below asserts absence from the ACTUAL offered tool schema, a "tool
// not found"-shaped refusal on a direct call attempt, and zero
// rush-fetch-* directories ever appearing in the process temp
// directory -- including TRANSIENT creations that the tool's
// defer os.RemoveAll would erase before Run returns (detected by
// re-scanning the temp dir inside the provider request handler, which
// runs while a sub-agent would sit between MkdirTemp and its cleanup).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKLibraryModeEphemeralAgenticFetchAttemptNeverTouchesRealDisk
// mirrors TestSDKLibraryModeEphemeralDownloadAttemptNeverTouchesRealDisk
// (sdk/r6_1_library_mode_diskless_test.go) for agentic_fetch.
func TestSDKLibraryModeEphemeralAgenticFetchAttemptNeverTouchesRealDisk(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	// Redirect the process temp directory into a scan dir we control,
	// so any os.MkdirTemp("", ...) fallback (what agentic_fetch hits
	// with an ephemeral session's empty Options.DataDirectory) lands
	// where the test can observe it. TMP/TEMP cover Go's os.TempDir on
	// Windows, TMPDIR on Unix.
	scanDir := t.TempDir()
	t.Setenv("TMP", scanDir)
	t.Setenv("TEMP", scanDir)
	t.Setenv("TMPDIR", scanDir)

	// rush-fetch-* hits seen so far, including transient ones observed
	// from inside the provider handler (which executes while an
	// agentic_fetch call would still be between MkdirTemp and its
	// deferred RemoveAll).
	var (
		hitsMu sync.Mutex
		hits   []string
	)
	scanForRushFetchDirs := func(where string) {
		entries, err := os.ReadDir(scanDir)
		if err != nil {
			return
		}
		hitsMu.Lock()
		defer hitsMu.Unlock()
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "rush-fetch-") {
				hits = append(hits, where+": "+e.Name())
			}
		}
	}

	const marker = "R14_4_AGENTIC_FETCH_DENIED_OK"

	var (
		bodiesMu sync.Mutex
		bodies   [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		scanForRushFetchDirs("provider-request")
		bodiesMu.Lock()
		bodies = append(bodies, body)
		bodiesMu.Unlock()
		switch {
		case bytes.Contains(body, []byte("Generate a concise title")):
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_fetch"`)):
			// The tool-result round-trip request: reply with the
			// marker so the turn ends after the refused call.
			sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_fetch", tools.AgenticFetchToolName, map[string]any{
					"url":    "https://example.invalid/payload",
					"prompt": "summarize this page",
				}),
				finishChunk("probe", "tool_calls"),
			})
		}
	}))
	t.Cleanup(srv.Close)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	const sessionID = "sdk-r14-4-agentic-fetch-denied"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "attempt agentic_fetch",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker, res.FinalText)

	// The offered tool schema must not contain agentic_fetch.
	mainBody := func() []byte {
		bodiesMu.Lock()
		defer bodiesMu.Unlock()
		for _, b := range bodies {
			if !bytes.Contains(b, []byte("Generate a concise title")) {
				return b
			}
		}
		t.Fatal("no captured request body outside of title generation")
		return nil
	}()
	names := r6_1ToolNamesFromBody(t, mainBody)
	require.NotEmpty(t, names, "the main turn must offer SOME tools (an empty toolset would trivially pass an absence check)")
	require.NotContains(t, names, tools.AgenticFetchToolName,
		"an ephemeral library-mode session must never offer %q by default", tools.AgenticFetchToolName)

	// Positive control: a non-disk tool the default set still offers,
	// proving the filter is targeted rather than an accidental empty
	// toolset.
	require.Contains(t, names, "agent",
		"the delegation tool is not a real-disk tool and must still be offered")

	// The direct call attempt must be refused as not-found, never
	// executed.
	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	fetchResult := fsToolResultOf(t, msgs, tools.AgenticFetchToolName)
	require.True(t, fetchResult.IsError, "content %q", fetchResult.Content)
	require.Contains(t, fetchResult.Content, "tool not found",
		"an unoffered tool must be refused as not-found, never executed")

	// Final sweep: nothing matching rush-fetch-* may exist under the
	// redirected temp dir.
	scanForRushFetchDirs("after-run")
	hitsMu.Lock()
	defer hitsMu.Unlock()
	require.Empty(t, hits,
		"an ephemeral session must never create rush-fetch-* temp directories on the real disk: %v", hits)
}
