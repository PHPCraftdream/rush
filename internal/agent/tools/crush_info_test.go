package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestRushInfo_MinimalConfig(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.NotContains(t, output, "[providers]")
	_ = output
	require.NotContains(t, output, "[mcp]")
	require.NotContains(t, output, "[permissions]")
	require.NotContains(t, output, "[tools]")
}

func TestRushInfo_ConfigFiles(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(
		&config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()},
		"/home/user/.config/rush/rush.json",
		"/project/.rush/rush.json",
	)
	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[config_files]")
	require.Contains(t, output, "/home/user/.config/rush/rush.json")
	require.Contains(t, output, "/project/.rush/rush.json")
}

func TestRushInfo_Models(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeSmart: {Model: "claude-sonnet-4-20250514", Provider: "anthropic"},
			config.SelectedModelTypeFast:  {Model: "claude-haiku-3-20250307", Provider: "anthropic"},
		},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[model]")
	require.Contains(t, output, "smart = claude-sonnet-4-20250514 (anthropic)")
	require.Contains(t, output, "fast = claude-haiku-3-20250307 (anthropic)")
}

func TestRushInfo_Models_WorkerAndReviewer(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeSmart:    {Model: "claude-sonnet-4-20250514", Provider: "anthropic"},
			config.SelectedModelTypeFast:     {Model: "claude-haiku-3-20250307", Provider: "anthropic"},
			config.SelectedModelTypeWorker:   {Model: "claude-haiku-3-20250307", Provider: "anthropic"},
			config.SelectedModelTypeReviewer: {Model: "claude-opus-4-20250514", Provider: "anthropic"},
		},
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[model]")
	require.Contains(t, output, "smart = claude-sonnet-4-20250514 (anthropic)")
	require.Contains(t, output, "fast = claude-haiku-3-20250307 (anthropic)")
	require.Contains(t, output, "worker = claude-haiku-3-20250307 (anthropic)")
	require.Contains(t, output, "reviewer = claude-opus-4-20250514 (anthropic)")
}

func TestRushInfo_Providers(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{Models: make([]catwalk.Model, 8)})
	providers.Set("anthropic", config.ProviderConfig{Models: make([]catwalk.Model, 12)})

	cfg := config.NewTestStore(&config.Config{Providers: providers})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[providers]")
	anthropicIdx := strings.Index(output, "anthropic = enabled")
	openaiIdx := strings.Index(output, "openai = enabled")
	require.Greater(t, anthropicIdx, -1)
	require.Greater(t, openaiIdx, -1)
	require.Less(t, anthropicIdx, openaiIdx, "anthropic should appear before openai")
	require.Contains(t, output, "anthropic = enabled (12 models)")
	require.Contains(t, output, "openai = enabled (8 models)")
}

func TestRushInfo_DisabledProvidersOmitted(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{Disable: true, Models: make([]catwalk.Model, 8)})
	providers.Set("anthropic", config.ProviderConfig{Models: make([]catwalk.Model, 12)})

	cfg := config.NewTestStore(&config.Config{Providers: providers})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "anthropic = enabled")
	require.NotContains(t, output, "openai")
}

func TestRushInfo_MCPStates(t *testing.T) {
	t.Parallel()

	connectedAt := time.Date(2025, 1, 15, 15, 4, 5, 0, time.UTC)
	states := map[string]mcp.ClientInfo{
		"github": {
			Name:        "github",
			State:       mcp.StateConnected,
			Counts:      mcp.Counts{Tools: 42, Resources: 7},
			ConnectedAt: connectedAt,
		},
		"filesystem": {
			Name:  "filesystem",
			State: mcp.StateError,
			Error: errors.New("connection refused"),
		},
	}

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})

	var b strings.Builder
	writeMCP(&b, states, cfg)
	output := b.String()
	require.Contains(t, output, "[mcp]")
	require.Contains(t, output, "filesystem = error: connection refused")
	require.Contains(t, output, "github = connected (42 tools, 7 resources) since 15:04:05")
	filesystemIdx := strings.Index(output, "filesystem")
	githubIdx := strings.Index(output, "github")
	require.Less(t, filesystemIdx, githubIdx, "filesystem should appear before github")
}

func TestRushInfo_YoloMode(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{},
	})
	cfg.SetSkipPermissionRequests(true)

	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[permissions]")
	require.Contains(t, output, "mode = yolo")
}

