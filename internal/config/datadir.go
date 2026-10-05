// Data-directory selection (#1143 SD-D): the ordered resolution of the
// effective .rush directory, its recorded source, dev-build isolation,
// and the shared-workspace marker that pins a linked worktree to the
// main checkout's data directory.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// DataDirSource records WHY a process resolved a given data directory
// (WS-3: computed once in Load, stored on the ConfigStore, never changed
// by reload). The empty value behaves as DataDirSourceDefault.
type DataDirSource string

const (
	// DataDirSourceFlag: the --data-dir flag won (priority 1).
	DataDirSourceFlag DataDirSource = "flag"
	// DataDirSourceConfig: options.data_directory from the merged config
	// files won (priority 2).
	DataDirSourceConfig DataDirSource = "config"
	// DataDirSourceLegacyLocal: a pre-existing local <dir>/.rush with a
	// rush.db was adopted (priority 3 -- existing databases are never
	// abandoned by the shared-directory switch; operator decision).
	DataDirSourceLegacyLocal DataDirSource = "legacy-local"
	// DataDirSourceShared: a linked worktree redirected to the main
	// checkout's <main>/.rush (priority 4).
	DataDirSourceShared DataDirSource = "shared"
	// DataDirSourceDevIsolated: a dev build (go run / go build -o inside
	// a checkout or temp) in a linked worktree redirected to
	// <wt>/.rush/dev so branch builds never touch the shared DB
	// (priority 4, dev builds only).
	DataDirSourceDevIsolated DataDirSource = "dev-isolated"
	// DataDirSourceDefault: nothing else applied; <cwd>/.rush (priority 5).
	DataDirSourceDefault DataDirSource = "default"
)

// sharedWorkspaceDirName is the subdirectory of the shared data directory
// holding one marker file per linked worktree pinned to it.
const sharedWorkspaceDirName = "workspaces"

// Test seams. Production code never touches these; config's own tests use
// them to exercise the linked-worktree redirection and the dev-build
// heuristic (both of which are disabled under `go test` so that a plain
// `go test ./...` in a worktree -- including the pre-push hook -- can
// never see, create, or migrate the shared database).
var (
	// osExecutablePath stands in for os.Executable. Tests point it at a
	// fake binary path to drive the dev-build heuristic.
	osExecutablePath = func() (string, error) { return os.Executable() }
	// dataDirRedirectionOverride forces the under-test guard: -1 keeps
	// the production rule (redirection off under testing.Testing()),
	// 0 forces it off, 1 forces it on. Tests set it via
	// setDataDirRedirectionOverride and MUST restore it.
	dataDirRedirectionOverride = -1
)

// redirectionAllowed reports whether priority-4 redirection (linked
// worktree to <main>/.rush or <wt>/.rush/dev) may apply. Under a test
// binary it never applies unless a test forces it on, so `go test` in a
// worktree resolves data directories exactly as before SD-D.
func redirectionAllowed() bool {
	switch dataDirRedirectionOverride {
	case 0:
		return false
	case 1:
		return true
	}
	return !testing.Testing()
}

// resolveDataDirectory applies the full priority order of the
// data-directory selection (design #1143 SD-D section 5):
//
//  1. --data-dir (dataDirFlag)
//  2. options.data_directory (dataDirFromConfig, set only when it came
//     from a config file, not from setDefaults)
//  3. the closest existing .rush bounded by the workspace root: outside
//     a linked worktree it is used as-is; inside one it is used only
//     when it holds a rush.db AND the shared-mode marker is absent
//     (legacy-local, WARN) -- otherwise ignored (WARN when it holds a
//     rush.db: "stray local DB ignored")
//  4. a linked worktree, when redirection is allowed: a dev build goes
//     to <wt>/.rush/dev (dev-isolated), everything else to
//     <main>/.rush (shared)
//  5. <cwd>/.rush (default)
//
// The result is deterministic for a given (workingDir, flags, disk
// state): Load and ResolveDataDirectory both call this one function, so
// rescue commands always agree with what Load stored.
func resolveDataDirectory(workingDir, dataDirFlag, dataDirFromConfig string) (string, DataDirSource) {
	if dataDirFlag != "" {
		return cleanDataDirPath(workingDir, dataDirFlag), DataDirSourceFlag
	}
	if dataDirFromConfig != "" {
		return cleanDataDirPath(workingDir, dataDirFromConfig), DataDirSourceConfig
	}
	return defaultDataDirForWorkspace(workingDir)
}

// cleanDataDirPath absolutizes a data-directory override the same way
// setDefaults always has (SmartJoin against the working directory).
func cleanDataDirPath(workingDir, dir string) string {
	return filepath.Clean(filepathext.SmartJoin(workingDir, dir))
}

