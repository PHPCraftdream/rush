package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPMutationPreservesUnrelatedDocumentBytes(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	path := GlobalConfigData()
	original := []byte("{\r\n  \"before\": {\"sentinel\": \"prefix\"},\r\n  \"mcp\": {\r\n    \"first\": {\"type\":\"http\",\"url\":\"http://first\"},\r\n    \"middle\": {\"type\":\"http\",\"url\":\"http://middle\"},\r\n    \"last\": {\"type\":\"http\",\"url\":\"http://last\"}\r\n  },\r\n  \"after\": [\"sentinel\", 7]\r\n}")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, original, 0o600))
	require.NoError(t, store.ReloadFromDisk(t.Context()))

	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, "middle", true))
	updated, err := os.ReadFile(path)
	require.NoError(t, err)

	prefix := []byte("{\r\n  \"before\": {\"sentinel\": \"prefix\"},\r\n  \"mcp\": {\r\n    \"first\": {\"type\":\"http\",\"url\":\"http://first\"},\r\n    \"middle\": ")
	suffix := []byte(",\r\n    \"last\": {\"type\":\"http\",\"url\":\"http://last\"}\r\n  },\r\n  \"after\": [\"sentinel\", 7]\r\n}")
	require.True(t, bytes.HasPrefix(updated, prefix), "the untouched prefix must remain byte-identical")
	require.True(t, bytes.HasSuffix(updated, suffix), "the untouched suffix must remain byte-identical")
	require.True(t, bytes.Contains(updated, []byte("\r\n")), "CRLF formatting must not be normalized")
	require.False(t, bytes.HasSuffix(updated, []byte("\n")), "a document without a final newline must stay without one")
	require.True(t, json.Valid(updated))

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	middle, ok := store.MCPConfig("middle")
	require.True(t, ok)
	require.True(t, middle.Disabled)
	first, ok := store.MCPConfig("first")
	require.True(t, ok)
	require.Equal(t, "http://first", first.URL)
}

func TestMCPDocumentEditorMutationsPreserveContainersAndLiteralNames(t *testing.T) {
	name := "literal.foo/slash\\\"\u2603"
	nameJSON, err := json.Marshal(name)
	require.NoError(t, err)
	compact := append([]byte(`{"sentinel":"prefix", "mcp":{"first":{"type":"http"},`), nameJSON...)
	compact = append(compact, []byte(`:{"type":"http","url":"old"},"last":{"type":"http"}}, "tail":{"keep":true}}`)...)
	value := MCPConfig{Type: MCPHttp, URL: "http://new"}

	updated, err := editMCPDocument(compact, "rush.json", "replace", name, "renamed/\u2603", value, nil, nil)
	require.NoError(t, err)
	require.True(t, json.Valid(updated))
	require.True(t, bytes.Contains(updated, []byte(`"sentinel":"prefix"`)))
	require.True(t, bytes.Contains(updated, []byte(`"tail":{"keep":true}`)))
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(updated, &root))
	require.NotContains(t, root.MCP, name)
	require.Contains(t, root.MCP, "renamed/\u2603")
	require.Contains(t, root.MCP, "first")
	require.Contains(t, root.MCP, "last")

	for _, operation := range []string{"first", "renamed/\u2603", "last"} {
		removed, removeErr := editMCPDocument(updated, "rush.json", "remove", operation, operation, MCPConfig{}, nil, nil)
		require.NoError(t, removeErr)
		require.True(t, json.Valid(removed))
		updated = removed
	}
	var emptyRoot struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(updated, &emptyRoot))
	require.Empty(t, emptyRoot.MCP)
	require.True(t, bytes.Contains(updated, []byte(`"sentinel":"prefix"`)))
	require.True(t, bytes.Contains(updated, []byte(`"tail":{"keep":true}`)))
}

