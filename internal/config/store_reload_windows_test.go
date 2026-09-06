//go:build windows

package config

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadStableConfigFileRejectsReplacedLeafWithStableMetadata(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "path"
		if alias {
			name = "symlink alias"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "rush.json")
			path := target
			oldData := []byte(`{"value":"old"}`)
			newData := []byte(`{"value":"new"}`)
			require.Equal(t, len(oldData), len(newData))
			require.NoError(t, os.WriteFile(target, oldData, 0o600))
			if alias {
				path = filepath.Join(root, "alias.json")
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}

			oldInfo, err := os.Stat(target)
			require.NoError(t, err)
			oldIdentity, err := configFileIdentityAtPath(target)
			require.NoError(t, err)
			var replaced sync.Once
			configTestHooks.Lock()
			previous := configTestHooks.afterStableRead
			configTestHooks.afterStableRead = func(string) {
				replaced.Do(func() {
					moved := filepath.Join(root, "old-rush.json")
					require.NoError(t, os.Rename(target, moved))
					require.NoError(t, os.WriteFile(target, newData, oldInfo.Mode().Perm()))
					require.NoError(t, os.Chmod(target, oldInfo.Mode().Perm()))
					require.NoError(t, os.Chtimes(target, oldInfo.ModTime(), oldInfo.ModTime()))
				})
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.afterStableRead = previous
				configTestHooks.Unlock()
			})

			data, fingerprint, err := readStableConfigFile(path)
			require.NoError(t, err)
			require.Equal(t, newData, data)
			require.Equal(t, sha256.Sum256(newData), fingerprint.digest)
			newIdentity, err := configFileIdentityAtPath(target)
			require.NoError(t, err)
			require.NotEqual(t, oldIdentity, newIdentity)
			require.Equal(t, newIdentity, fingerprint.identity)
		})
	}
}
