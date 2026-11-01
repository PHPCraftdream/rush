package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

func isolateListenerConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(GlobalDirEnv, root)
	t.Setenv("RUSH_GLOBAL_CONFIG", "")
	t.Setenv("RUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	return root
}

func TestGlobalServerListenersReadErrorKind(t *testing.T) {
	root := isolateListenerConfig(t)
	path := filepath.Join(root, "config", "rush.json")
	require.NoError(t, os.Mkdir(path, 0o700))

	_, err := GlobalServerListeners(ServerListenerFlags{})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrListenerConfigRead))
	require.False(t, errors.Is(err, ErrListenerConfigDecode))
}

func TestGlobalServerListenersDecodeErrorKindAndPath(t *testing.T) {
	root := isolateListenerConfig(t)
	path := filepath.Join(root, "config", "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"server":{"listeners":[`), 0o600))

	_, err := GlobalServerListeners(ServerListenerFlags{})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrListenerConfigDecode))
	require.False(t, errors.Is(err, ErrListenerConfigRead))
	require.Contains(t, err.Error(), path)
}

func TestServerListenerMaxBodySizeSuffixes(t *testing.T) {
	isolateListenerConfig(t)
	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{name: "mib", value: "1MiB", want: 1024 * 1024},
		{name: "kib", value: "2KiB", want: 2 * 1024},
		{name: "gib", value: "3GiB", want: 3 * 1024 * 1024 * 1024},
		{name: "mb", value: "4MB", want: 4 * 1000 * 1000},
		{name: "kb", value: "5KB", want: 5 * 1000},
		{name: "gb", value: "6GB", want: 6 * 1000 * 1000 * 1000},
		{name: "bytes", value: "7B", want: 7},
		{name: "mixed case and whitespace", value: "  1 mIb  ", want: 1024 * 1024},
		{name: "bare number", value: "42", want: 42},
		{name: "unknown suffix", value: "1tb", wantErr: true},
		{name: "integer overflow", value: "9223372036854775808", wantErr: true},
		{name: "multiplier overflow", value: "9223372036854775807kb", wantErr: true},
	}

	check := func(tc struct {
		name    string
		value   string
		want    int64
		wantErr bool
	},
	) {
		t.Helper()
		var limits ServerListenerLimits
		err := json.Unmarshal([]byte(`{"max_body":`+strconv.Quote(tc.value)+`}`), &limits)
		if tc.wantErr {
			require.Error(t, err, tc.name)
			return
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, limits.MaxBody, tc.name)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { check(tc) })
	}
	for iteration := 0; iteration < 200; iteration++ {
		for _, tc := range tests {
			check(tc)
		}
	}
}

func TestGlobalServerListenersGlobalOnlyAndFlags(t *testing.T) {
	root := isolateListenerConfig(t)
	configDir := filepath.Join(root, "config")
	global := `{"server":{"listeners":[{"addr":"127.0.0.1:8000","allow_admin":true,"tls":{"cert":"old.pem","min_version":"1.2"}}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "rush.json"), []byte(global), 0o600))
	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "rush.json"), []byte(`{"server":{"listeners":[{"addr":"0.0.0.0:9999","allow_admin":false}]}}`), 0o600))
	workspace := filepath.Join(project, ".rush")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "rush.json"), []byte(`{"server":{"listeners":[{"addr":"192.0.2.1:9998","allow_admin":false}]}}`), 0o600))
	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(project))
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	addr, cert, minVersion := "127.0.0.1:8443", "new.pem", "1.3"
	got, err := GlobalServerListeners(ServerListenerFlags{Addr: &addr, TLSCert: &cert, TLSMinVersion: &minVersion})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "127.0.0.1:8443", got[0].Addr)
	require.True(t, got[0].AllowAdmin)
	require.Equal(t, "new.pem", got[0].TLS.Cert)
	require.Equal(t, "1.3", got[0].TLS.MinVersion)
}

func TestGlobalServerListenersJSONAndKtav(t *testing.T) {
	for _, format := range []string{"json", "ktav"} {
		t.Run(format, func(t *testing.T) {
			root := isolateListenerConfig(t)
			doc := []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8443","surfaces":["rest","ws"],"allow_cleartext":true}]}}`)
			path := filepath.Join(root, "config", "rush.json")
			if format == "ktav" {
				encoded, err := ktavcodec.ConvertJSONToKtav(doc)
				require.NoError(t, err)
				doc = encoded
				path = filepath.Join(root, "config", "rush.ktav")
			}
			require.NoError(t, os.WriteFile(path, doc, 0o600))
			got, sources, err := GlobalServerListenersWithSources(ServerListenerFlags{})
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, "127.0.0.1:8443", got[0].Addr)
			require.Equal(t, []string{"rest", "ws"}, got[0].Surfaces)
			require.True(t, got[0].AllowCleartext)
			require.Equal(t, "global config", sources[0]["addr"])
			require.Equal(t, "global config", sources[0]["surfaces"])
			require.Equal(t, "global config", sources[0]["allow_cleartext"])
			require.Empty(t, sources[0]["allow_admin"])
			require.Empty(t, sources[0]["limits.rps"])
		})
	}
}

