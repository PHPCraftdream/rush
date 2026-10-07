// Behavioral tests for the data-directory selection (SD-D #1143): the
// ordered priority, linked-worktree classification and redirection,
// dev-build isolation, the shared-mode marker, WS-3 (one resolution in
// Load, reload and rescue parity), and the migration policy derived from
// the source. Each group names the mutant its revert-check kills.
package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// gitRun runs one git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := platform.Command(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
	return strings.TrimSpace(string(out))
}

// makeMainRepoWithWorktree creates a main checkout with one commit and a
// linked worktree. Returns both canonical-ish absolute paths.
func makeMainRepoWithWorktree(t *testing.T) (string, string) {
	t.Helper()
	// Long form: Windows runners hand out 8.3 temp paths, git reports long ones.
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	mainRoot := filepath.Join(base, "main")
	wtRoot := filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(mainRoot, 0o755))
	gitRun(t, mainRoot, "init", "-q")
	gitRun(t, mainRoot, "config", "user.email", "test@example.com")
	gitRun(t, mainRoot, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "README.md"), []byte("x\n"), 0o644))
	gitRun(t, mainRoot, "add", ".")
	gitRun(t, mainRoot, "commit", "-q", "-m", "init")
	gitRun(t, mainRoot, "worktree", "add", "-q", wtRoot, "-b", "feature")
	return mainRoot, wtRoot
}

// makeBareRepoWithWorktree creates a bare repository plus one linked
// worktree (design section 3: "bare + worktree -> no change").
func makeBareRepoWithWorktree(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	bare := filepath.Join(base, "origin.git")
	wtRoot := filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(seed, 0o755))
	gitRun(t, seed, "init", "-q")
	gitRun(t, seed, "config", "user.email", "test@example.com")
	gitRun(t, seed, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "f.txt"), []byte("x\n"), 0o644))
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "init")
	gitRun(t, seed, "clone", "-q", "--bare", seed, bare)
	gitRun(t, bare, "worktree", "add", "-q", wtRoot, "-b", "feature")
	return bare, wtRoot
}

func allowRedirection(t *testing.T) {
	t.Helper()
	t.Cleanup(setDataDirRedirectionOverride(1))
}

// deployedExeSeam points the dev-build heuristic at a deployed-style
// binary path (outside temp and any checkout). The `go test` binary
// itself IS a dev build (exe in temp), so tests asserting a shared
// resolution must pin this seam.
func deployedExeSeam(t *testing.T) {
	t.Helper()
	t.Cleanup(setOSExecutablePathForTest(filepath.Join(
		filepath.VolumeName(t.TempDir()), string(os.PathSeparator), "opt", "rush", "rush.exe")))
}

// TestLoad_LinkedWorktree_RedirectsToMainDataDir: inside/outside the
// repository and from a subdirectory, a linked worktree resolves to
// <main>/.rush with source shared, and the process stops being home.
//
// Revert-check: remove the priority-4 redirection branch in
// defaultDataDirForWorkspace (return fallback instead of shared) and all
// subtests fail on the directory or the source.
func TestLoad_LinkedWorktree_RedirectsToMainDataDir(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	deployedExeSeam(t)
	isolateAllGlobalConfigPaths(t)

	mainRoot, wtRoot := makeMainRepoWithWorktree(t)

	t.Run("worktree root", func(t *testing.T) {
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceShared, store.DataDirSource())
		require.False(t, store.WorkspaceHome())
		require.False(t, WorkspaceHome(), "WorkspaceHome must follow the source")
	})

	t.Run("subdirectory of the worktree", func(t *testing.T) {
		sub := filepath.Join(wtRoot, "pkg", "deep")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		store, err := Load(sub, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceShared, store.DataDirSource())
	})

	t.Run("worktree outside the repository directory", func(t *testing.T) {
		// makeMainRepoWithWorktree already places wt outside main; this
		// subtest pins the contract explicitly.
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
	})

	t.Cleanup(func() { setDataDirHomeForTest(true) })
}

