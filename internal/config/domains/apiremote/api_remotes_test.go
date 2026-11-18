package apiremote

import (
	"os"
	"path/filepath"
	"runtime"
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