// dataDirIsSharedOfLinkedWorktree reports whether dataDir IS the shared
// data directory of a linked worktree (<main>/.rush), whatever source
// picked it: the redirected default (source shared), or an explicit
// --data-dir / options.data_directory pointing there. The directory,
// not the resolution step, defines shared-from-linked (WS-1). Outside a
// linked worktree, or when the probe is unavailable, it is always false.
func dataDirIsSharedOfLinkedWorktree(workingDir, dataDir string, source DataDirSource) bool {
	if source == DataDirSourceShared {
		return true
	}
	wtRoot := worktreeRoot(workingDir)
	if wtRoot == "" {
		return false
	}
	info := linkedWorktreeInfo(wtRoot)
	if info == nil || info.mainRoot == "" {
		return false
	}
	return sameDir(dataDir, filepath.Join(info.mainRoot, defaultDataDirectory))
}

// defaultDataDirForWorkspace implements priorities 3-5 for the case
// where neither the flag nor the config named a directory.
func defaultDataDirForWorkspace(workingDir string) (string, DataDirSource) {
	fallback := filepath.Join(workingDir, defaultDataDirectory)

	wtRoot := worktreeRoot(workingDir)
	if wtRoot == "" {
		// Not inside any git working tree: the closest .rush (bounded
		// by the working directory itself) or <cwd>/.rush, as before.
		if local, ok := fsext.LookupClosestBounded(workingDir, workingDir, defaultDataDirectory); ok {
			return local, DataDirSourceLegacyLocal
		}
		return fallback, DataDirSourceDefault
	}

	info := linkedWorktreeInfo(wtRoot)
	localDir, hasLocalRushDB := closestLocalRushDB(workingDir, wtRoot)

	if info != nil {
		// Linked worktree. An existing local rush.db without the
		// shared-mode marker stays local (operator decision: existing
		// worktree databases are never abandoned); with the marker it
		// is a stray and is ignored.
		mainDataDir := filepath.Join(info.mainRoot, defaultDataDirectory)
		if hasLocalRushDB && !sharedMarkerExists(mainDataDir, wtRoot) {
			slog.Warn("Using legacy local data directory in linked worktree",
				"data_dir", localDir,
				"hint", "remove the directory or let the shared switch adopt this worktree")
			return localDir, DataDirSourceLegacyLocal
		}
		if hasLocalRushDB {
			slog.Warn("Stray local DB ignored; linked worktree uses the shared data directory",
				"ignored_data_dir", localDir,
				"shared_data_dir", mainDataDir)
		}
		if redirectionAllowed() {
			if isDevBuild() {
				return filepath.Join(wtRoot, defaultDataDirectory, "dev"), DataDirSourceDevIsolated
			}
			if info.mainRoot != "" {
				return filepath.Join(info.mainRoot, defaultDataDirectory), DataDirSourceShared
			}
		}
		return fallback, DataDirSourceDefault
	}

	// Not linked (main checkout, submodule, bare, or probe unavailable):
	// the historical bounded lookup, unchanged.
	if local, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
		return local, DataDirSourceLegacyLocal
	}
	return fallback, DataDirSourceDefault
}

// closestLocalRushDB reports the closest .rush directory under wtRoot
// that contains a rush.db, or whether any closest .rush exists at all.
// Only the rush.db file marks a legacy local database (design amendment:
// .rush/ appears without a database too -- skills, system-prompts, dev).
func closestLocalRushDB(workingDir, wtRoot string) (string, bool) {
	local, ok := fsext.LookupClosestBounded(workingDir, wtRoot, defaultDataDirectory)
	if !ok {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(local, "rush.db")); err == nil {
		return local, true
	}
	return local, false
}

// linkedWorktree is the cached classification of one workspace root:
// non-nil only when the root is a LINKED git worktree whose main
// checkout was positively identified.
type linkedWorktree struct {
	mainRoot string
}

var linkedWorktreeCache sync.Map // map[string]*linkedWorktree

// linkedWorktreeInfo classifies wtRoot (design section 3). It returns
// nil when wtRoot is not a linked worktree: main checkout, submodules,
// bare repositories, missing/old git, any probe error, or the under-test
// guard. The result is cached per canonical directory for the process
// lifetime (same rationale as worktreeRootCache).
func linkedWorktreeInfo(wtRoot string) *linkedWorktree {
	if cached, ok := linkedWorktreeCache.Load(wtRoot); ok {
		return cached.(*linkedWorktree)
	}
	if !redirectionAllowed() {
		// Under a test binary without the seam: answer "not linked"
		// WITHOUT caching, so a later test may turn the seam on for the
		// same directory.
		return nil
	}
	info := probeLinkedWorktree(wtRoot)
	linkedWorktreeCache.Store(wtRoot, info)
	return info
}

