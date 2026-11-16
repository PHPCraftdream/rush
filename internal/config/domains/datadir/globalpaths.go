// Well-known global paths: the config, cache, data, and server-workspace
// locations every load reads from and writes to.
package datadir

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	assertTestGlobalStateIsolated()
	// --global-dir wins over every other global-config override: a single
	// root must not partially mix with RUSH_GLOBAL_CONFIG/XDG/home.
	if root, ok := GlobalDir(); ok {
		return filepath.Join(root, "config", fmt.Sprintf("%s.json", cfgmodel.AppName))
	}
	if rushGlobal := os.Getenv("RUSH_GLOBAL_CONFIG"); rushGlobal != "" {
		return filepath.Join(rushGlobal, fmt.Sprintf("%s.json", cfgmodel.AppName))
	}
	return filepath.Join(home.Config(), cfgmodel.AppName, fmt.Sprintf("%s.json", cfgmodel.AppName))
}

// SystemConfig returns the system-wide configuration file path (e.g.
// /etc/rush/rush.json on Unix). It is empty on Windows, where there is no
// system-wide config location — see config_unix.go / config_windows.go.
func SystemConfig() string {
	return systemConfigSlot()
}

// GlobalCacheDir returns the path to the global cache directory for the
// application.
func GlobalCacheDir() string {
	assertTestGlobalStateIsolated()
	// --global-dir wins over every other cache-dir override (see GlobalConfig).
	if root, ok := GlobalDir(); ok {
		return filepath.Join(root, "cache")
	}
	if rushCache := os.Getenv("RUSH_CACHE_DIR"); rushCache != "" {
		return rushCache
	}
	if xdgCacheHome := os.Getenv("XDG_CACHE_HOME"); xdgCacheHome != "" {
		return filepath.Join(xdgCacheHome, cfgmodel.AppName)
	}
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, cfgmodel.AppName, "cache")
	}
	return filepath.Join(home.Dir(), ".cache", cfgmodel.AppName)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	assertTestGlobalStateIsolated()
	// --global-dir wins over every other data-dir override (see GlobalConfig).
	if dataRoot, ok := globalDataRoot(); ok {
		return filepath.Join(dataRoot, fmt.Sprintf("%s.json", cfgmodel.AppName))
	}
	if rushData := os.Getenv("RUSH_GLOBAL_DATA"); rushData != "" {
		return filepath.Join(rushData, fmt.Sprintf("%s.json", cfgmodel.AppName))
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, cfgmodel.AppName, fmt.Sprintf("%s.json", cfgmodel.AppName))
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/rush/`
	// for linux and macOS, it should be in `$HOME/.local/share/rush/`
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, cfgmodel.AppName, fmt.Sprintf("%s.json", cfgmodel.AppName))
	}

	return filepath.Join(home.Dir(), ".local", "share", cfgmodel.AppName, fmt.Sprintf("%s.json", cfgmodel.AppName))
}

// GlobalWorkspaceDir returns the path to the global server workspace
// directory. This directory acts as a meta-workspace for the server
// process, giving it a real workingDir so that config loading, scoped
// writes, and provider resolution behave identically to project
// workspaces.
func GlobalWorkspaceDir() string {
	assertTestGlobalStateIsolated()
	return filepath.Dir(GlobalConfigData())
}

// file to cache provider data
func cachePathFor(name string) string {
	// Provider caches are Rush-owned data siblings: under --global-dir they
	// move into the isolated root's data dir.
	if dataRoot, ok := globalDataRoot(); ok {
		return filepath.Join(dataRoot, name+".json")
	}
	xdgDataHome := os.Getenv("XDG_DATA_HOME")
	if xdgDataHome != "" {
		return filepath.Join(xdgDataHome, cfgmodel.AppName, name+".json")
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/rush/`
	// for linux and macOS, it should be in `$HOME/.local/share/rush/`
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(localAppData, cfgmodel.AppName, name+".json")
	}

	return filepath.Join(home.Dir(), ".local", "share", cfgmodel.AppName, name+".json")
}

// canonicalConfigPath resolves the starting directory before both the git
// boundary probe and the upward walk. A symlinked working directory can point
// into a nested repository; walking its lexical parents would otherwise let
// the search escape that repository and adopt an unrelated parent config.
func canonicalConfigPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	} else if !os.IsNotExist(err) {
		return abs
	}

	// EvalSymlinks cannot resolve an absent leaf, but its existing parent may
	// still be a symlink or junction. Resolve that parent so an absent
	// candidate and the same candidate reached through an alias share the
	// identity used by reload fingerprints.
	var missing []string
	for current := abs; ; current = filepath.Dir(current) {
		missing = append(missing, filepath.Base(current))
		parent := filepath.Dir(current)
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if parent == current || !os.IsNotExist(resolveErr) {
			return abs
		}
	}
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Rush from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	dir = canonicalConfigPath(dir)
	if root := worktreeRoot(dir); root != "" {
		return canonicalConfigPath(root)
	}
	return dir
}

// sameDir reports whether a and b refer to the same directory, accounting
// for symbolic links. Paths are canonicalised with filepath.EvalSymlinks
// before comparison so that a symlinked path (macOS /var) and its resolved
// target (/private/var) compare equal. If either path cannot be resolved
// (e.g. it does not exist), the raw value is used as a best-effort fallback.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}
