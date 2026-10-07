// Revert check: TestDefaultDir_EnvAndGuard -> deleting the RUSH_HEARTBEAT_DIR
// branch or the testing.Testing() guard in DefaultDir.
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
	t.Setenv("RUSH_GLOBAL_DATA", "")
	if got := DefaultDir(); got != "" {
		t.Fatalf("test guard: got %q, want \"\"", got)
	}
	tmp2 := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", tmp2)
	want := filepath.Join(tmp2, "heartbeat")
	if got := DefaultDir(); got != want {
		t.Fatalf("global data: got %q, want %q", got, want)
	}
}
