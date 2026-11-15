package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fang/v2"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCommandErrorHandlerSettingsLockHint(t *testing.T) {
	var styles fang.Styles
	var got bytes.Buffer
	path := `C:\Users\user\AppData\Rush\rush.ktav`
	locked := &config.ErrSettingsLockUnrecognized{Path: path}
	commandErrorHandler(&got, styles, locked)
	require.Contains(t, got.String(), path)
	require.Contains(t, got.String(), "remove sync_rev by hand and run rush lock again")

	got.Reset()
	root := t.TempDir()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "XDG_DATA_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	for _, key := range []string{"RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	dataDir := filepath.Join(root, "isolated-global")
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	lockFile := filepath.Join(dataDir, "rush.json")
	fixture := `{"sync_rev":"` + config.HashPassword("known-password") + `"}`
	require.NoError(t, os.WriteFile(lockFile, []byte(fixture), 0o600))
	wrongPassword := config.CheckPasswordAgainstDisk("not-the-password")
	require.ErrorIs(t, wrongPassword, config.ErrWrongPassword)
	commandErrorHandler(&got, styles, wrongPassword)
	require.NotContains(t, got.String(), "remove sync_rev")
}

// Revert-check: commandErrorHandler (installed by Execute) pins plain timeout rendering and default delegation.
func TestCommandErrorHandlerTimeoutAndDefault(t *testing.T) {
	var styles fang.Styles
	timeoutErr := &app.RunTimeoutError{Cause: &agent.RunTimeoutCause{Duration: time.Second, Source: "--timeout"}, ResumeCommand: "rush run --role fast --session s1"}
	var got bytes.Buffer
	commandErrorHandler(&got, styles, fmt.Errorf("wrapped: %w", timeoutErr))
	require.Equal(t, "rush: run timeout 1s exceeded (source: --timeout); resume: rush run --role fast --session s1; stored work remains in the session (rush sessions show/last)\n", got.String())
	require.NotContains(t, got.String(), "ERROR")
	require.ErrorIs(t, timeoutErr, context.DeadlineExceeded)
	got.Reset()
	var want bytes.Buffer
	generic := errors.New("ordinary failure")
	fang.DefaultErrorHandler(&want, styles, generic)
	commandErrorHandler(&got, styles, generic)
	require.Equal(t, want.String(), got.String())
}
