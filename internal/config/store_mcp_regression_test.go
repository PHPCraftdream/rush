package config

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStoreMCPMutatorsPublishDetachedSnapshots(t *testing.T) {
	store := NewTestStore(&Config{MCP: MCPs{
		"one": {
			Type:          MCPStdio,
			Command:       "old",
			Args:          []string{"a"},
			Env:           map[string]string{"TOKEN": "one"},
			DisabledTools: []string{"old-tool"},
		},
	}})

	oldConfig, oldGeneration := store.Snapshot()
	oldMCP := oldConfig.MCP
	oldOne := oldMCP["one"]

	updated, ok := store.UpdateMCP("one", func(m *MCPConfig) {
		m.Command = "new"
		m.Args[0] = "changed"
		m.Env["TOKEN"] = "changed"
	})
	require.True(t, ok)
	require.Equal(t, "new", updated.Command)
	require.Equal(t, oldGeneration+1, store.Generation())
	require.Equal(t, "old", oldMCP["one"].Command)
	require.Equal(t, "a", oldOne.Args[0])
	require.Equal(t, "one", oldOne.Env["TOKEN"])

	require.True(t, store.AddMCP("two", MCPConfig{Type: MCPHttp, URL: "http://two"}))
	require.Equal(t, oldGeneration+2, store.Generation())
	removed, ok := store.RemoveMCP("two")
	require.True(t, ok)
	require.Equal(t, "http://two", removed.URL)
	require.Equal(t, oldGeneration+3, store.Generation())
	require.False(t, store.AddMCP("one", MCPConfig{}))
	require.Equal(t, oldGeneration+3, store.Generation())
}

func TestMCPAdmissionRevisionsIgnoreUnrelatedCOW(t *testing.T) {
	const name = "stable"
	value := MCPConfig{Type: MCPStdio, Command: "stable"}
	store := NewTestStore(&Config{MCP: MCPs{name: value}})

	initial := store.SnapshotMCPAdmission(name)
	store.SetSkipPermissionRequests(true)
	unchanged := store.SnapshotMCPAdmission(name)
	require.Equal(t, initial.MCPRevision, unchanged.MCPRevision)
	require.Equal(t, initial.ResolverRevision, unchanged.ResolverRevision)

	updated, ok := store.UpdateMCP(name, func(*MCPConfig) {})
	require.True(t, ok)
	require.Equal(t, value, updated)
	changed := store.SnapshotMCPAdmission(name)
	require.NotEqual(t, initial.MCPRevision, changed.MCPRevision)
	require.Equal(t, initial.ResolverRevision, changed.ResolverRevision)
	_, ok = store.RemoveMCPIfCurrent(name, value, initial.MCPRevision)
	require.False(t, ok)
	_, ok = store.MCPConfig(name)
	require.True(t, ok)
}

func TestMCPRevisionsTrackGenericCOWPerName(t *testing.T) {
	const changed = "changed"
	const unrelated = "unrelated"
	store := NewTestStore(&Config{MCP: MCPs{
		changed:   {Type: MCPStdio, Command: "old"},
		unrelated: {Type: MCPStdio, Command: "untouched"},
	}})
	initialChanged := store.SnapshotMCPAdmission(changed).MCPRevision
	initialUnrelated := store.SnapshotMCPAdmission(unrelated).MCPRevision

	store.updateConfig(func(cfg *Config) {
		cfg.MCP = maps.Clone(cfg.MCP)
		cfg.MCP[changed] = MCPConfig{Type: MCPStdio, Command: "new"}
	})

	revisedChanged := store.SnapshotMCPAdmission(changed).MCPRevision
	revisedUnrelated := store.SnapshotMCPAdmission(unrelated).MCPRevision
	require.NotEqual(t, initialChanged, revisedChanged)
	require.Equal(t, initialUnrelated, revisedUnrelated)
}

func TestMCPRevisionABAInvalidatesStagedReAdd(t *testing.T) {
	const name = "aba"
	value := MCPConfig{Type: MCPStdio, Command: "same"}
	store := NewTestStore(&Config{MCP: MCPs{name: value}})
	initial := store.SnapshotMCPAdmission(name).MCPRevision
	_, ok := store.RemoveMCP(name)
	require.True(t, ok)
	require.True(t, store.AddMCP(name, value))
	readded := store.SnapshotMCPAdmission(name).MCPRevision
	require.Greater(t, readded, initial)
	_, ok = store.RemoveMCPIfCurrent(name, value, initial)
	require.False(t, ok)
}

func TestConfigStoreMCPMutatorsConcurrentPublication(t *testing.T) {
	store := NewTestStore(&Config{MCP: make(MCPs)})
	const workers = 8
	const iterations = 40
	var wg sync.WaitGroup
	var readers sync.WaitGroup
	var stop atomic.Bool
	var failed atomic.Bool
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				snapshot := store.Config()
				for _, mcpConfig := range snapshot.MCP {
					_ = mcpConfig.Args
					for range mcpConfig.Env {
					}
				}
			}
		}()
	}
	for worker := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iteration := range iterations {
				name := fmt.Sprintf("server-%d-%d", worker, iteration)
				if !store.AddMCP(name, MCPConfig{Type: MCPStdio}) {
					failed.Store(true)
				}
				_, ok := store.UpdateMCP(name, func(m *MCPConfig) { m.Command = name })
				if !ok {
					failed.Store(true)
				}
			}
		}(worker)
	}
	wg.Wait()
	stop.Store(true)
	readers.Wait()
	require.False(t, failed.Load())
	require.Len(t, store.Config().MCP, workers*iterations)
	require.Equal(t, uint64(workers*iterations*2), store.Generation())
}
