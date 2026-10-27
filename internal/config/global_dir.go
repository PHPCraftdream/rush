// --global-dir (RUSH_GLOBAL_DIR): a single root that relocates ALL
// Rush-owned global state — config, data, cache, Rush-owned skills and
// commands, and the settings-lock canonical source — under one directory,
// so an experimental build can run side by side with a production install
// without ever touching the operator's real global state.
package config

import (
	"os"
	"path/filepath"
)

// GlobalDirEnv is the environment variable that selects the --global-dir
// root. The command-line flag wins over the environment because the flag
// handler overwrites this variable (see internal/cmd/root.go).
const GlobalDirEnv = "RUSH_GLOBAL_DIR"

// GlobalDir returns the --global-dir root and whether it is active. The
// value is made absolute and cleaned; if filepath.Abs fails (it only
// fails when the working directory cannot be resolved) the cleaned
// relative value is returned rather than dropping the override.
func GlobalDir() (string, bool) {
	root := os.Getenv(GlobalDirEnv)
	if root == "" {
		return "", false
	}
	root = filepath.Clean(root)
	if abs, err := filepath.Abs(root); err == nil {
		return abs, true
	}
	return root, true
}

// globalDataRoot returns the <root>/data directory under an active
// --global-dir root. It is the common parent of the data file and every
// sibling cache (oauth journals, provider/hyper/model-catalog caches,
// projects.json, audit logs, heartbeat, bin).
func globalDataRoot() (string, bool) {
	root, ok := GlobalDir()
	if !ok {
		return "", false
	}
	return filepath.Join(root, "data"), true
}

// EnsureGlobalDirTree eagerly creates the fixed layout under an active
// --global-dir root with 0700 permissions. It is called from the flag
// handler (not for env-only use, where each consumer creates its own
// directories as it does today). MkdirAll is idempotent and resolution
// must never fail because of it, so errors are intentionally ignored.
func EnsureGlobalDirTree() {
	root, ok := GlobalDir()
	if !ok {
		return
	}
	for _, dir := range []string{
		root,
		filepath.Join(root, "config"),
		filepath.Join(root, "data"),
		filepath.Join(root, "cache"),
	} {
		_ = os.MkdirAll(dir, 0o700)
	}
}

// dedupeDirs removes duplicate paths (after cleaning) while preserving
// order. Used when an isolated --global-dir root redirects two legacy
// entries to the same relocated directory.
func dedupeDirs(dirs []string) []string {
	seen := make(map[string]struct{}, len(dirs))
	out := dirs[:0]
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// systemConfigSlot is the system-wide config slot, disabled under an active
// --global-dir root: an isolated instance must neither read nor migrate the
// stable install's /etc/rush config.
func systemConfigSlot() string {
	if _, ok := GlobalDir(); ok {
		return ""
	}
	return systemConfigPath
}