func TestMCPDocumentEditorSupportsEmptyMissingAndExternalContainers(t *testing.T) {
	for _, fixture := range [][]byte{
		{},
		[]byte(`{"prefix":1}`),
		[]byte(`{"mcp":{}}`),
		[]byte(`{"mcp":null}`),
	} {
		updated, err := editMCPDocument(fixture, "rush.json", "add", "new.server", "new.server", MCPConfig{Type: MCPHttp, URL: "http://new"}, nil, nil)
		require.NoError(t, err)
		require.True(t, json.Valid(updated))
		require.Equal(t, 1, bytes.Count(updated, []byte(`"mcp"`)))
		var root struct {
			MCP map[string]MCPConfig `json:"mcp"`
		}
		require.NoError(t, json.Unmarshal(updated, &root))
		require.Equal(t, "http://new", root.MCP["new.server"].URL)
	}

	external := []byte(`{"prefix":"keep", "mcpServers":{"first":{"type":"http","url":"first"},"target":{"type":"http","url":"old"}}, "suffix":"keep"}`)
	updated, err := editMCPDocument(external, filepath.Join("root", ".mcp.json"), "disable", "target", "target", MCPConfig{}, ptr(true), nil)
	require.NoError(t, err)
	require.True(t, json.Valid(updated))
	require.True(t, bytes.Contains(updated, []byte(`"prefix":"keep"`)))
	require.True(t, bytes.Contains(updated, []byte(`"suffix":"keep"`)))
	var root struct {
		MCPServers map[string]map[string]json.RawMessage `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(updated, &root))
	require.Contains(t, root.MCPServers, "first")
	var disabled bool
	require.NoError(t, json.Unmarshal(root.MCPServers["target"]["disabled"], &disabled))
	require.True(t, disabled)
}

func TestMCPDocumentEditorRetainsDuplicateKeyPolicy(t *testing.T) {
	data := []byte(`{"mcp":{"duplicate":{"type":"http","url":"first"},"keep":{"type":"http"},"duplicate":{"type":"http","url":"last"}}}`)
	require.Equal(t, 2, mcpDocumentEntryKeyCount(t, data, "duplicate"))
	require.Equal(t, 1, mcpDocumentEntryKeyCount(t, data, "keep"))

	removed, err := editMCPDocument(data, "rush.json", "remove", "duplicate", "duplicate", MCPConfig{}, nil, nil)
	require.NoError(t, err)
	var removedRoot struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(removed, &removedRoot))
	require.Equal(t, 0, mcpDocumentEntryKeyCount(t, removed, "duplicate"))
	require.Equal(t, 1, mcpDocumentEntryKeyCount(t, removed, "keep"))
	require.Len(t, removedRoot.MCP, 1)
	require.NotContains(t, removedRoot.MCP, "duplicate")
	require.Contains(t, removedRoot.MCP, "keep")

	replaced, err := editMCPDocument(data, "rush.json", "replace", "duplicate", "renamed", MCPConfig{Type: MCPHttp, URL: "new"}, nil, nil)
	require.NoError(t, err)
	var replacedRoot struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(replaced, &replacedRoot))
	require.Equal(t, 0, mcpDocumentEntryKeyCount(t, replaced, "duplicate"))
	require.Equal(t, 1, mcpDocumentEntryKeyCount(t, replaced, "renamed"))
	require.Equal(t, 1, mcpDocumentEntryKeyCount(t, replaced, "keep"))
	require.Len(t, replacedRoot.MCP, 2)
	require.NotContains(t, replacedRoot.MCP, "duplicate")
	require.Contains(t, replacedRoot.MCP, "renamed")
	require.Contains(t, replacedRoot.MCP, "keep")
}

func TestMCPDocumentEditorScalesAlternatingDuplicateMembers(t *testing.T) {
	const memberPairs = 128

	var source, removeWant, renameWant strings.Builder
	source.WriteString("{\r\n  \"prefix\": 1,\r\n  \"mcp\": {\r\n")
	removeWant.WriteString("{\r\n  \"prefix\": 1,\r\n  \"mcp\": {\r\n")
	renameWant.WriteString("{\r\n  \"prefix\": 1,\r\n  \"mcp\": {\r\n")
	for index := 0; index < memberPairs; index++ {
		fmt.Fprintf(&source, "    \"target\": {\"type\":\"http\",\"url\":\"target-%03d\"},\r\n", index)
		fmt.Fprintf(&source, "    \"keep-%03d\": {\"type\":\"http\",\"url\":\"keep-%03d\"}", index, index)
		fmt.Fprintf(&removeWant, "    \"keep-%03d\": {\"type\":\"http\",\"url\":\"keep-%03d\"}", index, index)
		fmt.Fprintf(&renameWant, "    \"keep-%03d\": {\"type\":\"http\",\"url\":\"keep-%03d\"}", index, index)
		if index+1 < memberPairs {
			source.WriteString(",\r\n")
			removeWant.WriteString(",\r\n")
			renameWant.WriteString(",\r\n")
		}
	}
	source.WriteString("\r\n  },\r\n  \"suffix\": 2\r\n}\r\n")
	removeWant.WriteString("\r\n  },\r\n  \"suffix\": 2\r\n}\r\n")
	renameWant.WriteString(",\r\n    \"renamed\":{\"type\":\"http\",\"url\":\"http://renamed\"}\r\n  },\r\n  \"suffix\": 2\r\n}\r\n")

	data := []byte(source.String())
	removed, err := editMCPDocument(data, "rush.json", "remove", "target", "target", MCPConfig{}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, removeWant.String(), string(removed))
	require.True(t, json.Valid(removed))
	require.Equal(t, 0, mcpDocumentEntryKeyCount(t, removed, "target"))
	var removedRoot struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(removed, &removedRoot))
	require.Len(t, removedRoot.MCP, memberPairs)

	renameValue := MCPConfig{Type: MCPHttp, URL: "http://renamed"}
	rename, err := editMCPDocument(data, "rush.json", "replace", "target", "renamed", renameValue, nil, nil)
	require.NoError(t, err)
	require.Equal(t, renameWant.String(), string(rename))
	require.True(t, json.Valid(rename))
	require.Equal(t, 0, mcpDocumentEntryKeyCount(t, rename, "target"))
	require.Equal(t, 1, mcpDocumentEntryKeyCount(t, rename, "renamed"))
}

func TestApplyMCPJSONEditsHandlesArbitraryOrderAdjacentGrowthShrinkAndAliases(t *testing.T) {
	data := []byte("0123456789")
	edits := []mcpJSONEdit{
		{start: 6, end: 8, replacement: []byte("LONG")},
		{start: 2, end: 4, replacement: data[8:10]},
		{start: 4, end: 6},
	}
	originalEdits := slices.Clone(edits)

	got := applyMCPJSONEdits(data, edits...)

	require.Equal(t, []byte("0189LONG89"), got)
	require.Equal(t, originalEdits, edits)
}

func TestApplyMCPJSONEditsHandlesZeroAndOneEdit(t *testing.T) {
	data := []byte("abc")

	require.Equal(t, data, applyMCPJSONEdits(data))
	require.Equal(t, []byte("aXc"), applyMCPJSONEdits(data, mcpJSONEdit{
		start: 1, end: 2, replacement: []byte("X"),
	}))
}

func TestApplyMCPJSONEditsPreservesEmptyAndCRLFBytes(t *testing.T) {
	data := []byte("left\r\nold\r\nright")
	got := applyMCPJSONEdits(data, mcpJSONEdit{
		start:       len("left\r\n"),
		end:         len("left\r\nold"),
		replacement: []byte("new\r\nvalue"),
	})

	require.Equal(t, []byte("left\r\nnew\r\nvalue\r\nright"), got)
}

func TestApplyMCPJSONEditsRejectsInvalidSpans(t *testing.T) {
	data := []byte("012345")
	for name, edits := range map[string][]mcpJSONEdit{
		"negative start":   {{start: -1, end: 1}},
		"end before start": {{start: 3, end: 2}},
		"end beyond data":  {{start: 1, end: len(data) + 1}},
		"overlap":          {{start: 1, end: 4}, {start: 3, end: 5}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Panics(t, func() {
				applyMCPJSONEdits(data, edits...)
			})
		})
	}
}

func TestApplyMCPJSONEditsAllocationsDoNotScalePerEdit(t *testing.T) {
	makeEdits := func(count int) []mcpJSONEdit {
		edits := make([]mcpJSONEdit, count)
		for index := range edits {
			edits[index] = mcpJSONEdit{start: index * 2, end: index*2 + 1, replacement: []byte("x")}
		}
		return edits
	}

	smallData := bytes.Repeat([]byte("a."), 8)
	largeData := bytes.Repeat([]byte("a."), 64)
	smallEdits := makeEdits(8)
	largeEdits := makeEdits(64)
	var got []byte
	smallAllocs := testing.AllocsPerRun(100, func() {
		got = applyMCPJSONEdits(smallData, smallEdits...)
	})
	largeAllocs := testing.AllocsPerRun(100, func() {
		got = applyMCPJSONEdits(largeData, largeEdits...)
	})
	require.NotEmpty(t, got)
	require.LessOrEqual(t, largeAllocs, smallAllocs+2,
		"edit assembly allocations must be independent of edit count")
}

func mcpDocumentEntryKeyCount(t *testing.T, data []byte, key string) int {
	t.Helper()
	root, err := scanMCPJSONObject(data)
	require.NoError(t, err)
	containerMember := lastMCPMember(root.members, "mcp")
	require.NotNil(t, containerMember)
	_, container, err := scanMCPJSONValue(data, containerMember.valueStart)
	require.NoError(t, err)
	require.NotNil(t, container)

	count := 0
	for _, member := range container.members {
		if member.key == key {
			count++
		}
	}
	return count
}

func TestMCPDocumentEditorRemovalPreservesClosingWhitespace(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want []byte
	}{
		{
			name: "sole LF without final newline",
			data: []byte("{\"mcp\":{\n  \"only\":{\"type\":\"http\"}\n  },\"tail\":\"keep\"}"),
			want: []byte("{\"mcp\":{\n  },\"tail\":\"keep\"}"),
		},
		{
			name: "sole CRLF with final newline",
			data: []byte("{\r\n  \"mcp\": {\r\n    \"only\": {\"type\":\"http\"}\r\n  }\r\n}\r\n"),
			want: []byte("{\r\n  \"mcp\": {\r\n  }\r\n}\r\n"),
		},
		{
			name: "all duplicate entries",
			data: []byte("{\"mcp\":{\r\n  \"dup\":{\"type\":\"http\"},\r\n  \"dup\":{\"type\":\"http\"}\r\n  },\"tail\":1}"),
			want: []byte("{\"mcp\":{\r\n  },\"tail\":1}"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := editMCPDocument(test.data, "rush.json", "remove", "only", "only", MCPConfig{}, nil, nil)
			if test.name == "all duplicate entries" {
				got, err = editMCPDocument(test.data, "rush.json", "remove", "dup", "dup", MCPConfig{}, nil, nil)
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
			require.True(t, json.Valid(got))
		})
	}
}

func TestMCPDocumentEditorIgnoresNestedNewlinesWhenInferringLayout(t *testing.T) {
	root := []byte("{\"sentinel\":{\"nested\":[\n1,\n2\n]}}")
	updated, err := editMCPDocument(root, "rush.json", "add", "added", "added", MCPConfig{Type: MCPHttp}, nil, nil)
	require.NoError(t, err)
	require.True(t, json.Valid(updated))
	require.True(t, bytes.HasPrefix(updated, root[:len(root)-1]), "root bytes before the new container must remain unchanged")

	inlineMCP := []byte("{\"before\":1,\"mcp\":{\"existing\":{\"env\":{\n    \"A\":\"B\"\n  }}},\"after\":2}")
	updated, err = editMCPDocument(inlineMCP, "rush.json", "add", "added", "added", MCPConfig{Type: MCPHttp}, nil, nil)
	require.NoError(t, err)
	require.True(t, json.Valid(updated))
	require.True(t, bytes.Contains(updated, []byte("\"env\":{\n    \"A\":\"B\"\n  }")))
	require.True(t, bytes.HasPrefix(updated, []byte("{\"before\":1,")))
	require.True(t, bytes.HasSuffix(updated, []byte(",\"after\":2}")))
}

func TestRushJSONMutationDoesNotSelectUnrelatedMCPServersKey(t *testing.T) {
	store, _ := isolatedMCPConfigStore(t)
	path := GlobalConfigData()
	original := []byte(`{"mcpServers":{"sentinel":{"type":"http","url":"ignored"}},"tail":{"keep":true}}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, original, 0o600))
	require.NoError(t, store.ReloadFromDisk(t.Context()))

	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "added", MCPConfig{Type: MCPHttp, URL: "http://added"}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	beforeMCPServers := []byte(`"mcpServers":{"sentinel":{"type":"http","url":"ignored"}}`)
	require.True(t, bytes.Contains(data, beforeMCPServers))
	require.NotEqual(t, -1, bytes.Index(data, []byte(`"mcp":{"added"`)))

	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, "added", true))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	added, ok := store.MCPConfig("added")
	require.True(t, ok)
	require.True(t, added.Disabled)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Contains(after, beforeMCPServers), "the ignored mcpServers field must remain byte-identical")
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(after, &root))
	require.Contains(t, root, "mcp")
	require.Contains(t, root, "mcpServers")
}

