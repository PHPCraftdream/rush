// Config file discovery, parsing and merging tests: loadFromBytes
// merge order and selected-model parsing, lookupConfigs project/git
// boundaries, skills-dir discovery, and loadFromConfigPaths errors.

package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestConfig_LoadFromBytes(t *testing.T) {
	data1 := []byte(`{"providers": {"openai": {"api_key": "key1", "base_url": "https://api.openai.com/v1"}}}`)
	data2 := []byte(`{"providers": {"openai": {"api_key": "key2", "base_url": "https://api.openai.com/v2"}}}`)
	data3 := []byte(`{"providers": {"openai": {}}}`)

	loadedConfig, err := loadFromBytes([][]byte{data1, data2, data3})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig)
	require.Equal(t, 1, loadedConfig.Providers.Len())
	pc, _ := loadedConfig.Providers.Get("openai")
	require.Equal(t, "key2", pc.APIKey)
	require.Equal(t, "https://api.openai.com/v2", pc.BaseURL)
}

func TestConfig_LoadFromBytes_WorkerAndReviewerModels(t *testing.T) {
	data := []byte(`{
		"models": {
			"smart": {"model": "gpt-4o", "provider": "openai"},
			"fast": {"model": "gpt-4o-mini", "provider": "openai"},
			"worker": {"model": "gpt-4o-mini", "provider": "openai"},
			"reviewer": {"model": "o1", "provider": "openai"}
		}
	}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig)
	require.Len(t, loadedConfig.Models, 4)

	smart, ok := loadedConfig.Models[SelectedModelTypeSmart]
	require.True(t, ok)
	require.Equal(t, "gpt-4o", smart.Model)

	fast, ok := loadedConfig.Models[SelectedModelTypeFast]
	require.True(t, ok)
	require.Equal(t, "gpt-4o-mini", fast.Model)

	worker, ok := loadedConfig.Models[SelectedModelTypeWorker]
	require.True(t, ok)
	require.Equal(t, "gpt-4o-mini", worker.Model)
	require.Equal(t, "openai", worker.Provider)

	reviewer, ok := loadedConfig.Models[SelectedModelTypeReviewer]
	require.True(t, ok)
	require.Equal(t, "o1", reviewer.Model)
	require.Equal(t, "openai", reviewer.Provider)

	// Round-trip: marshal back to JSON and confirm the new keys survive.
	marshaled, err := json.Marshal(loadedConfig.Models)
	require.NoError(t, err)
	require.Contains(t, string(marshaled), `"worker":`)
	require.Contains(t, string(marshaled), `"reviewer":`)
}

func TestLookupConfigs_BoundedByProject(t *testing.T) {
	// Force GlobalConfig and GlobalConfigData to point at locations we
	// control so they can be present in the result without polluting
	// the developer's real config.
	isolateAllGlobalConfigPaths(t)

	t.Run("does not pick up rush.json above non-git project", func(t *testing.T) {
		parent := t.TempDir()

		// rush.json above the project must not be adopted.
		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "rush.json"),
			[]byte(`{}`),
			0o644,
		))

		project := filepath.Join(parent, "project")
		require.NoError(t, os.Mkdir(project, 0o755))

		got := lookupConfigs(project)
		for _, p := range got {
			require.NotEqual(t, filepath.Join(parent, "rush.json"), p)
		}
	})

	t.Run("does not climb out of git worktree to find rush.json", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "rush.json"),
			[]byte(`{}`),
			0o644,
		))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))
		gitInit := platform.Command(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		got := lookupConfigs(worktree)
		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, "rush.json"))
		require.NoError(t, err)
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			require.NotEqual(t, strayEval, pEval, "must not adopt parent rush.json")
		}
	})

	t.Run("picks up rush.json inside the project", func(t *testing.T) {
		project := t.TempDir()
		local := filepath.Join(project, "rush.json")
		require.NoError(t, os.WriteFile(local, []byte(`{}`), 0o644))

		got := lookupConfigs(project)

		localEval, err := filepath.EvalSymlinks(local)
		require.NoError(t, err)
		var foundLocal bool
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			if pEval == localEval {
				foundLocal = true
				break
			}
		}
		require.True(t, foundLocal, "expected project rush.json to be in lookup result: %v", got)
	})

	t.Run("global config is always included regardless of boundary", func(t *testing.T) {
		project := t.TempDir()

		got := lookupConfigs(project)
		// Global config and global data path are always prepended,
		// even when no project file exists.
		require.Contains(t, got, GlobalConfig())
		require.Contains(t, got, GlobalConfigData())
	})

	t.Run("system config is loaded first", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("system config not supported on Windows")
		}

		got := lookupConfigs(t.TempDir())
		require.NotEmpty(t, got)
		// The system-wide config must be first so it has the lowest
		// priority when configs are merged.
		require.Equal(t, "/etc/rush/rush.json", got[0])
	})
}

func TestLookupConfigCandidatesCanonicalizesSymlinkedNestedRepository(t *testing.T) {
	outer := t.TempDir()
	nested := filepath.Join(outer, "nested")
	subdir := filepath.Join(nested, "packages", "app")
	link := filepath.Join(outer, "linked-nested")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	if err := os.Symlink(nested, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(outer, "rush.json"), []byte(`{"options":{"debug":true}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "rush.json"), []byte(`{"options":{"debug":false}}`), 0o600))

	canonicalSubdir := canonicalConfigPath(subdir)
	canonicalNested := canonicalConfigPath(nested)
	lexicalSubdir := filepath.Join(link, "packages", "app")
	worktreeRootCache.Store(canonicalSubdir, canonicalNested)
	worktreeRootCache.Store(filepath.Clean(lexicalSubdir), canonicalNested)
	t.Cleanup(func() {
		worktreeRootCache.Delete(canonicalSubdir)
		worktreeRootCache.Delete(filepath.Clean(lexicalSubdir))
	})

	paths := lookupConfigCandidates(lexicalSubdir)
	require.Contains(t, paths, filepath.Join(canonicalNested, "rush.json"))
	require.NotContains(t, paths, filepath.Join(outer, "rush.json"))
}