// probeLinkedWorktree runs the actual git probes. Any error means "not
// linked": the classification degrades to the pre-SD-D behavior instead
// of guessing (fail-closed for correctness, open for availability).
func probeLinkedWorktree(wtRoot string) *linkedWorktree {
	if !redirectionAllowed() {
		return nil
	}
	gitDir, commonDir, err := revParseGitDirs(wtRoot)
	if err != nil || gitDir == "" || commonDir == "" || sameDir(gitDir, commonDir) {
		// Main checkout or submodule (git-dir == common-dir), or the
		// probe failed: not linked. A probe that actually ran is cached
		// by the caller; here the answer is definitive.
		return nil
	}

	mainRoot, err := mainWorktreeRoot(wtRoot)
	if err != nil || mainRoot == "" {
		return nil
	}
	if sameDir(mainRoot, wtRoot) {
		// `worktree list` reports this root as the main one: not linked.
		return nil
	}
	info, err := os.Stat(mainRoot)
	if err != nil || !info.IsDir() {
		slog.Warn("Linked worktree main checkout not found; keeping local data directory",
			"main_root", mainRoot)
		return nil
	}
	if err := checkMainRootOwner(mainRoot, wtRoot); err != nil {
		slog.Warn("Linked worktree main checkout rejected; keeping local data directory",
			"main_root", mainRoot, "reason", err)
		return nil
	}
	return &linkedWorktree{mainRoot: mainRoot}
}

// revParseGitDirs returns the resolved git dir and common git dir for
// dir. Git exports GIT_DIR/GIT_WORK_TREE/GIT_COMMON_DIR/GIT_INDEX_FILE
// into hook environments; they are stripped so a hook-invoked rush sees
// the real repository geometry, not the hook's.
func revParseGitDirs(dir string) (string, string, error) {
	cmd := platform.Command(context.Background(),
		"git", "rev-parse", "--git-dir", "--git-common-dir")
	cmd.Dir = dir
	stripGitWorktreeEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("unexpected git rev-parse output: %q", strings.TrimSpace(string(out)))
	}
	return canonicalGitPath(dir, lines[0]), canonicalGitPath(dir, lines[1]), nil
}

// mainWorktreeRoot parses `git worktree list --porcelain` and returns
// the first entry's path: the main worktree. --path-format=absolute
// needs git >= 2.31 (Ubuntu 20.04 ships 2.25), so the output is parsed
// manually and slash paths are converted; bare main repositories are
// rejected (`bare` line), as is any probe error.
func mainWorktreeRoot(dir string) (string, error) {
	cmd := platform.Command(context.Background(),
		"git", "worktree", "list", "--porcelain")
	cmd.Dir = dir
	stripGitWorktreeEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// The first record is the main worktree. The `bare` attribute line
	// follows the `worktree` path line inside the record, so the whole
	// record is consumed before deciding -- a bare main repository is
	// rejected, not adopted as the main checkout.
	first := strings.Split(string(out), "\n\n")[0]
	var path string
	bare := false
	for _, line := range strings.Split(first, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "bare":
			bare = true
		}
	}
	if path == "" {
		return "", fmt.Errorf("worktree list has no main entry")
	}
	if bare {
		return "", fmt.Errorf("main worktree is bare")
	}
	return canonicalGitPath(dir, path), nil
}

// canonicalGitPath resolves a git-reported path (possibly relative to
// cmdDir, possibly slash-separated) to a canonical absolute path.
func canonicalGitPath(cmdDir, p string) string {
	p = filepath.FromSlash(strings.TrimSpace(p))
	if !filepath.IsAbs(p) {
		p = filepath.Join(cmdDir, p)
	}
	return canonicalConfigPath(p)
}

// stripGitWorktreeEnv removes the GIT_* variables git exports to hooks
// (pre-push runs the test suite, which loads config) so probes observe
// the real repository instead of the hook's view.
func stripGitWorktreeEnv(cmd *exec.Cmd) {
	stripped := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true,
		"GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if i := strings.IndexByte(entry, '='); i > 0 && stripped[entry[:i]] {
			continue
		}
		filtered = append(filtered, entry)
	}
	cmd.Env = filtered
}

// checkMainRootOwner rejects a main checkout whose owner differs from
// the working directory's owner on platforms with real ownership
// (POSIX): adopting another user's data directory would run against
// someone else's sessions and locks.
func checkMainRootOwner(mainRoot, workingDir string) error {
	mainOwner, err := fsext.Owner(mainRoot)
	if err != nil {
		return nil // Ownership unknown on this platform: allow.
	}
	wtOwner, err := fsext.Owner(workingDir)
	if err != nil {
		return nil
	}
	if mainOwner != wtOwner {
		return fmt.Errorf("owner %d differs from worktree owner %d", mainOwner, wtOwner)
	}
	return nil
}

