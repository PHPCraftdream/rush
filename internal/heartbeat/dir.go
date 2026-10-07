package heartbeat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
)

// DefaultDir is the global heartbeat directory: RUSH_HEARTBEAT_DIR wins,
// else heartbeat/ next to the global settings file. A test binary without
// an isolated global dir returns "" so tests never write the real registry.
func DefaultDir() string {
	if d := os.Getenv("RUSH_HEARTBEAT_DIR"); d != "" {
		return d
	}
	// A test binary writes snapshots only into an explicit RUSH_HEARTBEAT_DIR:
	// the process-wide writer outlives each test and would race its TempDir.
	if skipDefaultDirInTests {
		return ""
	}
	return filepath.Join(config.GlobalWorkspaceDir(), "heartbeat")
}

// skipDefaultDirInTests is true in a test binary; the dir test turns it off.
var skipDefaultDirInTests = testing.Testing()
