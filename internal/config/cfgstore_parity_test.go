package config

// Parity tests for the s0-w85-cfgstore duplicate audit (resolution 2): the
// root keeps historical copies of store helpers that the cfgstore leaf also
// owns. Each test runs BOTH implementations over the same input table and
// fails on divergence, so neither copy can silently rot. The leaf side is
// reached through the cfgstore *ForTest wrappers (exports_surface.go).

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"
	"github.com/stretchr/testify/require"
)

// TestParitySetDefaults runs the root setDefaults copy and the leaf copy over
// the same input table and requires byte-identical marshalled results.
func TestParitySetDefaults(t *testing.T) {
	type input struct {
		cfg        func() *Config
		workingDir string
		dataDir    string
	}
	mk := func(mut func(*Config)) *Config {
		c := &Config{}
		if mut != nil {
			mut(c)
		}
		return c
	}
	table := []input{
		{func() *Config { return mk(nil) }, "", ""},
		{func() *Config { return mk(nil) }, "C:/parity-workdir", ""},
		{func() *Config {
			return mk(func(c *Config) {
				c.Options = &cfgmodel.Options{DataDirectory: "custom-data"}
				c.Options.ContextPaths = []string{"CLAUDE.md", "AGENTS.md", "CLAUDE.md"}
			})
		}, "C:/parity-workdir", "explicit-data"},
		{func() *Config {
			return mk(func(c *Config) {
				c.Options = &cfgmodel.Options{}
				c.Options.SkillsPaths = []string{"skills"}
				c.MCP = map[string]cfgmodel.MCPConfig{"kept": {Type: "http", URL: "https://x.example"}}
			})
		}, "C:/parity-workdir-2", ""},
		{func() *Config {
			return mk(func(c *Config) {
				c.Options = &cfgmodel.Options{Attribution: &cfgmodel.Attribution{CoAuthoredBy: boolPtr(true)}}
			})
		}, "", ""},
		{func() *Config {
			return mk(func(c *Config) {
				c.Options = &cfgmodel.Options{Attribution: &cfgmodel.Attribution{CoAuthoredBy: boolPtr(false)}}
			})
		}, "", ""},
	}
	for i, in := range table {
		a := in.cfg()
		b := in.cfg()
		setDefaults(a, in.workingDir, in.dataDir)
		cfgstore.SetDefaultsForTest(b, in.workingDir, in.dataDir)
		require.JSONEq(t, string(mustMarshalConfig(a)), string(cfgstore.MustMarshalConfigForTest(b)),
			"setDefaults copy divergence on table row %d", i)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestParityCandidateInputsChanged pins the root fingerprint copy against the
// leaf copy. The candidate path sets are the discovered ones, so the path-set
// short circuit is NOT taken, and the recorded fingerprints come from the
// production recorder (loadConfigCandidateStableForWorkingDir), so they are
// spelled exactly as the comparison re-reads them (Windows runners return 8.3
// short temp paths). Rows: nothing recorded and nothing changed (false), then
// the file rewritten after recording (true); dropping the fingerprint
// comparison from either copy fails the second row.
func TestParityCandidateInputsChanged(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	configPaths := docread.LookupConfigCandidates(dir)
	externalPaths := docread.McpJSONCandidatePaths(dir)
	require.NotEmpty(t, configPaths)

	_, _, recorded, _, err := loadConfigCandidateStableForWorkingDir(configPaths, dir)
	require.NoError(t, err)
	require.NotEmpty(t, recorded)

	check := func(name string, fps map[string]reloadFileFingerprint, want bool) {
		t.Helper()
		root := candidateInputsChanged(dir, configPaths, externalPaths, fps)
		leaf := cfgstore.CandidateInputsChangedForTest(dir, configPaths, externalPaths, fps)
		require.Equal(t, want, root, "root candidateInputsChanged on %q", name)
		require.Equal(t, want, leaf, "leaf candidateInputsChanged on %q", name)
	}
	check("nothing recorded", nil, false)
	check("recorded, nothing changed", recorded, false)

	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4-changed"}}`)
	check("recorded, file rewritten", recorded, true)

	// A candidate set that differs from the discovered one is itself a change
	// in both copies.
	require.True(t, candidateInputsChanged(dir, []string{dir + "/other.json"}, externalPaths, nil))
	require.True(t, cfgstore.CandidateInputsChangedForTest(dir, []string{dir + "/other.json"}, externalPaths, nil))
}

func stabDoc(path string, data []byte, digest [32]byte, exists bool) stableConfigDocument {
	return stableConfigDocument{Path: path, Data: data, Fingerprint: docread.Fingerprint{Exists: exists, Digest: digest}}
}

// TestParityMCPFingerprintsRevisionsAndNames covers initialMCPRevisions,
// mcpInputFingerprints, mcpRevisionDiff and mcpNames in both copies.
func TestParityMCPFingerprintsRevisionsAndNames(t *testing.T) {
	rushDoc := stabDoc("C:/parity/rush.json", []byte(`{"mcp":{"a":{"type":"http","url":"https://a.example"},"b":{"type":"http","url":"https://b.example"}}}`), [32]byte{9}, true)
	extDoc := stabDoc("C:/parity/.mcp.json", []byte(`{"mcpServers":{"c":{"type":"http","url":"https://c.example"}}}`), [32]byte{}, true)
	ktavDoc := stabDoc("C:/parity/rush.ktav", []byte(`{"mcp":{"a":{"type":"http","url":"https://a2.example"}}}`), [32]byte{8}, true)

	var rushRoot, rushLeaf []stableConfigDocument
	var extRoot, extLeaf []stableConfigDocument
	rushRoot = append(rushRoot, rushDoc, ktavDoc)
	rushLeaf = append(rushLeaf, rushDoc, ktavDoc)
	extRoot = append(extRoot, extDoc)
	extLeaf = append(extLeaf, extDoc)

	rootFps := mcpInputFingerprints(rushRoot, extRoot)
	leafFps := cfgstore.MCPInputFingerprintsForTest(rushLeaf, extLeaf)
	require.Equal(t, rootFps, leafFps, "mcpInputFingerprints copy divergence")

	cfgWith := func(urls map[string]string) *Config {
		c := &Config{MCP: map[string]cfgmodel.MCPConfig{}}
		for name, u := range urls {
			c.MCP[name] = cfgmodel.MCPConfig{Type: "http", URL: u}
		}
		return c
	}
	previous := cfgWith(map[string]string{"a": "https://a.example", "b": "https://b.example"})
	nextSame := cfgWith(map[string]string{"a": "https://a.example", "b": "https://b.example"})
	nextChanged := cfgWith(map[string]string{"a": "https://a2.example", "b": "https://b.example"})

	require.Equal(t, initialMCPRevisions(previous), cfgstore.InitialMCPRevisionsForTest(previous),
		"initialMCPRevisions copy divergence")
	require.Equal(t, mcpNames(previous), cfgstore.MCPNamesForTest(previous),
		"mcpNames copy divergence")

	for i, tc := range []struct {
		current map[string]uint64
		pc, nc  *Config
		force   []string
	}{
		{nil, previous, nextSame, nil},
		{nil, previous, nextChanged, nil},
		{map[string]uint64{"a": 5}, previous, nextChanged, []string{"b"}},
		{map[string]uint64{"z": 2}, previous, nextSame, []string{""}},
	} {
		pi := rootFps
		ni := leafFps
		rootGot := mcpRevisionDiff(tc.current, tc.pc, tc.nc, pi, ni, tc.force...)
		leafGot := cfgstore.MCPRevisionDiffForTest(tc.current, tc.pc, tc.nc, pi, ni, tc.force...)
		require.Equal(t, rootGot, leafGot, "mcpRevisionDiff copy divergence on row %d", i)
	}
}

// TestParityMustMarshalAndResolverInput covers mustMarshalConfig and
// resolverInputFingerprint in both copies.
func TestParityMustMarshalAndResolverInput(t *testing.T) {
	c := &Config{MCP: map[string]cfgmodel.MCPConfig{"x": {Type: "http", URL: "https://x.example"}}}
	data, err := json.Marshal(c)
	require.NoError(t, err)
	require.Equal(t, data, cfgstore.MustMarshalConfigForTest(c), "mustMarshalConfig copy divergence")

	for _, values := range [][]string{nil, {"z"}, {"b", "a"}, {"a", "b", "a"}} {
		require.Equal(t, resolverInputFingerprint(values), cfgstore.ResolverInputFingerprintForTest(values),
			"resolverInputFingerprint copy divergence for %v", values)
	}
}

// TestParityLoadConfigCandidateStableForWorkingDir runs both copies of
// loadConfigCandidateStableForWorkingDir over the same on-disk fixture.
func TestParityLoadConfigCandidateStableForWorkingDir(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-parity"}}`)

	rootCfg, rootLoaded, rootFps, rootDocs, rootErr := loadConfigCandidateStableForWorkingDir([]string{configPath}, dir)
	leafCfg, leafLoaded, leafFps, leafDocs, leafErr := cfgstore.LoadConfigCandidateStableForWorkingDirForTest([]string{configPath}, dir)
	require.NoError(t, rootErr)
	require.NoError(t, leafErr)
	require.JSONEq(t, string(mustMarshalConfig(rootCfg)), string(cfgstore.MustMarshalConfigForTest(leafCfg)),
		"loadConfigCandidateStableForWorkingDir loaded-config divergence")
	require.Equal(t, rootLoaded, leafLoaded, "loadedPaths divergence")
	require.Equal(t, rootDocs, leafDocs, "stable documents divergence")
	require.Equal(t, len(rootFps), len(leafFps), "fingerprint map size divergence")
	for k, v := range rootFps {
		require.Equal(t, v, leafFps[k], "fingerprint divergence for %s", k)
	}
}
