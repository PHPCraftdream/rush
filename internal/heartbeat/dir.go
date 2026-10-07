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
	if testing.Testing() && os.Getenv("RUSH_GLOBAL_DATA") == "" {
		return ""
	}
	return filepath.Join(config.GlobalWorkspaceDir(), "heartbeat")
}
