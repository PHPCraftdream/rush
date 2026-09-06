//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestRenameConfigTempUsesWriteThroughAndOptionalReplacement(t *testing.T) {
	for _, test := range []struct {
		name      string
		replace   bool
		wantFlags uint32
	}{
		{name: "no replace", wantFlags: windows.MOVEFILE_WRITE_THROUGH},
		{name: "replace", replace: true, wantFlags: windows.MOVEFILE_WRITE_THROUGH | windows.MOVEFILE_REPLACE_EXISTING},
	} {
		t.Run(test.name, func(t *testing.T) {
			apiErr := errors.New("injected MoveFileEx failure")
			var gotFlags uint32
			configTestHooks.Lock()
			previous := configTestHooks.moveFileEx
			configTestHooks.moveFileEx = func(_, _ *uint16, flags uint32) error {
				gotFlags = flags
				return apiErr
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.moveFileEx = previous
				configTestHooks.Unlock()
			})

			err := renameConfigTemp("source", "destination", test.replace)
			require.ErrorIs(t, err, apiErr)
			require.Equal(t, test.wantFlags, gotFlags)
		})
	}
}

func TestWindowsCommitMoveFileExFailureAfterPublicationReturnsCommitOutcome(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	data := []byte(`{"new":true}`)
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))
	_, expected, err := readStableConfigFile(path)
	require.NoError(t, err)

	apiErr := errors.New("MoveFileEx result lost after publication")
	configTestHooks.Lock()
	previous := configTestHooks.moveFileEx
	configTestHooks.moveFileEx = func(from, to *uint16, _ uint32) error {
		fromPath := windows.UTF16PtrToString(from)
		toPath := windows.UTF16PtrToString(to)
		if renameErr := os.Rename(fromPath, toPath); renameErr != nil {
			return renameErr
		}
		return apiErr
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.moveFileEx = previous
		configTestHooks.Unlock()
	})

	_, err = commitConfigFile(path, path, data, 0o600, expected, -1, false)
	var outcome *CommitOutcome
	require.ErrorAs(t, err, &outcome)
	require.True(t, outcome.Committed)
	require.True(t, outcome.Reconciled)
	require.ErrorIs(t, outcome, errConfigCommitCommitted)
	require.ErrorIs(t, outcome, apiErr)
	require.Equal(t, data, mustReadFile(t, path))
}

func TestWindowsParentSyncIsAProductionNoOp(t *testing.T) {
	require.NoError(t, syncConfigParentOnDisk(filepath.Join(t.TempDir(), "does-not-exist")))
}
