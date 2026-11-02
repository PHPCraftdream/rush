package config

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidAPIRemotePin(t *testing.T) {
	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = byte(i)
	}
	for pin, want := range map[string]bool{
		"sha256//" + base64.StdEncoding.EncodeToString(sum):      true,
		"cert-sha256:" + hex.EncodeToString(sum):                 true,
		"sha256//" + base64.StdEncoding.EncodeToString(sum[:16]): false,
		"sha256//not base64!":                                    false,
		"cert-sha256:" + hex.EncodeToString(sum[:16]):            false,
		"cert-sha256:" + strings.Repeat("zz", 32):                false,
		"md5:abcd": false,
		"":         false,
		"cert-sha256:" + strings.ToUpper(hex.EncodeToString(sum)): true,
	} {
		require.Equal(t, want, validAPIRemotePin(pin), "pin %q", pin)
	}
}

func TestIsLoopbackHostClassification(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "::1": true,
		"10.0.0.1": false, "example.test": false, "": false,
	} {
		require.Equal(t, want, isLoopbackHost(host), "host %q", host)
	}
}

func TestExpandRemotePath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	got, err := expandRemotePath("~/tokens/prod")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "tokens", "prod"), got)
	got, err = expandRemotePath("~")
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(home), got)
	_, err = expandRemotePath("relative/path")
	require.Error(t, err)
	abs := filepath.Join(t.TempDir(), "x")
	got, err = expandRemotePath(abs)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(abs), got)
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

func TestAPIRemoteClientCertPairingAndPathRules(t *testing.T) {
	base := APIRemote{URL: "https://prod.test", TokenEnv: "T"}
	withCert := base
	withCert.ClientCert = filepath.Join(t.TempDir(), "c.pem")
	require.Error(t, ValidateAPIRemote(withCert), "cert without key")
	withCert.ClientKey = filepath.Join(t.TempDir(), "k.pem")
	require.NoError(t, ValidateAPIRemote(withCert))
	relative := base
	relative.CA = "relative/ca.pem"
	require.Error(t, ValidateAPIRemote(relative))
	tilde := base
	tilde.CA = "~/ca.pem"
	require.NoError(t, ValidateAPIRemote(tilde))
	for _, bad := range []string{"https://user:pw@prod.test", "https://prod.test?x=1", "https://prod.test#f", "ftp://prod.test", "https://", "://bad"} {
		remote := base
		remote.URL = bad
		require.Error(t, ValidateAPIRemote(remote), "url %q", bad)
	}
	reserved := APIRemote{URL: "https://prod.test", TokenEnv: "RUSH_TOKENS"}
	require.Error(t, ValidateAPIRemote(reserved), "RUSH_TOKENS is not a client token variable")
	cleartext := APIRemote{URL: "http://prod.test", TokenEnv: "T"}
	require.Error(t, ValidateAPIRemote(cleartext))
	cleartext.AllowCleartext = true
	require.NoError(t, ValidateAPIRemote(cleartext))
	loopback := APIRemote{URL: "http://127.0.0.1:8080", TokenEnv: "T"}
	require.NoError(t, ValidateAPIRemote(loopback))
}

func TestAPIRemoteRejectsBothCredentialSources(t *testing.T) {
	both := APIRemote{URL: "https://prod.test", TokenEnv: "T", TokenFile: filepath.Join(t.TempDir(), "t")}
	require.Error(t, ValidateAPIRemote(both))
}

func TestAPIRemoteTokenLookingNameGetsRotationAdvice(t *testing.T) {
	token := "rush_12345678." + strings.Repeat("a", 43)
	err := validateAPIRemoteName(token)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rotate")
	require.NotContains(t, err.Error(), token)
	err = ValidateAPIRemote(APIRemote{URL: "https://prod.test", TokenEnv: token})
	require.Error(t, err)
	require.Contains(t, err.Error(), "rotate")
}
