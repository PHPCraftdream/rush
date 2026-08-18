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
	"testing"

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
			"large": {"model": "gpt-4o", "provider": "openai"},
			"small": {"model": "gpt-4o-mini", "provider": "openai"},
			"worker": {"model": "gpt-4o-mini", "provider": "openai"},
			"reviewer": {"model": "o1", "provider": "openai"}
		}
	}`)

	loadedConfig, err := loadFromBytes([][]byte{data})

	require.NoError(t, err)
	require.NotNil(t, loadedConfig)
	require.Len(t, loadedConfig.Models, 4)

	large, ok := loadedConfig.Models[SelectedModelTypeLarge]
	require.True(t, ok)
	require.Equal(t, "gpt-4o", large.Model)

	small, ok := loadedConfig.Models[SelectedModelTypeSmall]
	require.True(t, ok)
	require.Equal(t, "gpt-4o-mini", small.Model)

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

	t.Run("does not pick up crush.json above non-git project", func(t *testing.T) {
		parent := t.TempDir()

		// crush.json above the project must not be adopted.
		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "crush.json"),
			[]byte(`{}`),
			0o644,
		))

		project := filepath.Join(parent, "project")
		require.NoError(t, os.Mkdir(project, 0o755))

		got := lookupConfigs(project)
		for _, p := range got {
			require.NotEqual(t, filepath.Join(parent, "crush.json"), p)
		}
	})

	t.Run("does not climb out of git worktree to find crush.json", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		parent := t.TempDir()

		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "crush.json"),
			[]byte(`{}`),
			0o644,
		))

		worktree := filepath.Join(parent, "worktree")
		require.NoError(t, os.Mkdir(worktree, 0o755))
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
		gitInit.Dir = worktree
		require.NoError(t, gitInit.Run())

		got := lookupConfigs(worktree)
		strayEval, err := filepath.EvalSymlinks(filepath.Join(parent, "crush.json"))
		require.NoError(t, err)
		for _, p := range got {
			pEval, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			require.NotEqual(t, strayEval, pEval, "must not adopt parent crush.json")
		}
	})

	t.Run("picks up crush.json inside the project", func(t *testing.T) {
		project := t.TempDir()
		local := filepath.Join(project, "crush.json")
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
		require.True(t, foundLocal, "expected project crush.json to be in lookup result: %v", got)
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
		require.Equal(t, "/etc/crush/crush.json", got[0])
	})
}

func TestProjectSkillsDir_MonorepoGitRoot(t *testing.T) {
	t.Parallel()

	t.Run("includes git worktree root skills dirs after working-dir dirs", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}

		root := t.TempDir()
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
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
		gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
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
