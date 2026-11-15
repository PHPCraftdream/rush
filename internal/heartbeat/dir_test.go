// Revert check: TestDefaultDir_EnvAndGuard -> deleting the RUSH_HEARTBEAT_DIR
// branch or the skipDefaultDirInTests guard in DefaultDir.
package heartbeat

import (
	"path/filepath"
	"testing"
)

func TestDefaultDir_EnvAndGuard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", tmp)
	SetDirFunc(nil)
	t.Cleanup(func() { SetDirFunc(nil) })
	workspaceDir := t.TempDir()
	workspace := func() string { return workspaceDir }
	if got := DefaultDir(workspace, false); got != tmp {
		t.Fatalf("env override: got %q, want %q", got, tmp)
	}
	t.Setenv("RUSH_HEARTBEAT_DIR", "")
	if got := DefaultDir(workspace, false); got != "" {
		t.Fatalf("test guard (even with a supplied default dir): got %q, want \"\"", got)
	}
	skipDefaultDirInTests = false
	t.Cleanup(func() { skipDefaultDirInTests = true })
	want := filepath.Join(workspaceDir, "heartbeat")
	if got := DefaultDir(workspace, false); got != want {
		t.Fatalf("supplied dir: got %q, want %q", got, want)
	}
}

// An active --global-dir root (globalDirActive) must not bring the heartbeat
// directory back in a test binary: the process-wide writer outlives each test
// and raced the TempDir removal of whichever test had set RUSH_GLOBAL_DIR
// ("data\heartbeat: The directory is not empty").
func TestDefaultDir_GlobalDirDoesNotLiftTheTestGuard(t *testing.T) {
	SetDirFunc(nil)
	t.Cleanup(func() { SetDirFunc(nil) })
	t.Setenv("RUSH_HEARTBEAT_DIR", "")
	if got := DefaultDir(func() string { return t.TempDir() }, true); got != "" {
		t.Fatalf("test guard with a global-dir root: got %q, want \"\"", got)
	}
}