// devBuildExeDirsCache memoizes the dev-build verdict: os.Executable is
// stable for the process lifetime.
var devBuildCache atomic.Pointer[bool]

// isDevBuild reports whether the running binary is a development build:
// located under os.TempDir() / $GOTMPDIR (`go run`, `go test`) or inside
// any git working tree (`go build -o` inside a checkout). A deployed
// binary (PATH, npm, `go install`) lives in none of these and is NOT a
// dev build. Dev builds in a linked worktree are isolated to
// <wt>/.rush/dev and never migrate the shared database (design
// section 2).
func isDevBuild() bool {
	if cached := devBuildCache.Load(); cached != nil {
		return *cached
	}
	dev := detectDevBuild()
	devBuildCache.Store(&dev)
	return dev
}

func detectDevBuild() bool {
	exe, err := osExecutablePath()
	if err != nil || exe == "" {
		return false
	}
	exeDir := filepath.Dir(exe)
	for _, tempDir := range []string{os.TempDir(), os.Getenv("GOTMPDIR")} {
		if tempDir == "" {
			continue
		}
		if inDir(exeDir, tempDir) {
			return true
		}
	}
	// `go build -o` inside a checkout: the binary sits in a git
	// working tree. Deployed targets never do.
	return worktreeRoot(exeDir) != ""
}

// inDir reports whether path is dir itself or somewhere below it.
func inDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

// SharedWorkspaceMarkerPath returns the marker file path pinning
// wsRoot to the shared data directory mainDataDir: a content-free
// (workspace-path-annotated) file under <main>/.rush/workspaces named
// by a short hash of the canonical workspace root, so path case,
// trailing separators, and aliasing cannot split the namespace.
func SharedWorkspaceMarkerPath(mainDataDir, wsRoot string) string {
	sum := sha256.Sum256([]byte(canonicalConfigPath(wsRoot)))
	return filepath.Join(mainDataDir, sharedWorkspaceDirName, hex.EncodeToString(sum[:16]))
}

// sharedMarkerExists reports whether wsRoot is already pinned to
// mainDataDir. An unreadable marker directory is treated as absent so a
// transient failure degrades to the legacy-local rule, never to a silent
// redirect away from an existing local database.
func sharedMarkerExists(mainDataDir, wsRoot string) bool {
	if mainDataDir == "" {
		return false
	}
	_, err := os.Stat(SharedWorkspaceMarkerPath(mainDataDir, wsRoot))
	return err == nil
}

// MarkSharedWorkspace creates the shared-mode marker for wsRoot under
// mainDataDir (design section 5: setupApp/setupAppLite/sdk create it
// once a process actually resolved source=shared). The file records the
// workspace root it pins for diagnosability. Creating it is idempotent.
func MarkSharedWorkspace(mainDataDir, wsRoot string) error {
	path := SharedWorkspaceMarkerPath(mainDataDir, wsRoot)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create shared workspace marker directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(canonicalConfigPath(wsRoot)+"\n"), 0o600); err != nil {
		return fmt.Errorf("failed to write shared workspace marker: %w", err)
	}
	return nil
}

// workspaceHomeFlag is the process-wide "owns its own data directory"
// flag behind WorkspaceHome. It defaults to true (every pre-SD-D process
// is home) and Load stores !dataDirIsSharedOfLinkedWorktree exactly once;
// reload never touches it (WS-3).
var workspaceHomeFlag atomic.Bool

func init() { workspaceHomeFlag.Store(true) }

// setDataDirHomeForTest overrides the process home flag from white-box
// tests. Tests using it must restore true.
func setDataDirHomeForTest(v bool) { workspaceHomeFlag.Store(v) }

// setDataDirRedirectionOverride sets the under-test guard override and
// returns a restore function for defer.
func setDataDirRedirectionOverride(v int) func() {
	prev := dataDirRedirectionOverride
	dataDirRedirectionOverride = v
	return func() { dataDirRedirectionOverride = prev }
}

// setOSExecutablePathForTest points the dev-build heuristic at a fake
// binary path and returns a restore function for defer. It also drops
// the dev-build verdict cache: without that, the first test to trigger
// isDevBuild would freeze its answer for every later test in the binary.
func setOSExecutablePathForTest(path string) func() {
	prev := osExecutablePath
	osExecutablePath = func() (string, error) { return path, nil }
	devBuildCache.Store(nil)
	return func() {
		osExecutablePath = prev
		devBuildCache.Store(nil)
	}
}