func TestRushLogicalSymlinkUsesRushSchemaForPhysicalMCPName(t *testing.T) {
	store, root := isolatedMCPConfigStore(t)
	logical := GlobalConfigData()
	physical := filepath.Join(root, "physical", ".mcp.json")
	original := []byte(`{"mcpServers":{"sentinel":{"type":"http","url":"untouched"}},"mcp":{"old":{"type":"http","url":"old"}}}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	if err := os.MkdirAll(filepath.Dir(logical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "added", MCPConfig{Type: MCPHttp, URL: "http://added"}))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	added, ok := store.MCPConfig("added")
	require.True(t, ok)
	require.Equal(t, "http://added", added.URL)

	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, "added", true))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	added, ok = store.MCPConfig("added")
	require.True(t, ok)
	require.True(t, added.Disabled)

	require.NoError(t, store.PersistReplaceMCPInScope(ScopeGlobal, "old", "replaced", MCPConfig{Type: MCPHttp, URL: "http://replaced"}))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	_, ok = store.MCPConfig("old")
	require.False(t, ok)
	replaced, ok := store.MCPConfig("replaced")
	require.True(t, ok)
	require.Equal(t, "http://replaced", replaced.URL)

	require.NoError(t, store.PersistRemoveMCPConfig(ScopeGlobal, "replaced"))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	_, ok = store.MCPConfig("replaced")
	require.False(t, ok)
	after, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Contains(t, string(after), `"mcpServers":{"sentinel":{"type":"http","url":"untouched"}}`)
	require.NotContains(t, string(after), `"mcpServers":{"added"`)
	var document map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(after, &document))
	require.Contains(t, document, "mcp")
}

func TestMCPTransactionUsesRushSchemaForPhysicalMCPBasename(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	original := []byte(`{"mcpServers":{"sentinel":{"type":"http","url":"untouched"}}}`)
	files := &mcpLockedFiles{
		data:         make(map[string][]byte),
		present:      make(map[string]bool),
		changed:      make(map[string]bool),
		fingerprints: make(map[string]reloadFileFingerprint),
		pathRecords:  make(map[string]string),
		records:      make(map[string]*mcpFileRecord),
	}
	files.bindMCPPath(path, original, true, dataFingerprint(path, original))

	require.NoError(t, (&ConfigStore{}).prepareMCPFileMutation(files, path, "add", "added", "added", MCPConfig{
		Type: MCPHttp, URL: "http://added",
	}, nil))
	require.NoError(t, updateMCPFile(files, path, "added", func(entry map[string]any) error {
		entry["timeout"] = 30
		return nil
	}))

	updated := files.mcpData(path)
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(updated, &root))
	require.Equal(t, json.RawMessage(`{"sentinel":{"type":"http","url":"untouched"}}`), root["mcpServers"])
	var mcp map[string]MCPConfig
	require.NoError(t, json.Unmarshal(root["mcp"], &mcp))
	require.Equal(t, "http://added", mcp["added"].URL)
	require.Equal(t, 30, mcp["added"].Timeout)
}

func TestMCPDocumentEditorMutatesNullEntryWithoutLosingUnrelatedBytes(t *testing.T) {
	data := []byte(`{"before":"sentinel","mcp":{"null-entry":null,"sibling":{"type":"http"}},"after":"sentinel"}`)
	disabled, err := editMCPDocument(data, "rush.json", "disable", "null-entry", "null-entry", MCPConfig{}, ptr(true), nil)
	require.NoError(t, err)
	require.True(t, json.Valid(disabled))
	require.True(t, bytes.Contains(disabled, []byte(`"before":"sentinel"`)))
	require.True(t, bytes.Contains(disabled, []byte(`"after":"sentinel"`)))
	var disabledRoot struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(disabled, &disabledRoot))
	var disabledEntry map[string]any
	require.NoError(t, json.Unmarshal(disabledRoot.MCP["null-entry"], &disabledEntry))
	require.Equal(t, true, disabledEntry["disabled"])
	require.Contains(t, disabledRoot.MCP, "sibling")

	updated, err := editMCPDocument(data, "rush.json", "update", "null-entry", "null-entry", MCPConfig{}, nil, func(entry map[string]any) error {
		entry["url"] = "http://updated"
		return nil
	})
	require.NoError(t, err)
	require.True(t, json.Valid(updated))
	require.True(t, bytes.Contains(updated, []byte(`"before":"sentinel"`)))
	require.True(t, bytes.Contains(updated, []byte(`"after":"sentinel"`)))
	require.NoError(t, json.Unmarshal(updated, &disabledRoot))
	var updatedEntry map[string]any
	require.NoError(t, json.Unmarshal(disabledRoot.MCP["null-entry"], &updatedEntry))
	require.Equal(t, "http://updated", updatedEntry["url"])

	removed, err := editMCPDocument([]byte(`{"mcp":{"raw":"not-an-object"},"tail":1}`), "rush.json", "remove", "raw", "raw", MCPConfig{}, nil, nil)
	require.NoError(t, err)
	require.True(t, json.Valid(removed))
	require.NotContains(t, string(removed), `"raw"`)
}

func TestMCPContainerNameUsesOnlyFileKind(t *testing.T) {
	require.Equal(t, "mcp", mcpContainerName("rush.json"))
	require.Equal(t, "mcp", mcpContainerName(filepath.Join("root", "config.json")))
	require.Equal(t, "mcpServers", mcpContainerName(filepath.Join("root", ".mcp.json")))
}

func ptr(value bool) *bool {
	return &value
}