func TestGlobalServerListenersTracksSystemAndGlobalProvenance(t *testing.T) {
	system := []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8000","surfaces":["rest"],"limits":{"burst":2}}]}}`)
	global := []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:9000","allow_admin":true,"limits":{"rps":5}}]}}`)
	listeners, sources, err := globalServerListenersFromDocuments([]serverListenerConfigDocument{
		{data: system, source: "system config"},
		{data: global, source: "global config"},
	}, ServerListenerFlags{})
	require.NoError(t, err)
	require.Len(t, listeners, 2) // arrays append, matching jsons.Merge semantics
	require.Equal(t, "127.0.0.1:8000", listeners[0].Addr)
	require.Equal(t, "system config", sources[0]["addr"])
	require.Equal(t, "system config", sources[0]["surfaces"])
	require.Equal(t, "system config", sources[0]["limits.burst"])
	require.Equal(t, "127.0.0.1:9000", listeners[1].Addr)
	require.Equal(t, "global config", sources[1]["addr"])
	require.Equal(t, "global config", sources[1]["allow_admin"])
	require.Equal(t, "global config", sources[1]["limits.rps"])
}

func TestGlobalServerListenersFlagFalseHasFlagProvenance(t *testing.T) {
	no := false
	listeners, sources, err := globalServerListenersFromDocuments([]serverListenerConfigDocument{{
		data: []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8000","allow_cleartext":true}]}}`), source: "global config",
	}}, ServerListenerFlags{AllowCleartext: &no})
	require.NoError(t, err)
	require.False(t, listeners[0].AllowCleartext)
	require.Equal(t, "flag", sources[0]["allow_cleartext"])
}

func TestServerListenerConfiguredFieldsIncludesNestedMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
		miss []string
	}{
		{
			name: "TLS values including empty strings",
			raw:  `{"tls":{"cert":"cert.pem","key":"","min_version":""}}`,
			want: []string{"tls.cert", "tls.key", "tls.min_version"},
			miss: []string{"tls.client_ca", "limits.rps"},
		},
		{
			name: "limits including explicit zeros",
			raw:  `{"limits":{"rps":0,"burst":0,"max_conns_per_token":0,"max_body":0}}`,
			want: []string{"limits.rps", "limits.burst", "limits.max_conns_per_token", "limits.max_body"},
			miss: []string{"tls.cert"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &raw))
			fields := serverListenerConfiguredFields(raw)
			for _, field := range tc.want {
				require.True(t, fields[field], field)
			}
			for _, field := range tc.miss {
				require.False(t, fields[field], field)
			}
		})
	}
}

