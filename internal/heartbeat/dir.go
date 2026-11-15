package heartbeat

import (
	"os"
	"path/filepath"
	"testing"
)

// DefaultDir is the global heartbeat directory: RUSH_HEARTBEAT_DIR wins,
// else heartbeat/ next to the global settings file — derived from
// workspaceDir, the caller's global workspace directory (a func so it is
// resolved only when the old code resolved it). An active
// --global-dir root (globalDirActive) pins the directory to the workspace,
// so RUSH_HEARTBEAT_DIR cannot override it. A test binary without an
// isolated global dir returns "" so tests never write the real registry.
func DefaultDir(workspaceDir func() string, globalDirActive bool) string {
	// An isolated global dir (RUSH_GLOBAL_DIR) does not lift the test guard
	// below: the process-wide writer follows the environment of whichever test
	// runs now and would keep writing into that test's TempDir while it is
	// being removed.
	if globalDirActive && !skipDefaultDirInTests {
		return filepath.Join(workspaceDir(), "heartbeat")
	}
	if d := os.Getenv("RUSH_HEARTBEAT_DIR"); d != "" {
		return d
	}
	// A test binary writes snapshots only into an explicit RUSH_HEARTBEAT_DIR:
	// the process-wide writer outlives each test and would race its TempDir.
	if skipDefaultDirInTests {
		return ""
	}
	return filepath.Join(workspaceDir(), "heartbeat")
}

// skipDefaultDirInTests is true in a test binary; the dir test turns it off.
var skipDefaultDirInTests = testing.Testing()
