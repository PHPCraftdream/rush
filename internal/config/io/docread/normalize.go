// Config path normalization for reload bookkeeping.
package docread

import (
	"errors"
	"path/filepath"
)

func normalizeReloadPath(path string) string {
	if path == "" {
		return ""
	}
	return canonicalConfigPath(path)
}

func normalizeDiscoveryPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

// ErrConfigReloadUnstable indicates that no stable file snapshot could be
// built within the bounded reload retry budget.
var ErrConfigReloadUnstable = errors.New("config files remained unstable during reload")
