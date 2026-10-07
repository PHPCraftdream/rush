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
	if got := DefaultDir(); got != tmp {
		t.Fatalf("env override: got %q, want %q", got, tmp)
	}
	t.Setenv("RUSH_HEARTBEAT_DIR", "")
	tmp2 := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", tmp2)
	if got := DefaultDir(); got != "" {
		t.Fatalf("test guard (even with RUSH_GLOBAL_DATA): got %q, want \"\"", got)
	}
	skipDefaultDirInTests = false
	t.Cleanup(func() { skipDefaultDirInTests = true })
	want := filepath.Join(tmp2, "heartbeat")
	if got := DefaultDir(); got != want {
		t.Fatalf("global data: got %q, want %q", got, want)
	}
}
