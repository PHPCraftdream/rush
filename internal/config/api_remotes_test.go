package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func isolateAPIRemoteTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv(GlobalDirEnv, "")
	return root
}

func TestAPIRemoteValidation(t *testing.T) {
	isolateAPIRemoteTest(t)
	valid := APIRemote{URL: "https://example.test", TokenEnv: "RUSH_API_TOKEN"}
	cases := []struct {
		name   string
		remote APIRemote
		valid  bool
	}{
		{"valid", valid, true},
		{"no source", APIRemote{URL: valid.URL}, false},
		{"two sources", APIRemote{URL: valid.URL, TokenEnv: "TOKEN", TokenFile: "/tmp/token"}, false},
		{"reserved env", APIRemote{URL: valid.URL, TokenEnv: "RUSH_TOKENS"}, false},
		{"http remote", APIRemote{URL: "http://example.test", TokenEnv: "TOKEN"}, false},
		{"loopback http", APIRemote{URL: "http://127.0.0.1:8080", TokenEnv: "TOKEN"}, true},
		{"userinfo", APIRemote{URL: "https://user:pass@example.test", TokenEnv: "TOKEN"}, false},
		{"query", APIRemote{URL: "https://example.test/?x=1", TokenEnv: "TOKEN"}, false},
		{"bad pin", APIRemote{URL: valid.URL, TokenEnv: "TOKEN", Pin: "sha256//bad"}, false},
		{"one client file", APIRemote{URL: valid.URL, TokenEnv: "TOKEN", ClientCert: "/cert"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { err := ValidateAPIRemote(tc.remote); require.Equal(t, tc.valid, err == nil) })
	}
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

func TestAPIRemoteTokenRead(t *testing.T) {
	isolateAPIRemoteTest(t)
	got, err := ReadAPIRemoteToken(APIRemote{TokenEnv: "SECRET"}, func(string) string { return "credential" })
	require.NoError(t, err)
	require.Equal(t, "credential", got)
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("credential\n"), 0o600))
	got, err = ReadAPIRemoteToken(APIRemote{TokenFile: path}, nil)
	home, homeErr := os.UserHomeDir()
	underProfile := homeErr == nil && pathWithin(home, path)
	if runtime.GOOS != "windows" || underProfile {
		require.NoError(t, err)
		require.Equal(t, "credential", got)
	} else {
		require.Error(t, err)
	}
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	_, err = ReadAPIRemoteToken(APIRemote{TokenFile: path}, nil)
	require.Error(t, err)
}

func TestAPIRemoteErrorsDoNotEchoToken(t *testing.T) {
	isolateAPIRemoteTest(t)
	token := "rush_12345678." + strings.Repeat("a", 43)
	for _, err := range []error{validateAPIRemoteName(token), ValidateAPIRemote(APIRemote{URL: "https://example.test", TokenEnv: token}), func() error { _, err := WriteAPIRemoteTokenFile(token, []byte("bad")); return err }()} {
		require.NotNil(t, err)
		require.NotContains(t, err.Error(), token)
	}
}

func TestWriteAPIRemoteTokenFile(t *testing.T) {
	isolateAPIRemoteTest(t)
	token := []byte("rush_12345678." + strings.Repeat("a", 43))
	path, err := WriteAPIRemoteTokenFile("prod", token)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, token, got)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		sentinel := filepath.Join(filepath.Dir(path), "sentinel")
		require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Symlink(sentinel, path))
		_, err = WriteAPIRemoteTokenFile("prod", token)
		require.NoError(t, err)
		got, err := os.ReadFile(sentinel)
		require.NoError(t, err)
		require.Equal(t, "keep", string(got))
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

func TestAPIRemoteTokenSizeAndPermission(t *testing.T) {
	isolateAPIRemoteTest(t)
	dir := t.TempDir()
	home, homeErr := os.UserHomeDir()
	if runtime.GOOS == "windows" && (homeErr != nil || !pathWithin(home, dir)) {
		t.Skip("temp dir is outside the user profile; size check is unreachable on Windows")
	}
	big := filepath.Join(dir, "big")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("x", 4097)), 0o600))
	_, err := ReadAPIRemoteToken(APIRemote{TokenFile: big}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "4 KiB")
	if runtime.GOOS != "windows" {
		tokenFile := filepath.Join(dir, "token-file")
		require.NoError(t, os.WriteFile(tokenFile, []byte("credential"), 0o600))
		_, err = ReadAPIRemoteToken(APIRemote{TokenFile: tokenFile}, nil)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(tokenFile, 0o644))
		_, err = ReadAPIRemoteToken(APIRemote{TokenFile: tokenFile}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "permissions")
	}
}

func TestReadAPIRemoteTokenDoesNotEchoTokenLookingEnv(t *testing.T) {
	isolateAPIRemoteTest(t)
	token := "rush_12345678." + strings.Repeat("a", 43)
	_, err := ReadAPIRemoteToken(APIRemote{TokenEnv: token}, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), token)
	_, _, err = ResolveAPIRemote(token, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), token)
}
