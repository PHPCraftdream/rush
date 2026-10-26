// Lock-anchor alias unification: the inter-process write sidecar must anchor
// on the RESOLVED physical destination (normalizeReloadPath first, then the
// Ktav -> legacy JSON mapping in configWriteLockAnchor — the production order
// inside resolveConfigWriteTarget), so two path aliases of one rush.ktav
// share one sidecar instead of racing on two .lock files.
//
// Revert-checks:
//   - resolving the anchor from the UNRESOLVED path (store_write.go:
//     `configWriteLockAnchor(path)` instead of `configWriteLockAnchor(resolved)`)
//     re-splits aliases onto alias.json.lock and target.json.lock: on non-Windows
//     normalizeReloadPath canonicalizes the symlink alias to the real spelling,
//     so the unresolved alias anchor (alias.json.lock) and the resolved one
//     (rush.json.lock) differ, and both assertions below fail.
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
// of one rush.ktav document resolve to the SAME write sidecar anchor through
// the PRODUCTION target resolution (resolveConfigWriteTarget).
func TestKtavWriteLockAnchorAliasUnification(t *testing.T) {
	lockSourceIsolate(t)

	// Canonical temp dir: t.TempDir can itself sit behind a symlink, and the
	// alias assertions below need the directory spelling to be canonical.
	canonicalDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	realPath := filepath.Join(canonicalDir, "rush.ktav")
	require.NoError(t, os.WriteFile(realPath, testKtavBytes(t, `{"providers":{"p":{"api_key":"k"}}}`), 0o600))

	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: realPath})

	if runtime.GOOS != "windows" {
		// A symlink alias of the same document must share the real file's
		// sidecar (created with require.NoError: a privilege failure must
		// fail the test, not silently drop the alias coverage).
		aliasPath := filepath.Join(canonicalDir, "alias.ktav")
		require.NoError(t, os.Symlink(realPath, aliasPath))
		realTarget, err := s.resolveConfigWriteTarget(realPath)
		require.NoError(t, err)
		aliasTarget, err := s.resolveConfigWriteTarget(aliasPath)
		require.NoError(t, err)
		require.Equal(t, realTarget.lockPath, aliasTarget.lockPath,
			"a symlink alias of one rush.ktav must share one write sidecar")
	} else {
		// Documented Windows alternative (symlink creation usually needs
		// elevated privileges there): two DISTINCT case spellings of the
		// same leaf. normalizeReloadPath inside resolveConfigWriteTarget
		// canonicalizes to the on-disk leaf casing, so both spellings
		// resolve to the same physical rush.ktav and therefore the same
		// sidecar.
		mixedPath := filepath.Join(canonicalDir, "rush.KTAV")
		realTarget, err := s.resolveConfigWriteTarget(realPath)
		require.NoError(t, err)
		mixedTarget, err := s.resolveConfigWriteTarget(mixedPath)
		require.NoError(t, err)
		require.Equal(t, realTarget.lockPath, mixedTarget.lockPath,
			"case-alias spellings of one rush.ktav must share one write sidecar")
	}

	// Platform-independent: the Ktav -> legacy strip must follow
	// isKtavConfigPath's EqualFold semantics, so a mixed-case extension
	// spelling still maps to the legacy JSON name (not "rush.KTAV.json").
	mixedPath := filepath.Join(canonicalDir, "rush.KTAV")
	require.Equal(t, filepath.Join(canonicalDir, "rush.json"), configWriteLockAnchor(mixedPath))
}
