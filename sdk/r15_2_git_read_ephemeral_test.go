package sdk_test

// Regression test for R15-2 (P1, SDK review round 15, task #891):
// before the fix, an ephemeral sdk.ModeLibrary session (no WorkingDir)
// still offered the model the git_read tool. The tool runs the real
// `git` binary as an OS subprocess with cmd.Dir set to the session's
// working directory (internal/agent/tools/git_read.go), which for an
// ephemeral session is the synthetic LibraryVirtualRoot sentinel -- a
// real, OS-interpreted host path (e.g. "K:\\rush-library-mode-root" on
// Windows). A git repository that happened to exist at that sentinel
// path would therefore have its status/diff/log/show/blame data read
// and sent to the external model provider inside the tool result,
// contradicting sdk/README.md's "NO real-disk" promise. The
// rejectRealDiskUnderLibraryVirtualRoot guard does not cover this tool:
// git_read never goes through a DiskProvider at all.
//
// The fix is layered: git_read joined noRealWorkspaceForbiddenTools
// (internal/agent/coordinator_tools.go) and
// libraryEphemeralDisabledTools (sdk/library_mode.go).
//
// Like the R6-1 download, R14-4 agentic_fetch, and R15-1 rush_logs
// regressions, git_read has NO opt-in path once removed from the
// ephemeral toolset, so the test asserts absence from the ACTUAL offered
// tool schema and a "tool not found"-shaped refusal on a direct call
// attempt. Planting a real git repository under the sentinel path is
// not required here: the sentinel is a synthetic path that will not
// exist on a CI runner, and schema absence plus the refusal shape is
// exactly how R14-4 and R15-1 prove the same absence.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKLibraryModeEphemeralGitReadAttemptNeverRunsHostGit mirrors
// TestSDKLibraryModeEphemeralRushLogsAttemptNeverReadsHostFile
// (sdk/r15_1_rush_logs_ephemeral_test.go) for git_read.
func TestSDKLibraryModeEphemeralGitReadAttemptNeverRunsHostGit(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const marker2 = "R15_2_GIT_READ_DENIED_OK"

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
		case bytes.Contains(body, []byte(`"call_git"`)):
			// The tool-result round-trip request: reply with the
			// marker so the turn ends after the refused call.
			sseChunks(t, w, []map[string]any{textChunk("probe", marker2), finishChunk("probe", "stop")})
		default:
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", "call_git", tools.GitReadToolName, map[string]any{
					"operation": "status",
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

	const sessionID = "sdk-r15-2-git-read-denied"
	var buf bytes.Buffer
	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "attempt git_read",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            &buf,
		HideSpinner:       true,
	})
	require.NoError(t, err, "output %q", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "error=%q warnings=%v", res.Error, res.Warnings)
	require.Equal(t, marker2, res.FinalText)

	// The offered tool schema must not contain git_read.
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
	require.NotContains(t, names, tools.GitReadToolName,
		"an ephemeral library-mode session must never offer %q by default", tools.GitReadToolName)

	// Positive control: a non-disk tool the default set still offers,
	// proving the filter is targeted rather than an accidental empty
	// toolset.
	require.Contains(t, names, "agent",
		"the delegation tool is not a real-disk tool and must still be offered")

	// The direct call attempt must be refused as not-found, never
	// executed as a real `git` subprocess.
	msgs, err := client.Messages(context.Background(), sessionID)
	require.NoError(t, err)
	gitResult := fsToolResultOf(t, msgs, tools.GitReadToolName)
	require.True(t, gitResult.IsError, "content %q", gitResult.Content)
	require.Contains(t, gitResult.Content, "tool not found",
		"an unoffered tool must be refused as not-found, never executed")
}
