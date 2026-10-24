// Lock-anchor alias unification: the inter-process write sidecar must anchor
// on the RESOLVED physical destination (normalizeReloadPath first, then the
// Ktav -> legacy JSON mapping in configWriteLockAnchor), so two path aliases
// of one rush.ktav share one sidecar instead of racing on two .lock files.
//
// Revert-checks:
//   - resolving the anchor from the UNRESOLVED path (the pre-fix ordering in
//     resolveConfigWriteTarget, store_write.go) re-splits aliases onto
//     alias.json.lock and target.json.lock.
//   - stripping the .ktav extension case-sensitively (strings.TrimSuffix)
//     turns "rush.KTAV" into the bogus anchor "rush.KTAV.json"; the strip
//     must follow isKtavConfigPath's strings.EqualFold semantics.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKtavWriteLockAnchorAliasUnification pins that distinct path spellings
// of one rush.ktav document normalize to the SAME write sidecar anchor.
func TestKtavWriteLockAnchorAliasUnification(t *testing.T) {
	ktavIsolateGlobals(t)

	// Canonical temp dir: t.TempDir can itself sit behind a symlink, and the
	// alias assertions below need the directory spelling to be canonical.
	canonicalDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	realPath := filepath.Join(canonicalDir, "rush.ktav")
	require.NoError(t, os.WriteFile(realPath, testKtavBytes(t, `{"providers":{"p":{"api_key":"k"}}}`), 0o600))

	// anchorOf mirrors the production ordering pinned by this test (resolve
	// the physical destination FIRST, then map Ktav -> legacy JSON).
	anchorOf := func(path string) string {
		return normalizeReloadPath(configWriteLockAnchor(normalizeReloadPath(path)))
	}

	if runtime.GOOS != "windows" {
		// A symlink alias of the same document must share the real file's
		// anchor (created with require.NoError: a privilege failure must
		// fail the test, not silently drop the alias coverage).
		aliasPath := filepath.Join(canonicalDir, "alias.ktav")
		require.NoError(t, os.Symlink(realPath, aliasPath))
		require.Equal(t, anchorOf(realPath), anchorOf(aliasPath),
			"a symlink alias of one rush.ktav must share one write sidecar anchor")
	} else {
		// Documented Windows alternative (symlink creation usually needs
		// elevated privileges there): two DISTINCT case spellings of the
		// same leaf. The Windows path normalization canonicalizes to the
		// on-disk leaf casing, so both spellings resolve to the same
		// physical rush.ktav and therefore the same anchor.
		mixedPath := filepath.Join(canonicalDir, "rush.KTAV")
		require.Equal(t, anchorOf(realPath), anchorOf(mixedPath),
			"case-alias spellings of one rush.ktav must share one write sidecar anchor")
	}

	// Platform-independent: the Ktav -> legacy strip must follow
	// isKtavConfigPath's EqualFold semantics, so a mixed-case extension
	// spelling still maps to the legacy JSON name (not "rush.KTAV.json").
	mixedPath := filepath.Join(canonicalDir, "rush.KTAV")
	require.Equal(t, filepath.Join(canonicalDir, "rush.json"), configWriteLockAnchor(mixedPath))
}
