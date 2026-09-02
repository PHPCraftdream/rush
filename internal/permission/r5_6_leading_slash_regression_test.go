package permission

// R5-6 follow-up regression, found during orchestrator zero-trust review
// (not in the original review file): the R5-6 fix widened
// BuildFolderScope's absoluteness check from filepath.IsAbs to
// filepathext.SmartIsAbs so entries canonicalized against a synthesized
// virtual root (a driveless "/..." literal, real-absolute on Unix but
// only SmartIsAbs on Windows) would not be re-joined onto WorkingDir a
// second time. But on Windows, filepathext.SmartIsAbs("/foo") is ALSO
// true under a REAL drive-rooted WorkingDir (e.g. `D:\project`) — so a
// leading-slash-style entry ({Dir: "/foo"}, a plausible operator
// spelling for "the foo subdirectory") stopped being joined onto
// WorkingDir and became a driveless, permanently unreachable root: since
// every REAL request resolves to a drive-rooted path, a leading-slash
// deny carve-out silently went inert — the exact fail-open regression
// class R5-2 exists to prevent, reintroduced through a different
// mechanism. Fixed by only skipping the join when spec.WorkingDir is
// ITSELF in that same driveless-virtual namespace (see
// BuildFolderScope's alreadyAbsInThisNamespace).

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFolderScope_LeadingSlashEntryUnderRealWorkingDirStillJoinsAndDenies(t *testing.T) {
	workDir := `D:\real-project`
	spec := FolderScopeSpec{
		WorkingDir: workDir,
		Entries: []FolderScopeEntry{
			{Dir: ".", Ops: []FileOp{FileOpRead}},
			{Dir: "/foo"}, // deny carve-out, leading-slash spelling
		},
	}
	scope, err := BuildFolderScope(spec)
	require.NoError(t, err)

	target := filepath.Join(workDir, "foo", "secret.txt")
	err = scope.Check(target, FileOpRead)
	require.Error(t, err, "a leading-slash-spelled deny carve-out under a REAL WorkingDir must still join and deny, not go inert")
	var denied *ScopeDeniedError
	require.ErrorAs(t, err, &denied)
	assert.Contains(t, denied.Reason, "deny-carved scope")

	// The parent grant still works for a sibling outside the carve-out.
	sibling := filepath.Join(workDir, "open.txt")
	assert.NoError(t, scope.Check(sibling, FileOpRead))
}

func TestBuildFolderScope_VirtualRootEntryStaysUnjoined(t *testing.T) {
	const virtualRoot = "/rush-library-mode-root"
	spec := FolderScopeSpec{
		WorkingDir: virtualRoot,
		Entries: []FolderScopeEntry{
			{Dir: ".", Ops: []FileOp{FileOpRead}},
		},
	}
	scope, err := BuildFolderScope(spec)
	require.NoError(t, err)

	target := filepath.Join(virtualRoot, "scoped", "f.txt")
	assert.NoError(t, scope.Check(target, FileOpRead),
		"a WorkingDir that is itself in the driveless-virtual namespace must still grant normally")
}
