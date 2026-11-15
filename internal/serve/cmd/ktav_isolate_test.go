package cmd

import (
	"path/filepath"
	"testing"
)

// ktavIsolateGlobalEnv isolates every environment variable consulted when
// resolving the global config/data locations: RUSH_GLOBAL_DATA,
// RUSH_GLOBAL_CONFIG, XDG_CONFIG_HOME, XDG_DATA_HOME, HOME, USERPROFILE and
// LOCALAPPDATA. Each is pointed at a fresh, EvalSymlinks-resolved temp
// directory so tests never touch the developer's real home.
//
// Uses t.Setenv, so tests calling this must NOT be t.Parallel, and it must
// run after (or instead of) other t.Setenv-based isolators — the last write
// wins.
func ktavIsolateGlobalEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", root, err)
	}
	for _, name := range []string{
		"RUSH_GLOBAL_DATA",
		"RUSH_GLOBAL_CONFIG",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"HOME",
		"USERPROFILE",
		"LOCALAPPDATA",
	} {
		t.Setenv(name, filepath.Join(resolved, name))
	}
}
