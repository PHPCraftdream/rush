//go:build windows

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
//
// Windows-only (//go:build windows): the ambiguity this guards against
// — a leading-slash path being SmartIsAbs but not filepath.IsAbs — only
// exists on Windows. On Unix, "/foo" is unambiguously filepath.IsAbs
// true, so alreadyAbsInThisNamespace is true via the FIRST disjunct
// regardless of WorkingDir's namespace, and this test's hardcoded
// `D:\real-project` WorkingDir literal is itself a Windows-path-format
// assumption that does not translate to Unix path semantics. Found by
// this exact test failing on GitHub Actions ubuntu-latest/macos-latest
// runners the first time this code was ever pushed to real CI (an
// error was expected but the check returned nil, because the whole
// scenario doesn't exist on those platforms) — the fix is to scope the
// test to the platform where the finding is real, not to reshape it for
// semantics that don't apply.

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