func TestRushInfo_AllowedTools(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{AllowedTools: []string{"edit:write", "bash"}},
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[permissions]")
	require.Contains(t, output, "allowed_tools = bash, edit:write")
}

func TestRushInfo_DisabledTools(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisabledTools: []string{"sourcegraph", "agentic_fetch"}},
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[tools]")
	require.Contains(t, output, "disabled = agentic_fetch, sourcegraph")
}

func TestRushInfo_Options(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options: &config.Options{
			DataDirectory:        "/Users/user/project/.rush",
			Debug:                true,
			DisableAutoSummarize: true,
		},
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[options]")
	require.Contains(t, output, "auto_summarize = false")
	require.Contains(t, output, "data_directory = /Users/user/project/.rush")
	require.Contains(t, output, "debug = true")
}

func TestRushInfo_AutoSummarizeInversion(t *testing.T) {
	t.Parallel()

	cfgFalse := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisableAutoSummarize: true},
	})
	outputFalse := buildRushInfo(cfgFalse, nil, nil, nil)
	require.Contains(t, outputFalse, "auto_summarize = false")

	cfgTrue := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisableAutoSummarize: false},
	})
	outputTrue := buildRushInfo(cfgTrue, nil, nil, nil)
	require.Contains(t, outputTrue, "auto_summarize = true")
}

func TestRushInfo_NoSecrets(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{
		APIKey: "sk-super-secret-key-12345",
		Models: make([]catwalk.Model, 8),
	})

	cfg := config.NewTestStore(&config.Config{Providers: providers})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.NotContains(t, output, "sk-super-secret-key-12345")
	require.NotContains(t, output, "secret")
	require.Contains(t, output, "openai = enabled (8 models)")
}

func TestRushInfo_DeterministicOrdering(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("zebra", config.ProviderConfig{Models: make([]catwalk.Model, 1)})
	providers.Set("alpha", config.ProviderConfig{Models: make([]catwalk.Model, 2)})
	providers.Set("middle", config.ProviderConfig{Models: make([]catwalk.Model, 3)})

	states := map[string]mcp.ClientInfo{
		"z-mcp": {Name: "z-mcp", State: mcp.StateConnected, Counts: mcp.Counts{Tools: 1}},
		"a-mcp": {Name: "a-mcp", State: mcp.StateConnected, Counts: mcp.Counts{Tools: 2}},
	}

	cfg := config.NewTestStore(&config.Config{
		Providers: providers,
		Options:   &config.Options{DisabledTools: []string{"z-tool", "a-tool"}},
		Permissions: &config.Permissions{
			AllowedTools: []string{"z-perm", "a-perm"},
		},
	})
	cfg.SetSkipPermissionRequests(true)

	// Test MCP ordering via writeMCP directly.
	var mcpBuf strings.Builder
	writeMCP(&mcpBuf, states, cfg)
	mcpOutput := mcpBuf.String()
	aMcpIdx := strings.Index(mcpOutput, "a-mcp = connected")
	zMcpIdx := strings.Index(mcpOutput, "z-mcp = connected")
	require.Less(t, aMcpIdx, zMcpIdx)

	output := buildRushInfo(cfg, nil, nil, nil)

	alphaIdx := strings.Index(output, "alpha = enabled")
	middleIdx := strings.Index(output, "middle = enabled")
	zebraIdx := strings.Index(output, "zebra = enabled")
	require.Less(t, alphaIdx, middleIdx)
	require.Less(t, middleIdx, zebraIdx)

	require.Contains(t, output, "disabled = a-tool, z-tool")
	require.Contains(t, output, "allowed_tools = a-perm, z-perm")
}

func TestRushInfo_EmptySectionsOmitted(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers:   csync.NewMap[string, config.ProviderConfig](),
		Permissions: &config.Permissions{},
		Options:     &config.Options{},
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.NotContains(t, output, "[tools]")
	require.NotContains(t, output, "[permissions]")
	_ = output
	require.NotContains(t, output, "[mcp]")
	require.NotContains(t, output, "[skills]")
}

