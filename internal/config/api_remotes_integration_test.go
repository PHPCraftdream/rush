package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/stretchr/testify/require"
)

func isolateAPIRemoteTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv(datadir.GlobalDirEnv, "")
	return root
}

func TestAPIRemoteRoundTripAndReplace(t *testing.T) {
	isolateAPIRemoteTest(t)
	remote := APIRemote{URL: "https://example.test", TokenEnv: "TOKEN"}
	require.NoError(t, SetAPIRemote("prod", remote, false))
	require.Error(t, SetAPIRemote("prod", remote, false))
	got, err := GetAPIRemote("prod")
	require.NoError(t, err)
	require.Equal(t, remote, got)
	require.NoError(t, SetAPIRemote("prod", APIRemote{URL: "https://other.test", TokenEnv: "TOKEN"}, true))
	require.NoError(t, SetDefaultAPIRemote("prod"))
	profiles, def, err := ListAPIRemotes()
	require.NoError(t, err)
	require.Equal(t, "prod", def)
	require.Len(t, profiles, 1)
	require.NoError(t, RemoveAPIRemote("prod"))
	_, def, err = ListAPIRemotes()
	require.NoError(t, err)
	require.Empty(t, def)
}

func TestAPIRemoteConcurrentSets(t *testing.T) {
	isolateAPIRemoteTest(t)
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			done <- SetAPIRemote("remote"+string(rune('a'+i)), APIRemote{URL: "https://example.test", TokenEnv: "TOKEN"}, false)
		}(i)
	}
	for i := 0; i < 8; i++ {
		require.NoError(t, <-done)
	}
	profiles, _, err := ListAPIRemotes()
	require.NoError(t, err)
	require.Len(t, profiles, 8)
}

func TestResolveAPIRemotePrecedence(t *testing.T) {
	isolateAPIRemoteTest(t)
	require.NoError(t, SetAPIRemote("prod", APIRemote{URL: "https://prod.test", TokenEnv: "TOKEN"}, false))
	require.NoError(t, SetAPIRemote("env", APIRemote{URL: "https://env.test", TokenEnv: "TOKEN"}, false))
	_, _, err := ResolveAPIRemote("", func(string) string { return "" })
	require.Error(t, err)
	require.Contains(t, err.Error(), "no remote selected")
	require.NoError(t, SetDefaultAPIRemote("prod"))
	for _, tc := range []struct{ selected, env, want string }{{"env", "prod", "env"}, {"", "env", "env"}, {"", "", "prod"}} {
		name, _, err := ResolveAPIRemote(tc.selected, func(string) string { return tc.env })
		require.NoError(t, err)
		require.Equal(t, tc.want, name)
	}
}

func TestGlobalAPIRemoteReadDoesNotUseWorkspace(t *testing.T) {
	root := isolateAPIRemoteTest(t)
	t.Chdir(root)
	workspace := filepath.Join(root, ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o700))
	require.NoError(t, os.WriteFile(workspace, []byte(`{"api":{"remotes":{"prod":{"url":"https://attacker.test","token_env":"TOKEN"},"workspace":{"url":"https://workspace.test","token_env":"TOKEN"}},"default_remote":"workspace"}}`), 0o600))
	require.NoError(t, SetAPIRemote("prod", APIRemote{URL: "https://global.test", TokenEnv: "TOKEN"}, false))
	_, remote, err := ResolveAPIRemote("prod", nil)
	require.NoError(t, err)
	require.Equal(t, "https://global.test", remote.URL)
	_, _, err = ResolveAPIRemote("workspace", nil)
	require.Error(t, err)
}

func TestAPIRemoteLifecycleErrorsAndCleanup(t *testing.T) {
	isolateAPIRemoteTest(t)
	remote := APIRemote{URL: "https://prod.test", TokenEnv: "PROD_TOKEN"}

	_, err := GetAPIRemote("missing")
	require.Error(t, err)
	require.Error(t, SetDefaultAPIRemote("missing"))
	require.Error(t, SetAPIRemote("Bad Name", remote, false))
	require.Error(t, SetAPIRemote("prod", APIRemote{URL: "https://prod.test"}, false), "no credential source")
	require.NoError(t, SetAPIRemote("prod", remote, false))
	require.Error(t, SetAPIRemote("prod", remote, false), "overwrite needs the explicit replace flag")
	require.NoError(t, SetAPIRemote("prod", APIRemote{URL: "https://prod2.test", TokenEnv: "PROD_TOKEN"}, true))
	got, err := GetAPIRemote("prod")
	require.NoError(t, err)
	require.Equal(t, "https://prod2.test", got.URL)

	token := []byte("rush_12345678." + strings.Repeat("b", 43))
	path, err := WriteAPIRemoteTokenFile("prod", token)
	require.NoError(t, err)
	require.FileExists(t, path)
	_, err = WriteAPIRemoteTokenFile("prod", []byte("not a token"))
	require.Error(t, err)
	_, err = WriteAPIRemoteTokenFile("Bad Name", token)
	require.Error(t, err)

	require.NoError(t, SetDefaultAPIRemote("prod"))
	require.NoError(t, RemoveAPIRemote("prod"))
	require.NoFileExists(t, path, "the token file created by this tool is removed with the profile")
	profiles, def, err := ListAPIRemotes()
	require.NoError(t, err)
	require.Empty(t, profiles)
	require.Empty(t, def, "removing the default profile clears api.default_remote")
	require.NoError(t, RemoveAPIRemote("prod"), "removing an absent profile is idempotent")
}