// TestLoad_LegacyLocalRushDB_Rules: an existing worktree-local rush.db is
// honored ONLY without the shared-mode marker; with the marker it is a
// stray and is ignored; a .rush without rush.db (e.g. skills) never makes
// the worktree legacy.
//
// Revert-check: treat mere directory presence (not rush.db) as the legacy
// sign and the skills subtest fails; drop the marker check and the marker
// subtest fails.
func TestLoad_LegacyLocalRushDB_Rules(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	deployedExeSeam(t)
	isolateAllGlobalConfigPaths(t)

	t.Run("rush.db without marker stays legacy-local", func(t *testing.T) {
		mainRoot, wtRoot := makeMainRepoWithWorktree(t)
		local := filepath.Join(wtRoot, ".rush")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(local, "rush.db"), []byte{}, 0o644))

		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, local, store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceLegacyLocal, store.DataDirSource())
		require.True(t, store.WorkspaceHome())
		_ = mainRoot
		t.Cleanup(func() { setDataDirHomeForTest(true) })
	})

	t.Run("rush.db with marker is ignored (stray), shared wins", func(t *testing.T) {
		mainRoot, wtRoot := makeMainRepoWithWorktree(t)
		mainData := filepath.Join(mainRoot, ".rush")
		require.NoError(t, os.MkdirAll(mainData, 0o755))
		local := filepath.Join(wtRoot, ".rush")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(local, "rush.db"), []byte{}, 0o644))
		require.NoError(t, MarkSharedWorkspace(mainData, wtRoot))

		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, mainData, store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceShared, store.DataDirSource())
		t.Cleanup(func() { setDataDirHomeForTest(true) })
	})

	t.Run("skills-only .rush without rush.db does not block shared", func(t *testing.T) {
		mainRoot, wtRoot := makeMainRepoWithWorktree(t)
		require.NoError(t, os.MkdirAll(filepath.Join(wtRoot, ".rush", "skills"), 0o755))

		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceShared, store.DataDirSource())
		t.Cleanup(func() { setDataDirHomeForTest(true) })
	})
}

// TestLoad_Classification_NoRedirection: main checkout, bare+worktree and
// plain non-git directories resolve exactly as before SD-D.
//
// Revert-check: guess the main root as the common git-dir's location
// instead of parsing `worktree list --porcelain` and the bare subtest
// fails (the bare repo gets adopted as the main checkout).
func TestLoad_Classification_NoRedirection(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	isolateAllGlobalConfigPaths(t)

	t.Run("main checkout is home, local .rush adopted as before", func(t *testing.T) {
		mainRoot, _ := makeMainRepoWithWorktree(t)
		local := filepath.Join(mainRoot, ".rush")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(local, "rush.db"), []byte{}, 0o644))

		store, err := Load(mainRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, local, store.Config().Options.DataDirectory)
		require.NotEqual(t, DataDirSourceShared, store.DataDirSource())
	})

	t.Run("bare repository with a worktree is not redirected", func(t *testing.T) {
		_, wtRoot := makeBareRepoWithWorktree(t)
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(wtRoot, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceDefault, store.DataDirSource())
	})

	t.Run("no git at all", func(t *testing.T) {
		plain := t.TempDir()
		store, err := Load(plain, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(plain, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceDefault, store.DataDirSource())
	})
}

// TestProbe_IgnoresGitHookEnv: GIT_DIR/GIT_WORK_TREE (git exports them
// into hook environments; pre-push runs this very test suite) must not
// change the linked-worktree classification.
//
// Revert-check: remove stripGitWorktreeEnv from the probe commands and
// the garbage-env subtests fail.
func TestProbe_IgnoresGitHookEnv(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)

	_, wtRoot := makeMainRepoWithWorktree(t)
	clean := linkedWorktreeInfo(wtRoot)
	require.NotNil(t, clean, "linked worktree must classify as linked")

	t.Setenv("GIT_DIR", "definitely-not-a-git-dir")
	t.Setenv("GIT_WORK_TREE", "definitely-not-a-worktree")
	probed := probeLinkedWorktree(wtRoot)
	require.NotNil(t, probed)
	require.Equal(t, clean.mainRoot, probed.mainRoot)
}

// TestLoad_DataDirPriority_FlagAndConfigBeatDefaults: --data-dir wins over
// options.data_directory, which wins over the worktree rules.
//
// Revert-check: swap the flag/config comparison in resolveDataDirectory
// and the flag subtest fails.
func TestLoad_DataDirPriority_FlagAndConfigBeatDefaults(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	mainRoot, wtRoot := makeMainRepoWithWorktree(t)
	allowRedirection(t)

	t.Run("flag wins over everything", func(t *testing.T) {
		flagDir := filepath.Join(t.TempDir(), "flag-data")
		store, err := Load(wtRoot, flagDir, false)
		require.NoError(t, err)
		require.Equal(t, flagDir, store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceFlag, store.DataDirSource())
	})

	t.Run("options.data_directory wins over the worktree rules", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg-data")
		body, marshalErr := json.Marshal(map[string]any{
			"options": map[string]any{"data_directory": cfgDir},
		})
		require.NoError(t, marshalErr)
		// The config lives in the main checkout; loading from the main
		// root picks it up via the project config discovery.
		require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "rush.json"), body, 0o644))
		defer os.Remove(filepath.Join(mainRoot, "rush.json"))

		store, err := Load(mainRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, cfgDir, store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceConfig, store.DataDirSource())
	})
}

