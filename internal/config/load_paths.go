// Well-known global paths: the config, cache, data, and server-workspace
// locations every load reads from and writes to.
package config

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/PHPCraftdream/rush/internal/home"
)

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	if rushGlobal := os.Getenv("RUSH_GLOBAL_CONFIG"); rushGlobal != "" {
		return filepath.Join(rushGlobal, fmt.Sprintf("%s.json", appName))
	}
	return filepath.Join(home.Config(), appName, fmt.Sprintf("%s.json", appName))
}

// SystemConfig returns the system-wide configuration file path (e.g.
// /etc/rush/rush.json on Unix). It is empty on Windows, where there is no
// system-wide config location — see config_unix.go / config_windows.go.
func SystemConfig() string {
	return systemConfigPath
}

// GlobalCacheDir returns the path to the global cache directory for the
// application.
func GlobalCacheDir() string {
	if rushCache := os.Getenv("RUSH_CACHE_DIR"); rushCache != "" {
		return rushCache
	}
	if xdgCacheHome := os.Getenv("XDG_CACHE_HOME"); xdgCacheHome != "" {
		return filepath.Join(xdgCacheHome, appName)
	}
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, appName, "cache")
	}
	return filepath.Join(home.Dir(), ".cache", appName)
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	return lookupConfigs(cwd)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	if rushData := os.Getenv("RUSH_GLOBAL_DATA"); rushData != "" {
		return filepath.Join(rushData, fmt.Sprintf("%s.json", appName))
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, appName, fmt.Sprintf("%s.json", appName))
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/rush/`
	// for linux and macOS, it should be in `$HOME/.local/share/rush/`
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, appName, fmt.Sprintf("%s.json", appName))
	}

	return filepath.Join(home.Dir(), ".local", "share", appName, fmt.Sprintf("%s.json", appName))
}

// GlobalWorkspaceDir returns the path to the global server workspace
// directory. This directory acts as a meta-workspace for the server
// process, giving it a real workingDir so that config loading, scoped
// writes, and provider resolution behave identically to project
// workspaces.
func GlobalWorkspaceDir() string {
	return filepath.Dir(GlobalConfigData())
}
