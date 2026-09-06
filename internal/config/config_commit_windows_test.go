//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsCommitExpectedAbsentRejectsFileCreatedBeforeRename(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	_, expected, err := readStableConfigFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	var once sync.Once
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = func() {
		once.Do(func() {
			require.NoError(t, os.WriteFile(path, []byte(`{"already":"there"}`), 0o600))
		})
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, []byte(`{"new":"value"}`), 0o600, expected, -1, false)
	require.Error(t, err)
	require.True(t, errors.Is(err, errConfigCommitVerification))
	require.Equal(t, []byte(`{"already":"there"}`), mustReadFile(t, path))
}