func TestRushInfo_ConfigStaleness_Clean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configPath)

	// Capture snapshot (normally done in Load)
	store.CaptureStalenessSnapshot([]string{configPath})

	output := buildRushInfo(store, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = false")
	require.NotContains(t, output, "changed_paths")
	require.NotContains(t, output, "missing_paths")
}

func TestRushInfo_ConfigStaleness_Dirty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configPath)

	// Capture initial snapshot
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file to trigger dirty state
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	output := buildRushInfo(store, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = true")
	require.Contains(t, output, "changed_paths")
	require.Contains(t, output, configPath)
}

func TestRushInfo_ConfigStaleness_MissingPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	}, configPath)

	// Capture initial snapshot
	store.CaptureStalenessSnapshot([]string{configPath})

	// Delete file to trigger missing state
	require.NoError(t, os.Remove(configPath))

	output := buildRushInfo(store, nil, nil, nil)
	require.Contains(t, output, "[config]")
	require.Contains(t, output, "dirty = true")
	require.Contains(t, output, "missing_paths")
	require.Contains(t, output, configPath)
}

func TestRushInfo_Skills_NoSkills(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, nil, nil, nil)
	require.NotContains(t, output, "[skills]")
}

func TestRushInfo_Skills_MixedLoadedUnloaded(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "go-doc", Builtin: false},
		{Name: "bash", Builtin: false},
		{Name: "crush-config", Builtin: true},
	}
	activeSkills := allSkills

	tracker := skills.NewTracker(activeSkills)
	tracker.MarkLoaded("bash")
	tracker.MarkLoaded("crush-config")

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, allSkills, activeSkills, tracker)
	require.Contains(t, output, "[skills]")
	require.Contains(t, output, "bash = user, loaded")
	require.Contains(t, output, "crush-config = builtin, loaded")
	require.Contains(t, output, "go-doc = user, unloaded")
}

func TestRushInfo_Skills_DisabledSkills(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "bash", Builtin: false},
		{Name: "crush-config", Builtin: true},
		{Name: "image-convert", Builtin: false},
	}
	activeSkills := []*skills.Skill{
		{Name: "bash", Builtin: false},
		{Name: "crush-config", Builtin: true},
	}

	tracker := skills.NewTracker(activeSkills)

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{DisabledSkills: []string{"image-convert"}},
	})
	output := buildRushInfo(cfg, allSkills, activeSkills, tracker)
	require.Contains(t, output, "[skills]")
	require.Contains(t, output, "bash = user, unloaded")
	require.Contains(t, output, "crush-config = builtin, unloaded")
	require.Contains(t, output, "image-convert = user, disabled")
}

func TestRushInfo_Skills_Ordering(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "z-skill", Builtin: false},
		{Name: "a-skill", Builtin: true},
		{Name: "m-skill", Builtin: false},
	}
	activeSkills := allSkills
	tracker := skills.NewTracker(activeSkills)

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, allSkills, activeSkills, tracker)

	aIdx := strings.Index(output, "a-skill")
	mIdx := strings.Index(output, "m-skill")
	zIdx := strings.Index(output, "z-skill")
	require.Less(t, aIdx, mIdx)
	require.Less(t, mIdx, zIdx)
}

func TestRushInfo_Skills_BuiltinOrigin(t *testing.T) {
	t.Parallel()

	allSkills := []*skills.Skill{
		{Name: "crush-config", Builtin: true},
		{Name: "my-skill", Builtin: false},
	}
	activeSkills := allSkills
	tracker := skills.NewTracker(activeSkills)

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	output := buildRushInfo(cfg, allSkills, activeSkills, tracker)
	require.Contains(t, output, "crush-config = builtin, unloaded")
	require.Contains(t, output, "my-skill = user, unloaded")
}

func TestRushInfo_Hooks(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Hooks: map[string][]config.HookConfig{
			"PreToolUse": {
				{Command: "check-privates.sh", Matcher: "edit|write"},
				{Command: "audit.sh"},
			},
		},
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.Contains(t, output, "[hooks]")
	require.Contains(t, output, "PreToolUse (matcher: edit|write) = check-privates.sh")
	require.Contains(t, output, "PreToolUse = audit.sh")
}

func TestRushInfo_Hooks_NoHooks(t *testing.T) {
	t.Parallel()

	cfg := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})

	output := buildRushInfo(cfg, nil, nil, nil)
	require.NotContains(t, output, "[hooks]")
}