func TestGlobalServerListenersFieldPresenceUsesMergedConfig(t *testing.T) {
	root := isolateListenerConfig(t)
	configPath := filepath.Join(root, "config", "rush.json")
	first := []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8000","tls":{"cert":"cert.pem"},"limits":{"burst":4}}]}}`)
	second := []byte(`{"server":{"listeners":[{"allow_admin":true,"tls":{"min_version":"1.3"}}]}}`)
	dataPath := filepath.Join(root, "data", "rush.json")
	require.NoError(t, os.WriteFile(configPath, first, 0o600))
	require.NoError(t, os.WriteFile(dataPath, second, 0o600))
	got, sources, err := GlobalServerListenersWithSources(ServerListenerFlags{})
	require.NoError(t, err)
	require.Len(t, got, 2) // arrays append, matching jsons.Merge semantics
	require.Equal(t, "127.0.0.1:8000", got[0].Addr)
	require.Equal(t, "global config", sources[0]["addr"])
	require.Equal(t, "global config", sources[0]["tls.cert"])
	require.Equal(t, "global config", sources[0]["limits.burst"])
	require.Empty(t, sources[0]["allow_admin"])
	require.Equal(t, "", got[1].Addr)
	require.True(t, got[1].AllowAdmin)
	require.Equal(t, "global config", sources[1]["allow_admin"])
	require.Equal(t, "global config", sources[1]["tls.min_version"])
	require.Empty(t, sources[1]["addr"])
	require.Empty(t, sources[1]["tls.cert"])
	require.Empty(t, sources[1]["limits.burst"])
}

func TestAnnotateServerListenerSourcesNullFields(t *testing.T) {
	annotated, err := annotateServerListenerSources([]byte(`{"server":{"listeners":[{"addr":null,"allow_admin":true,"tls":{"cert":null,"key":"key.pem"},"limits":{"rps":null,"burst":4}}]}}`), "global config")
	require.NoError(t, err)
	var document struct {
		Server struct {
			Listeners []map[string]json.RawMessage `json:"listeners"`
		} `json:"server"`
	}
	require.NoError(t, json.Unmarshal(annotated, &document))
	require.Len(t, document.Server.Listeners, 1)
	listener := document.Server.Listeners[0]
	require.JSONEq(t, `null`, string(listener["addr"]))
	require.JSONEq(t, `"global config"`, string(listener["allow_admin"]))
	var tls, limits map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(listener["tls"], &tls))
	require.JSONEq(t, `null`, string(tls["cert"]))
	require.JSONEq(t, `"global config"`, string(tls["key"]))
	require.NoError(t, json.Unmarshal(listener["limits"], &limits))
	require.JSONEq(t, `null`, string(limits["rps"]))
	require.JSONEq(t, `"global config"`, string(limits["burst"]))
}

func TestGlobalServerListenersFlagSourceIsNotConfigPresence(t *testing.T) {
	root := isolateListenerConfig(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config", "rush.json"), []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8000"}]}}`), 0o600))
	addr := "127.0.0.1:9000"
	got, sources, err := GlobalServerListenersWithSources(ServerListenerFlags{Addr: &addr})
	require.NoError(t, err)
	require.Equal(t, "flag", sources[0]["addr"])
	require.Empty(t, sources[0]["allow_cleartext"])
	require.Equal(t, addr, got[0].Addr)
}

func TestGlobalServerListenersIgnoresProjectAndWorkspace(t *testing.T) {
	isolateListenerConfig(t)
	project := t.TempDir()
	workspace := filepath.Join(project, ".rush")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	valid := []byte(`{"server":{"listeners":[{"addr":"0.0.0.0:9000","allow_admin":true}]}}`)
	require.NoError(t, os.WriteFile(filepath.Join(project, "rush.json"), valid, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "rush.json"), valid, 0o600))
	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(project))
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	got, err := GlobalServerListeners(ServerListenerFlags{})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestGlobalServerListenersFlagFalseOverridesTrue(t *testing.T) {
	root := isolateListenerConfig(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config", "rush.json"), []byte(`{"server":{"listeners":[{"addr":"127.0.0.1:8000","allow_admin":true,"allow_cleartext":true}]}}`), 0o600))
	no := false
	got, err := GlobalServerListeners(ServerListenerFlags{AllowAdmin: &no, AllowCleartext: &no})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.False(t, got[0].AllowAdmin)
	require.False(t, got[0].AllowCleartext)
}
