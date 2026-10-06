package filetracker

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// A path stored absolute (no relative form) must come back unchanged from
// ListReadFiles; joining it onto the cwd produced garbage (A48).
// Revert-check: restore the unconditional filepath.Join => red.
func TestService_ListReadFiles_AbsoluteStoredPath(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "abs")

	abs := filepath.Join(t.TempDir(), "SKILL.md")
	require.True(t, filepath.IsAbs(abs))
	require.NoError(t, env.q.RecordFileRead(env.ctx, db.RecordFileReadParams{SessionID: "abs", Path: abs}))

	got, err := env.svc.ListReadFiles(env.ctx, "abs")
	require.NoError(t, err)
	require.Equal(t, []string{abs}, got)
}

// A relative stored path is still resolved against the cwd.
func TestService_ListReadFiles_RelativeStoredPath(t *testing.T) {
	env := setupTest(t)
	env.createSession(t, "rel")

	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, env.q.RecordFileRead(env.ctx, db.RecordFileReadParams{SessionID: "rel", Path: filepath.Join("sub", "a.go")}))

	got, err := env.svc.ListReadFiles(env.ctx, "rel")
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(dir, "sub", "a.go")}, got)
}

// A file on another Windows volume has no relative form: it stays absolute.
func TestRelpathFrom_OtherVolumeStaysAbsolute(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("volumes exist only on Windows")
	}
	require.Equal(t, `C:\x\SKILL.md`, relpathFrom(`D:\ws`, `C:\x\SKILL.md`))
	require.Equal(t, `sub\a.go`, relpathFrom(`D:\ws`, `D:\ws\sub\a.go`))
}
