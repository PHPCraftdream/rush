package agent

// R5-2 regression (P0 security review,
// docs/reviews/2026-09-02-sdk-library-review-round-5-2331.md), durable
// rebuild path: RebuildSessionAgentCall recompiles a persisted
// FolderScopeSpec via permission.BuildFolderScope. Before the fix that
// call compiled the RAW, uncanonicalized spec, so a durable row whose
// carve-out entry names a REAL symlinked directory would rebuild into a
// scope whose carve-out never matches a symlink-resolved requested item
// path -- the exact R5-2 bypass, reachable a second time through the
// durable-restart path in addition to the initial in-process compile
// covered by internal/agent/tools/r5_2_canonicalize_folder_scope_test.go
// and internal/app/r5_2_symlink_carveout_test.go.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

func TestRebuildSessionAgentCall_RealSymlinkedDenyCarveOutUnderBroaderGrantStillDenies(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	workDir := t.TempDir()
	private := filepath.Join(workDir, "private")
	require.NoError(t, os.MkdirAll(private, 0o755))
	keyFile := filepath.Join(private, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("secret"), 0o644))

	alias := filepath.Join(workDir, "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	data := session.SessionAgentCallData{
		SessionID: "scope-rebuild-symlink-probe",
		Prompt:    "hello",
		FolderScopeSpec: &session.FolderScopeSpec{
			WorkingDir: workDir,
			Entries: []session.FolderScopeEntry{
				{Dir: ".", Ops: []session.FileOp{session.FileOp("read")}},
				{Dir: "alias"}, // empty Ops: deny carve-out
			},
		},
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)
	require.NotNil(t, call.CallOptions)
	require.NotNil(t, call.CallOptions.FolderScope)

	resolvedItem, err := filepath.EvalSymlinks(keyFile)
	require.NoError(t, err)

	err = call.CallOptions.FolderScope.Check(resolvedItem, permission.FileOpRead)
	require.Error(t, err, "a durably-rebuilt scope's symlinked deny carve-out must still deny the resolved item path")
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, err, &denied)
	require.Contains(t, denied.Reason, "deny-carved scope")

	// The parent grant still works for a sibling outside the carve-out.
	// Resolved through EvalSymlinks for the same reason resolvedItem is
	// above: Check's contract requires an already-resolved path (see its
	// doc comment), and a real request always arrives pre-resolved via
	// resolveScopedPath. workDir itself can sit under a symlinked path
	// component on some platforms/CI runners (e.g. macOS's /var ->
	// /private/var, or a directory-junctioned CI workspace root), which
	// a raw filepath.Join(workDir, ...) does not account for -- found by
	// this exact assertion failing on GitHub Actions windows-latest and
	// macos-latest runners the first time this test ever ran on real CI
	// (this dev machine's own temp directory happens not to traverse a
	// symlink, which is why it was never caught locally).
	sibling := filepath.Join(workDir, "open.txt")
	require.NoError(t, os.WriteFile(sibling, []byte("x"), 0o644))
	resolvedSibling, err := filepath.EvalSymlinks(sibling)
	require.NoError(t, err)
	require.NoError(t, call.CallOptions.FolderScope.Check(resolvedSibling, permission.FileOpRead))
}