func TestProjectConfigsPreservesProjectCandidatesAndNegativeStaleness(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	root := normalizeReloadPath(t.TempDir())
	rushPath := filepath.Join(root, "rush.json")
	dotRushPath := filepath.Join(root, ".rush.json")
	require.NoError(t, os.WriteFile(rushPath, []byte(`{"options":{"debug":true}}`), 0o600))
	require.NoError(t, os.WriteFile(dotRushPath, []byte(`{"options":{"debug":false}}`), 0o600))

	configs := ProjectConfigs(root)
	require.Contains(t, configs, rushPath)
	require.Contains(t, configs, dotRushPath)

	missing := filepath.Join(root, "later.json")
	store := newTestConfigStore(testStoreOpts{config: &Config{}})
	store.captureStalenessSnapshot([]string{missing})
	require.NoError(t, os.WriteFile(missing, []byte(`{}`), 0o600))
	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, missing)
}

func TestReadStableConfigDocumentsDeduplicatesCanonicalFileIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	aliasDir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, aliasDir); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{"debug":true}}`), 0o600))

	documents, err := readStableConfigDocuments([]string{path, filepath.Join(aliasDir, "rush.json")})
	require.NoError(t, err)
	require.Len(t, documents, 1, "one physical rush.json must produce one document")
	_, loaded, fingerprints := configDocumentBytes(documents)
	require.Len(t, loaded, 1)
	require.Len(t, fingerprints, 1)
	require.True(t, pathAlreadyLoaded(loaded, filepath.Join(aliasDir, "rush.json")))
}

func TestProjectSkillsDir_MonorepoGitRoot(t *testing.T) {
	t.Parallel()

	t.Run("includes git worktree root skills dirs after working-dir dirs", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		root := t.TempDir()
		gitInit := platform.Command(t.Context(), "git", "init", "-q")
		gitInit.Dir = root
		require.NoError(t, gitInit.Run())

		// Repo-root-level skills (monorepo-wide).
		require.NoError(t, os.MkdirAll(filepath.Join(root, ".agents", "skills"), 0o755))

		// Subdirectory the user is actually working in, with its own
		// local skills dir.
		subDir := filepath.Join(root, "packages", "app")
		require.NoError(t, os.MkdirAll(filepath.Join(subDir, ".agents", "skills"), 0o755))

		got := ProjectSkillsDir(subDir)

		rootEval, err := filepath.EvalSymlinks(root)
		require.NoError(t, err)
		subEval, err := filepath.EvalSymlinks(subDir)
		require.NoError(t, err)

		wantWorkingDir := filepath.Join(subEval, ".agents", "skills")
		wantGitRoot := filepath.Join(rootEval, ".agents", "skills")

		idxWorking, idxRoot := -1, -1
		for i, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				// Non-.agents/skills entries may not exist on disk; compare
				// the literal path instead.
				pEval = p
			}
			if pEval == wantWorkingDir && idxWorking == -1 {
				idxWorking = i
			}
			if pEval == wantGitRoot && idxRoot == -1 {
				idxRoot = i
			}
		}

		require.NotEqual(t, -1, idxWorking, "expected working-dir skills path in result: %v", got)
		require.NotEqual(t, -1, idxRoot, "expected git-root skills path in result: %v", got)
		require.Less(t, idxWorking, idxRoot, "working-dir paths must come before git-root paths (local precedence): %v", got)
	})

	t.Run("does not duplicate paths when working dir is the git root", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		root := t.TempDir()
		gitInit := platform.Command(t.Context(), "git", "init", "-q")
		gitInit.Dir = root
		require.NoError(t, gitInit.Run())

		got := ProjectSkillsDir(root)
		require.Len(t, got, len(projectSkillSubdirs), "must not append git-root dirs a second time when workingDir already is the root")
	})

	t.Run("falls back to working-dir-only paths outside a git repo", func(t *testing.T) {
		t.Parallel()
		nonGit := t.TempDir()

		got := ProjectSkillsDir(nonGit)
		require.Len(t, got, len(projectSkillSubdirs))
	})
}

func TestLoadFromConfigPaths_InvalidJSON(t *testing.T) {
	t.Parallel()

	t.Run("identifies the offending file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		good := filepath.Join(tmpDir, "good.json")
		bad := filepath.Join(tmpDir, "bad.json")
		require.NoError(t, os.WriteFile(good, []byte(`{"providers":{}}`), 0o644))
		require.NoError(t, os.WriteFile(bad, []byte(`{not valid json}`), 0o644))

		_, _, err := loadFromConfigPaths([]string{good, bad})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid JSON in config file")
		require.Contains(t, err.Error(), "bad.json")
	})

	t.Run("skips missing and empty files", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		empty := filepath.Join(tmpDir, "empty.json")
		require.NoError(t, os.WriteFile(empty, []byte(""), 0o644))

		cfg, _, err := loadFromConfigPaths([]string{
			filepath.Join(tmpDir, "nonexistent.json"),
			empty,
		})
		require.NoError(t, err)
		require.NotNil(t, cfg)
	})
}

// TestGlobalSkillsDirs_KeepOtherToolDirsForCommands is the revert oracle for
// task #1156: command-facing discovery must keep other tools' directories
// (~/.claude/skills) so skills installed by other agent tools remain
// loadable as slash commands; only the agent system prompt filters them out.
// Removing that directory from globalSkillsDirs turns this test red.
func TestGlobalSkillsDirs_KeepOtherToolDirsForCommands(t *testing.T) {
	// NOTE: deliberately NOT t.Parallel() — t.Setenv panics when combined
	// with t.Parallel.
	t.Setenv("RUSH_SKILLS_DIR", "")

	got := GlobalSkillsDirs()
	require.NotEmpty(t, got)

	var found bool
	for _, dir := range got {
		if strings.HasSuffix(dir, filepath.Join(".claude", "skills")) {
			found = true
			break
		}
	}
	require.True(t, found,
		"command discovery must keep ~/.claude/skills: %v", got)

	homeDir := t.TempDir()
	withHome := globalSkillsDirs(homeDir)
	require.Contains(t, withHome, filepath.Join(homeDir, ".claude", "skills"),
		"home-scoped global discovery must keep ~/.claude/skills: %v", withHome)
	require.Contains(t, withHome, filepath.Join(homeDir, ".agents", "skills"),
		"home-scoped global discovery must keep ~/.agents/skills: %v", withHome)
}

// TestGlobalPromptSkillsDirs_ExcludeOtherToolDirs locks the prompt-facing
// half of task #1156: the global list advertised in the agent system prompt
// drops other tools' directories (~/.claude/skills, .cursor/skills), while
// the command-facing list keeps them.
func TestGlobalPromptSkillsDirs_ExcludeOtherToolDirs(t *testing.T) {
	// NOTE: deliberately NOT t.Parallel() — t.Setenv panics when combined
	// with t.Parallel.
	t.Setenv("RUSH_SKILLS_DIR", "")

	full := GlobalSkillsDirs()
	prompt := GlobalPromptSkillsDirs()

	require.NotEmpty(t, full)
	require.NotEmpty(t, prompt)
	require.Less(t, len(prompt), len(full),
		"prompt list must be a strict subset of the command list: %v vs %v", full, prompt)

	for _, dir := range prompt {
		require.NotContains(t, dir, ".claude",
			"prompt discovery must not advertise ~/.claude/skills: %v", prompt)
		require.NotContains(t, dir, ".cursor",
			"prompt discovery must not advertise .cursor/skills: %v", prompt)
		if strings.HasSuffix(dir, "skills") {
			require.Contains(t, full, dir,
				"prompt dirs must still come from the command-facing list")
		}
	}
}

// TestProjectPromptSkillsDirs_ExcludeOtherToolDirs locks the project-side
// split: ProjectSkillsDir stays the command-facing full list (4 subdirs,
// including other tools' dirs), while ProjectPromptSkillsDirs — what the
// system prompt advertises — keeps only Rush's own and the Agent Skills
// spec directories.
func TestProjectPromptSkillsDirs_ExcludeOtherToolDirs(t *testing.T) {
	t.Parallel()

	nonGit := t.TempDir()

	full := ProjectSkillsDir(nonGit)
	require.Len(t, full, 4)
	require.Contains(t, full, filepath.Join(nonGit, ".claude", "skills"))
	require.Contains(t, full, filepath.Join(nonGit, ".cursor", "skills"))

	prompt := ProjectPromptSkillsDirs(nonGit)
	require.Len(t, prompt, 2)
	require.Contains(t, prompt, filepath.Join(nonGit, ".agents", "skills"))
	require.Contains(t, prompt, filepath.Join(nonGit, ".rush", "skills"))

	for _, dir := range prompt {
		require.NotContains(t, dir, ".claude",
			"prompt discovery must not advertise .claude/skills: %v", prompt)
		require.NotContains(t, dir, ".cursor",
			"prompt discovery must not advertise .cursor/skills: %v", prompt)
	}
}

// TestConfig_ExplicitSkillsPathsClaudeDirKept proves #1156 only removed the
// *default* scanning of other tools' directories: an explicitly configured
// skills_paths entry is honored verbatim, even when it points at a
// .claude-flavored directory. That is the documented opt-back-in path.
func TestConfig_ExplicitSkillsPathsClaudeDirKept(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, ".claude-skills")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(claudeDir, "SKILL.md"),
		[]byte("---\nname: optin\ndescription: Explicitly opted-in skill.\n---\nBody.\n"),
		0o644,
	))

	path := filepath.Join(tmp, "rush.json")
	data, err := json.Marshal(map[string]any{
		"options": map[string]any{
			"skills_paths": []string{filepath.ToSlash(claudeDir)},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	cfg, _, err := loadFromConfigPaths([]string{path})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Options)

	var found bool
	for _, p := range cfg.Options.SkillsPaths {
		if strings.Contains(p, ".claude") {
			found = true
			break
		}
	}
	require.True(t, found,
		"explicit skills_paths entries must be preserved verbatim, got: %v",
		cfg.Options.SkillsPaths)
}