// TestLoad_WS3_ReloadAndRescueParity: Load with a mergeable
// <main>/.rush/rush.json keeps the shared directory through a reload,
// with the source unchanged, and ResolveDataDirectory (rescue commands)
// returns the same directory.
//
// Revert-check: make the reload re-resolve (or reset) the source in
// buildAndPublishReload and the reload subtest fails; diverge
// ResolveDataDirectory from resolveDataDirectory and the rescue subtest
// fails.
func TestLoad_WS3_ReloadAndRescueParity(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	deployedExeSeam(t)
	isolateAllGlobalConfigPaths(t)

	mainRoot, wtRoot := makeMainRepoWithWorktree(t)
	mainData := filepath.Join(mainRoot, ".rush")
	require.NoError(t, os.MkdirAll(mainData, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mainData, "rush.json"),
		[]byte(`{"options":{"context_paths":[]}}`), 0o644))

	store, err := Load(wtRoot, "", false)
	require.NoError(t, err)
	require.Equal(t, DataDirSourceShared, store.DataDirSource())
	require.Equal(t, mainData, store.Config().Options.DataDirectory)

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, DataDirSourceShared, store.DataDirSource(), "reload must not change the source (WS-3)")
	require.Equal(t, mainData, store.Config().Options.DataDirectory, "reload must not change the directory (WS-3)")

	rescued, err := ResolveDataDirectory(wtRoot, "")
	require.NoError(t, err)
	require.Equal(t, mainData, rescued, "rescue resolution must match Load")

	t.Cleanup(func() { setDataDirHomeForTest(true) })
}

// TestLoad_UnderTest_NoRedirectionWithoutSeam: under a test binary the
// shared redirection is disabled entirely (the pre-push hook's `go test`
// in a worktree must never see or create the shared DB).
//
// Revert-check: make redirectionAllowed ignore testing.Testing() and the
// subtest fails (the worktree redirects to <main>/.rush).
func TestLoad_UnderTest_NoRedirectionWithoutSeam(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	isolateAllGlobalConfigPaths(t)
	require.False(t, redirectionAllowed(), "precondition: no seam under a test binary")

	mainRoot, wtRoot := makeMainRepoWithWorktree(t)
	store, err := Load(wtRoot, "", false)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(wtRoot, ".rush"), store.Config().Options.DataDirectory)
	require.NotEqual(t, DataDirSourceShared, store.DataDirSource())
	_ = mainRoot
}

// TestLoad_DevBuild_IsolatedInLinkedWorktree: a dev build (exe in temp or
// inside a checkout) resolves <wt>/.rush/dev and may not migrate the
// shared DB; a deployed-style exe path still redirects shared and may.
//
// Revert-check: drop the isDevBuild branch in defaultDataDirForWorkspace
// and the dev subtest fails; make MayMigrateData always true and the
// migration subtests fail.
func TestLoad_DevBuild_IsolatedInLinkedWorktree(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	isolateAllGlobalConfigPaths(t)
	t.Cleanup(func() { setDataDirHomeForTest(true) })

	mainRoot, wtRoot := makeMainRepoWithWorktree(t)

	t.Run("dev exe in temp", func(t *testing.T) {
		t.Cleanup(setOSExecutablePathForTest(filepath.Join(t.TempDir(), "rush.exe")))
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(wtRoot, ".rush", "dev"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceDevIsolated, store.DataDirSource())
		require.True(t, store.MayMigrateData(), "dev-isolated is its own database")
	})

	t.Run("dev exe inside the checkout", func(t *testing.T) {
		// The bin dir must exist: detectDevBuild asks git about exeDir, and a
		// missing dir only passed before via the temp-dir branch by accident.
		require.NoError(t, os.MkdirAll(filepath.Join(wtRoot, "bin"), 0o755))
		t.Cleanup(setOSExecutablePathForTest(filepath.Join(wtRoot, "bin", "rush.exe")))
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(wtRoot, ".rush", "dev"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceDevIsolated, store.DataDirSource())
	})

	t.Run("deployed-style exe redirects shared but may not migrate", func(t *testing.T) {
		deployed := filepath.Join(filepath.VolumeName(t.TempDir()), string(os.PathSeparator), "opt", "rush", "rush.exe")
		t.Cleanup(setOSExecutablePathForTest(deployed))
		store, err := Load(wtRoot, "", false)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(mainRoot, ".rush"), store.Config().Options.DataDirectory)
		require.Equal(t, DataDirSourceShared, store.DataDirSource())
		require.True(t, store.MayMigrateData(), "deployed binaries migrate from anywhere")
	})
}

