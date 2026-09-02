package app

// R5-2 regression (P0 security review,
// docs/reviews/2026-09-02-sdk-library-review-round-5-2331.md): end-to-end
// proof, through the real ExecuteRun/fs_read production path, of the
// review's concrete bypass scenario. WorkingDir grants read; a nested
// entry is a deny carve-out (empty Ops); that nested entry's directory is
// an ACTUAL os.Symlink pointing at a sibling "private" directory. Before
// the fix, permission.BuildFolderScope compiled the carve-out's root from
// its lexical spelling only (never resolving the symlink), while
// resolveScopedPath symlink-resolved the model's requested item path
// before the matcher ever saw it -- so the resolved item path no longer
// fell under the (unresolved) carve-out root and matched the broader
// WorkingDir grant instead, silently leaking the "private" file's content
// back to the model. This test drives that exact call through the app's
// real ExecuteRun so it also proves the production wiring (app_run.go's
// scope compilation) applies the fix, not just the underlying helper.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunSymlinkedDenyCarveOutUnderBroaderGrantStillDenies(t *testing.T) {
	var toolResultBody string

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"tr1"`):
			// Follow-up round: the fs_read result (denied or, pre-fix,
			// leaked) is in the request body now. Checked FIRST because
			// the original prompt (and its marker) stays in message
			// history and is resent on every round.
			toolResultBody = string(body)
			admissionWriteSSE(w, []string{
				admissionSSEText("c2", "DONE"),
				admissionSSEStop("c2", "stop"),
			})
		case strings.Contains(string(body), "SYMLINK_CARVEOUT_TURN_MARKER"):
			// Ask the model's turn to read a path that lexically sits
			// under the deny-carved "alias" entry.
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("c1", "tr1", "fs_read", `{"items":[{"path":"alias/key.txt"}]}`),
				admissionSSEStop("c1", "tool_calls"),
			})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEText("c0", "IGNORED"),
				admissionSSEStop("c0", "stop"),
			})
		}
	}

	application, sessionID := newAdmissionRaceApp(t, handler)
	workingDir := application.config.WorkingDir()

	private := filepath.Join(workingDir, "private")
	require.NoError(t, os.MkdirAll(private, 0o755))
	keyFile := filepath.Join(private, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("SECRET_MUST_NOT_LEAK"), 0o644))

	alias := filepath.Join(workingDir, "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	overrides := RunOverrides{
		FolderScopes: []permission.FolderScopeEntry{
			{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}},
			{Dir: "alias"}, // empty Ops: deny carve-out
		},
	}

	res, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "SYMLINK_CARVEOUT_TURN_MARKER please read alias/key.txt",
		Overrides:         overrides,
		Mode:              RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotEmpty(t, toolResultBody,
		"the follow-up round carrying the fs_read tool result never happened")
	require.NotContains(t, toolResultBody, "SECRET_MUST_NOT_LEAK",
		"a symlinked deny carve-out must still deny the read even though the broader WorkingDir grant covers its resolved target: "+
			"pre-fix, the matcher compared a symlink-resolved item path against a lexical (unresolved) carve-out root and let the read through")
	require.Contains(t, toolResultBody, "deny-carved scope",
		"the denial must be the typed carve-out reason, not a fall-through allow")
}
