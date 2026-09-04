package sdk_test

// Regression test for R15-1 (P0, SDK review round 15): before the fix,
// an ephemeral sdk.ModeLibrary session (no WorkingDir,
// Options.DataDirectory == "") still offered the model the rush_logs
// tool, whose log path is filepath.Join(Options.DataDirectory, "logs",
// "rush.log") (internal/agent/coordinator_tools.go) -- degrading to the
// RELATIVE "logs/rush.log" when DataDirectory is empty, which
// runRushLogs' plain os.Stat/os.Open (internal/agent/tools/rush_logs.go)
// resolves against the HOST process's current working directory. A host
// application that happened to have ./logs/rush.log in its cwd therefore
// had its log tail sent to the external model provider inside the tool
// result, contradicting sdk/README.md's "NO real-disk" promise.
//
// The fix is layered: rush_logs joined noRealWorkspaceForbiddenTools
// (internal/agent/coordinator_tools.go) and
// libraryEphemeralDisabledTools (sdk/library_mode.go), and buildTools
// now refuses to construct the tool at all when the resolved log path
// is empty (it would be a relative path, never a safe per-session one).
//
// Like the R6-1 download and R14-4 agentic_fetch regressions, rush_logs
// has NO opt-in path once removed from the ephemeral toolset, so the
// test asserts absence from the ACTUAL offered tool schema, a "tool
// not found"-shaped refusal on a direct call attempt, zero occurrences
// of a planted host log marker in ANY provider request body, and zero
// marker leakage into the session transcript's tool result.
//
// The planted log lines are JSON objects ({"msg": ...}): readLastLines
// silently skips non-JSON lines, so only JSON lines would survive into
// a tool result if the bug were present. formatLogEntry passes the msg
// value through verbatim (the marker contains no sensitive-key
// substrings), so pre-fix the marker would reach the provider round-trip.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKLibraryModeEphemeralRushLogsAttemptNeverReadsHostFile mirrors
// TestSDKLibraryModeEphemeralAgenticFetchAttemptNeverTouchesRealDisk
// (sdk/r14_4_agentic_fetch_ephemeral_test.go) for rush_logs.
func TestSDKLibraryModeEphemeralRushLogsAttemptNeverReadsHostFile(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	// Plant a real ./logs/rush.log relative to the process cwd, under a
	// temp cwd so the repo tree stays untouched and the relative-path
	// resolution is fully test-controlled. Pre-fix, runRushLogs would
	// resolve its empty-DataDirectory-relative "logs/rush.log" against
	// exactly this directory and tail the file below.
	const marker = "R15_1_RUSH_LOGS_HOST_MARKER"
	root := t.TempDir()
	t.Chdir(root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "logs"), 0o755))
	logContent := "{\"msg\":\"filler log line\"}\n{\"msg\":\"" + marker + "\"}\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "logs", "rush.log"),
		[]byte(logContent), 0o644))

	const marker2 = "R15_1_RUSH_LOGS_DENIED_OK"

	var (
		bodiesMu sync.Mutex
		bodies   [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, body)
		bodiesMu.Unlock()
		switch {
		case bytes.Contains(body, []byte("Generate a concise title")):
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
		case bytes.Contains(body, []byte(`"call_logs"`)):
			// The tool-result round-trip request: reply with the
			// marker so the turn ends after the refused call.
			sseChunks(t, w, []map[string]any{textChunk("probe", marker2), finishChunk("probe", "stop")})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_logs", tools.RushLogsToolName, map[string]any{
					"lines": 100,
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

	const sessionID = "sdk-r15-1-rush-logs-denied"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "attempt rush_logs",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker2, res.FinalText)

	// The offered tool schema must not contain rush_logs.
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
	require.NotContains(t, names, tools.RushLogsToolName,
		"an ephemeral library-mode session must never offer %q by default", tools.RushLogsToolName)

	// Positive control: a non-disk tool the default set still offers,
	// proving the filter is targeted rather than an accidental empty
	// toolset.
	require.Contains(t, names, "agent",
		"the delegation tool is not a real-disk tool and must still be offered")

	// No captured request body may contain the planted host log marker.
	// Pre-fix, the tool executed against the host cwd's logs/rush.log
	// and the tool-result round-trip body carried the marker to the
	// provider.
	bodiesMu.Lock()
	captured := bodies
	bodiesMu.Unlock()
	require.NotEmpty(t, captured, "the provider must have received at least one request")
	for i, b := range captured {
		require.NotContains(t, string(b), marker,
			"captured provider request #%d leaked the host log marker", i)
	}

	// The direct call attempt must be refused as not-found, never
	// executed, and must not leak the marker either.
	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	logsResult := fsToolResultOf(t, msgs, tools.RushLogsToolName)
	require.True(t, logsResult.IsError, "content %q", logsResult.Content)
	require.Contains(t, logsResult.Content, "tool not found",
		"an unoffered tool must be refused as not-found, never executed")
	require.NotContains(t, logsResult.Content, marker,
		"the refused tool result must not leak the host log marker")
}