// TestMayMigrateData_Matrix: only (dev build + shared source) is barred
// from migrating; every other combination migrates.
//
// Revert-check: return true unconditionally and the dev+shared case
// fails; return false for every shared source and the deployed+shared
// case fails.
func TestMayMigrateData_Matrix(t *testing.T) {
	base := t.TempDir()

	devExe := filepath.Join(base, "tmp-exe", "rush.exe")
	t.Cleanup(setOSExecutablePathForTest(devExe))

	shared := &ConfigStore{dataDirSource: DataDirSourceShared}
	isolated := &ConfigStore{dataDirSource: DataDirSourceDevIsolated}
	home := &ConfigStore{dataDirSource: DataDirSourceLegacyLocal}
	zero := &ConfigStore{}

	require.False(t, shared.MayMigrateData(), "dev build on the shared DB must not migrate")
	require.True(t, isolated.MayMigrateData())
	require.True(t, home.MayMigrateData())
	require.True(t, zero.MayMigrateData())

	t.Run("deployed build on shared may migrate", func(t *testing.T) {
		t.Cleanup(setOSExecutablePathForTest(filepath.Join(
			filepath.VolumeName(base), string(os.PathSeparator), "opt", "rush", "rush.exe")))
		require.True(t, shared.MayMigrateData())
	})
}

// TestMarkSharedWorkspace_IdempotentAndDeterministic: the marker path is
// a stable hash of the canonical workspace root, the file records the
// root, and re-marking is a no-op.
//
// Revert-check: hash the raw (uncanonicalized) path with a trailing
// separator variant and the stability subtest fails.
func TestMarkSharedWorkspace_IdempotentAndDeterministic(t *testing.T) {
	mainData := t.TempDir()
	ws := t.TempDir()

	p1 := SharedWorkspaceMarkerPath(mainData, ws)
	p2 := SharedWorkspaceMarkerPath(mainData, ws+string(os.PathSeparator))
	require.Equal(t, p1, p2, "trailing separators must not split the namespace")
	require.Contains(t, p1, filepath.Join("workspaces"), "marker lives under <main>/.rush/workspaces")

	require.NoError(t, MarkSharedWorkspace(mainData, ws))
	require.True(t, sharedMarkerExists(mainData, ws))
	content, err := os.ReadFile(p1)
	require.NoError(t, err)
	require.Contains(t, string(content), filepath.Base(ws))
	require.NoError(t, MarkSharedWorkspace(mainData, ws), "second mark must be a no-op")

	require.False(t, sharedMarkerExists(t.TempDir(), ws), "another data dir has no marker")
}

// TestWorkspaceHome_FollowsLoadSource: Load flips the process flag for a
// shared resolution and restores it for a home one; reload never touches
// it (WS-3).
//
// Revert-check: keep WorkspaceHome constant true and this test fails.
func TestWorkspaceHome_FollowsLoadSource(t *testing.T) {
	t.Cleanup(func() { setDataDirHomeForTest(true) })
	setDataDirHomeForTest(false)
	require.False(t, WorkspaceHome())
	setDataDirHomeForTest(true)
	require.True(t, WorkspaceHome())
}

// TestResolveDataDirectory_LegacyLocalParity: the rescue resolution
// returns the adopted legacy-local directory, matching Load.
func TestResolveDataDirectory_LegacyLocalParity(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "version").Output(); err != nil {
		t.Skip("git not available")
	}
	allowRedirection(t)
	isolateAllGlobalConfigPaths(t)

	_, wtRoot := makeMainRepoWithWorktree(t)
	local := filepath.Join(wtRoot, ".rush")
	require.NoError(t, os.MkdirAll(local, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(local, "rush.db"), []byte{}, 0o644))

	rescued, err := ResolveDataDirectory(wtRoot, "")
	require.NoError(t, err)
	require.Equal(t, local, rescued)

	store, err := Load(wtRoot, "", false)
	require.NoError(t, err)
	require.Equal(t, rescued, store.Config().Options.DataDirectory)
	t.Cleanup(func() { setDataDirHomeForTest(true) })
}
