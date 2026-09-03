package sdk

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/projects"
	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// OpenMode selects how Open resolves configuration and persistence.
type OpenMode int

const (
	// ModeApplication (zero value, default) is today's exact behavior:
	// Options.WorkingDir is required, rush.json/.mcp.json/global config
	// are auto-discovered and loaded from disk. Existing callers who
	// never set Options.Mode are completely unaffected by this feature.
	ModeApplication OpenMode = iota
	// ModeLibrary skips ALL config-file discovery -- every provider and
	// model role comes from Options.LibraryConfig instead. WorkingDir
	// becomes OPTIONAL: empty means an ephemeral, non-persisted session
	// backed by an in-memory SQLite connection (nothing survives past
	// Close(); the caller gets the full session transcript back directly
	// via RunResult/SubscribeMessages, never via disk). A non-empty
	// WorkingDir behaves like Open's normal WorkingDir handling (created
	// if missing) but STILL skips rush.json -- LibraryConfig remains the
	// only config source, just backed by a real, persisted DB under that
	// directory instead of :memory:.
	ModeLibrary
)

// LibraryConfig is the fully-explicit, zero-disk-I/O configuration used
// when Options.Mode == ModeLibrary. Ignored (may be nil) otherwise.
//
// Known limitations, stated honestly: MCP servers are not supported
// (the config's MCP map is empty and .mcp.json is never read); agent
// context files (AGENTS.md/RUSH.md) are not loaded (ContextPaths stays
// empty); and no catwalk provider catalog is fetched.
type LibraryConfig struct {
	Credentials []Credential
	Models      map[Role]ModelChoice

	// AllowConfiguredRoleFallback mirrors
	// CredentialSet.AllowConfiguredRoleFallback -- see that field's
	// doc comment. Inert in library mode: openLibrary only calls
	// Validate(), which does not read this field.
	AllowConfiguredRoleFallback bool
}

// LibraryVirtualRoot is the synthesized logical filesystem root used as
// ConfigStore.WorkingDir() for an ephemeral ModeLibrary session (empty
// Options.WorkingDir, in-memory database, nothing ever on disk).
// ConfigStore.WorkingDir() is the single point of truth every
// folder-scope compilation (permission.FolderScopeSpec.WorkingDir, set
// from the app config's WorkingDir() in app_run.go's ExecuteRun) and
// every scoped fs_* tool's item-path resolution
// (tools.resolveScopedPath's workingDir parameter, itself sourced from
// the SAME WorkingDir() getter in coordinator_tools.go) reads from.
//
// Before R5-6 an ephemeral session left ConfigStore.WorkingDir() as "".
// permission.BuildFolderScope hard-rejects a relative Dir entry when
// WorkingDir is empty, so the README's own natural example
// ({Dir: "src", ...}) could not be used unchanged in library mode. A
// caller who worked around that by supplying an absolute virtual scope
// still hit a second, silent failure: a RELATIVE item path (what a
// model actually emits in an fs_* tool call) was joined onto the empty
// WorkingDir and passed through filepath.Abs, which resolves a
// relative (or Windows driveless-but-rooted) path against the RUSH
// HOST PROCESS's own current working directory/drive -- a value the
// caller never chose and that can change between calls (R5-6, P1 SDK
// review finding). See tools.resolveScopedPath's doc comment for the
// mechanism of that second bug and its fix.
//
// A synthesized path string alone does not make a real disk-backed tool
// virtual (R6-1, P0 SDK review round 6): before that fix, an ephemeral
// session with no caller-supplied DiskProvider still handed the model
// every legacy host-disk tool (bash, write, edit, ...) and the fs_*
// family backed by the REAL OS filesystem, rooted at this sentinel --
// which on Windows is a real, OS-interpreted drive-relative path, and on
// Unix an ordinary absolute host path. Two changes close that hole:
//
//  1. buildLibraryConfig disables every host-disk and command-execution
//     tool by default for an ephemeral session (libraryEphemeralDisabledTools
//     below) -- an ephemeral coder gets NONE of bash/run_command/edit/
//     multiedit/glob/grep/ls/view/write/fs_* unless a call explicitly opts
//     in via RunOverrides.FolderScopes (which internal/agent's
//     applyCallFolderScope re-adds regardless of this default-disabled
//     list -- see that function's doc). bash/run_command have no such
//     opt-in: shell execution cannot be virtualized by a DiskProvider, so
//     they stay hard-denied for every ephemeral call, scoped or not.
//  2. tools.resolveScopedPath now refuses (fails closed, before the
//     first real disk.Stat call) any resolution that would touch the
//     REAL disk under this sentinel -- the backstop for a call that
//     opts into FolderScopes without also supplying a DiskProvider (see
//     tools.rejectRealDiskUnderLibraryVirtualRoot).
//
// LibraryVirtualRoot's value:
//   - It is exported so a host can build absolute-style FolderScopes
//     entries directly against it, or recognize it in returned/audited
//     paths, without guessing the sentinel value.
//   - Computed per-OS (R6-1) to satisfy the REAL, platform-native
//     filepath.IsAbs on every OS, not just internal/filepathext.SmartIsAbs's
//     cross-platform notion of "absolute enough". DiskProvider's own
//     documented contract (internal/agent/tools/fs_provider.go) is that
//     every method receives an absolute path; before this fix the one
//     literal used on Unix ("/rush-library-mode-root") satisfied that on
//     Unix but not on Windows (no drive letter), so a strict custom
//     DiskProvider could correctly reject a path the SDK itself produced.
//     See tools.LibraryVirtualRoot (the canonical definition this
//     re-exports) for the exact per-OS value and the reasoning behind it,
//     including why a real collision with an actual project at this path
//     is a non-issue: it is unconditionally denied on the real disk
//     either way (point 2 above).
//   - It is NOT a "\\host\share" UNC path: stat-ing a non-existent UNC
//     host can hang on a real Windows network name-resolution timeout --
//     moot in practice now that point 2 above refuses the real disk
//     before any Stat is ever issued, but avoided anyway for defense in
//     depth.
var LibraryVirtualRoot = tools.LibraryVirtualRoot

// libraryEphemeralDisabledTools are the built-in tool names removed by
// default from an ephemeral ModeLibrary session (Options.WorkingDir
// empty): every legacy tool that operates directly against a real OS
// path built from c.cfg.WorkingDir() -- for an ephemeral session, the
// synthetic LibraryVirtualRoot, an OS-interpreted real path, not a
// sandbox -- plus the scoped fs_* family, which normalizes to the real
// OS disk whenever a call does not supply RunOverrides.DiskProvider.
// Closes SDK review round 6, finding R6-1 (P0): the README's minimal
// ephemeral example must never hand the model host-disk or
// shell-execution tools by default.
//
// This is not a permanent ban on the fs_* half: internal/agent's
// applyCallFolderScope (internal/agent/coordinator_tools.go) re-adds each
// fs_* tool named in folderScopeOpForTool UNCONDITIONALLY whenever a
// per-call RunOverrides.FolderScopes grants the corresponding operation --
// regardless of this default-disabled list -- so a caller who explicitly
// opts in with FolderScopes (typically together with a DiskProvider) still
// gets a scoped fs_* toolset for that one call. When no DiskProvider is
// supplied for that opt-in call, tools.rejectRealDiskUnderLibraryVirtualRoot
// (internal/agent/tools/fs_library_virtual_root.go) fails the call closed
// rather than falling back to the real disk under the sentinel.
//
// bash/run_command/download have no such opt-in and are never re-added:
// shell execution cannot be virtualized by a DiskProvider (there is no
// seam a FolderScope or DiskProvider could plug into a spawned
// subprocess), and download (internal/agent/tools/download.go) writes
// straight to the real OS filesystem via c.cfg.WorkingDir() with no
// DiskProvider parameter at all -- it is not in folderScopeOpForTool
// (internal/agent/coordinator_tools.go), so applyCallFolderScope can
// never re-add it regardless of a call's FolderScope grants. All three
// stay hard-denied for every ephemeral call, scope or no scope.
var libraryEphemeralDisabledTools = []string{
	"bash", "run_command", "download",
	"edit", "multiedit", "glob", "grep", "ls", "view", "write",
	"fs_list", "fs_find", "fs_grep", "fs_read",
	"fs_write", "fs_replace", "fs_write_lines", "fs_delete",
}

// openLibrary is Open's library-mode path: no config-file discovery at
// all. Everything comes from o.LibraryConfig; persistence is either an
// ephemeral in-memory SQLite session (empty WorkingDir) or a real DB
// under <WorkingDir>/.rush.
func openLibrary(ctx context.Context, o Options) (*Client, error) {
	if o.LibraryConfig == nil {
		return nil, fmt.Errorf("sdk: Options.LibraryConfig is required when Mode == ModeLibrary")
	}
	lc := *o.LibraryConfig
	cs := CredentialSet(lc)
	if err := cs.Validate(); err != nil {
		return nil, fmt.Errorf("sdk: invalid LibraryConfig: %w", err)
	}
	if _, ok := cs.Models[RoleSmart]; !ok {
		// The smart model drives every turn; without it the first Run
		// could only fail later and less clearly.
		return nil, fmt.Errorf("sdk: LibraryConfig.Models must define the smart role")
	}

	var workDir string
	if o.WorkingDir != "" {
		var err error
		workDir, err = resolveWorkingDir(o.WorkingDir)
		if err != nil {
			return nil, err
		}
	}

	// Ephemeral session: dataDir empty means no data directory at all,
	// so nothing is ever written to disk.
	var dataDir string
	if workDir == "" {
		if o.DataDir != "" {
			slog.Warn("sdk: Options.DataDir is ignored in library mode without a WorkingDir; using an ephemeral in-memory session")
		}
	} else if o.DataDir != "" {
		dataDir = o.DataDir
	} else {
		dataDir = filepath.Join(workDir, ".rush")
	}

	// configWorkingDir feeds config.NewLibraryStore, becoming
	// ConfigStore.WorkingDir() -- see LibraryVirtualRoot's doc comment
	// for why an ephemeral session (workDir == "") needs a non-empty
	// synthesized value here even though workDir itself -- the real,
	// on-disk root used for dataDir/database/project-registration below
	// -- correctly stays "".
	configWorkingDir := workDir
	if configWorkingDir == "" {
		configWorkingDir = LibraryVirtualRoot
	}

	// workDir == "" is exactly the ephemeral case (see the dataDir block
	// above) -- buildLibraryConfig uses it to decide whether this
	// session gets the default host-disk/command tool set at all (R6-1).
	cfg := buildLibraryConfig(&lc, dataDir, workDir == "")
	store := config.NewLibraryStore(cfg, configWorkingDir)

	var (
		closeConns []*sql.DB
		conn       *sql.DB
		err        error
	)
	if workDir == "" {
		closeConns, conn, err = openMemoryDB(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("sdk: failed to create data directory %q: %w", dataDir, err)
		}
		conn, err = db.Connect(ctx, dataDir)
		if err != nil {
			return nil, fmt.Errorf("sdk: failed to connect database in %q: %w", dataDir, err)
		}
	}

	if o.SetupLogging && dataDir != "" {
		// See Options.SetupLogging for the process-singleton caveat.
		rushlog.Setup(filepath.Join(dataDir, "logs", "rush.log"), o.Debug)
	} else if o.SetupLogging {
		slog.Warn("sdk: SetupLogging is ignored for an ephemeral in-memory session (no data directory to write logs into)")
	}

	// Register the project only when rooted at a real directory, so
	// `rush projects` / `rush dirs` see embedded sessions the same way
	// they see CLI ones. Non-fatal, exactly like setupApp.
	if workDir != "" {
		if err := projects.Register(workDir, dataDir); err != nil {
			slog.Warn("sdk: failed to register project", "error", err)
		}
	}

	var mcpOpts []app.Option
	if o.MCP == MCPAll {
		mcpOpts = nil
	} else {
		mcpOpts = []app.Option{app.RestrictMCPToCLI()}
	}

	application, err := app.New(ctx, conn, store, mcpOpts...)
	if err != nil {
		slog.Error("sdk: failed to create app instance", "error", err)
		for _, c := range closeConns {
			if cerr := c.Close(); cerr != nil {
				slog.Error("sdk: failed to close in-memory database connection", "error", cerr)
			}
		}
		if closeConns == nil {
			// Ownership split: app.New releases only the ConnectRead
			// reference it acquired internally on this error path; it
			// never took ownership of our conn, so this reference must
			// be released here or the writer pool leaks (on Windows,
			// its file handle with it). Mirrors setupApp in
			// internal/cmd/root.go.
			if relErr := db.Release(dataDir); relErr != nil {
				slog.Error("sdk: failed to release DB connection after app init failure", "error", relErr)
			}
		}
		return nil, fmt.Errorf("sdk: failed to create app instance: %w", err)
	}

	return &Client{app: application, stdout: o.Stdout, stderr: o.Stderr, closeConns: closeConns}, nil
}

// buildLibraryConfig assembles the in-memory Config from a LibraryConfig,
// with zero disk I/O. The Options defaults mirror config.setDefaults'
// minimum guarantees (an Options record with TUI defaults and the
// Assisted-By attribution trailer); MCP is left empty by design and
// agent context paths stay unset (see LibraryConfig's limitations).
//
// ephemeral is true exactly when the caller's Options.WorkingDir was
// empty (openLibrary's workDir == ""): a session with no real working
// directory. In that case Options.DisabledTools is set to
// libraryEphemeralDisabledTools (R6-1, P0 SDK review round 6) so
// SetupAgents' resolveAllowedTools excludes every host-disk and
// command-execution tool from the coder's default toolset -- see that
// var's doc for exactly which tools and why, and for the per-call
// FolderScopes opt-in that still works despite this default.
func buildLibraryConfig(lc *LibraryConfig, dataDir string, ephemeral bool) *config.Config {
	cfg := &config.Config{
		Options: &config.Options{
			DataDirectory: dataDir,
			TUI:           &config.TUIOptions{},
			Attribution: &config.Attribution{
				TrailerStyle:  config.TrailerStyleAssistedBy,
				GeneratedWith: true,
			},
		},
		Providers:    csync.NewMap[string, config.ProviderConfig](),
		Models:       make(map[config.SelectedModelType]config.SelectedModel, len(lc.Models)),
		MCP:          config.MCPs{},
		RecentModels: make(map[config.SelectedModelType][]config.SelectedModel),
	}
	if ephemeral {
		cfg.Options.DisabledTools = libraryEphemeralDisabledTools
	}
	for _, cred := range lc.Credentials {
		cfg.Providers.Set(cred.Provider, libraryProviderConfig(cred))
	}
	for role, choice := range lc.Models {
		// Role and config.SelectedModelType share identical string
		// values, so the cast needs no conversion. Mirrors
		// internal/agent's buildCredentialModel modelCfg construction.
		cfg.Models[config.SelectedModelType(role)] = config.SelectedModel{
			Provider:        choice.Provider,
			Model:           choice.Model,
			ReasoningEffort: choice.ReasoningEffort,
			MaxTokens:       choice.MaxTokens,
		}
	}
	cfg.SetupAgents()
	return cfg
}

// libraryProviderConfig shapes a Credential into the config.ProviderConfig
// buildProvider consumes. It mirrors internal/agent's unexported
// credentialProviderConfig (identical fields, model metadata mapping, and
// reasoning ladder); a local copy is required because that helper is
// package-private to internal/agent and the sdk package must not reach
// into agent internals beyond its public aliases.
func libraryProviderConfig(cred Credential) config.ProviderConfig {
	provCfg := config.ProviderConfig{
		ID:      cred.Provider,
		Name:    cred.Provider,
		Type:    catwalk.Type(cred.Type),
		APIKey:  cred.APIKey,
		BaseURL: cred.BaseURL,
	}
	for _, m := range cred.Models {
		provCfg.Models = append(provCfg.Models, libraryCatwalkModel(cred, m.ID))
	}
	return provCfg
}

// libraryCatwalkModel maps a CredentialModel's metadata onto the
// catwalk.Model shape the Model wrapper carries, exactly like
// internal/agent's credentialCatwalkModel: a model id with no metadata
// entry degrades to the same unverified-minimal shape.
func libraryCatwalkModel(cred Credential, modelID string) catwalk.Model {
	for _, m := range cred.Models {
		if m.ID != modelID {
			continue
		}
		cw := catwalk.Model{
			ID:               m.ID,
			Name:             m.ID,
			ContextWindow:    m.ContextWindow,
			DefaultMaxTokens: m.DefaultMaxTokens,
			CanReason:        m.CanReason,
		}
		if m.CanReason {
			// The same effort ladder agent.credentialReasoningLevels
			// assumes for reasoning-capable models.
			cw.ReasoningLevels = []string{"low", "medium", "high"}
		}
		return cw
	}
	slog.Warn("per-call credential model has no metadata entry; using unverified minimal metadata (cost/context-window unknown)",
		"provider", cred.Provider, "model", modelID)
	return catwalk.Model{ID: modelID, Name: modelID}
}

// memPragmas is the pragma set applied to the in-memory database. Keep
// in sync with internal/db's pragmas; journal_mode is deliberately
// omitted (no WAL for an in-memory DB).
var memPragmas = map[string]string{
	"foreign_keys":  "ON",
	"page_size":     "4096",
	"temp_store":    "MEMORY",
	"cache_size":    "-8000",
	"synchronous":   "NORMAL",
	"secure_delete": "ON",
	"busy_timeout":  "30000",
}

// newMemDSN builds the DSN for one ephemeral client's shared-cache memory
// database, with a unique name per call. Two handles opened on the SAME
// named memory DB share one underlying database -- which the keeper and
// main handle of a single client must do, but two different clients must
// never: SQLite keys shared-cache memory databases by name process-wide,
// so a fixed name would make every ephemeral client in the process open
// (and re-run migrations on) one and the same database, breaking the
// per-client isolation an in-memory session promises. The uuid portion
// keeps each client's database private (google/uuid is already a direct
// dependency).
func newMemDSN() string {
	dsn := fmt.Sprintf("file:rush_sdk_memory_%s?mode=memory&cache=shared", uuid.NewString())
	for _, name := range sortedPragmaNames(memPragmas) {
		dsn += fmt.Sprintf("&_pragma=%s(%s)", name, memPragmas[name])
	}
	return dsn
}

// memoryDriverName picks the registered SQLite driver: modernc registers
// as "sqlite", ncruces as "sqlite3".
func memoryDriverName() (string, error) {
	registered := sql.Drivers()
	for _, candidate := range []string{"sqlite", "sqlite3"} {
		for _, d := range registered {
			if d == candidate {
				return candidate, nil
			}
		}
	}
	sort.Strings(registered)
	return "", fmt.Errorf("sdk: no SQLite driver registered for the in-memory database (have %v)", registered)
}

// openMemoryDB opens the ephemeral in-memory SQLite database and returns
// every handle the caller must eventually close plus the main handle to
// hand to app.New.
//
// Keeper-handle pattern: a named shared-cache memory database lives only
// while at least one connection to it is open. The keeper handle is
// opened and pinged FIRST, which materializes the database and keeps it
// alive for the process lifetime of this Client. The main handle is the
// one the app actually uses. closeConns is ordered main-first,
// keeper-last: the keeper must outlive main, or the memory DB dies under
// a still-open pool.
//
// db.Connect is deliberately not used here: it is hard-wired to a file
// path under a data directory (pooled and reference-counted by path),
// which is exactly what an ephemeral, nothing-on-disk session must not
// have.
func openMemoryDB(ctx context.Context) (closeConns []*sql.DB, conn *sql.DB, err error) {
	driver, err := memoryDriverName()
	if err != nil {
		return nil, nil, err
	}

	// The DSN _pragma params apply at every pooled connection; the
	// modernc spelling is _pragma=name(value). One unique DSN serves
	// both handles below, so the pair shares exactly one database.
	dsn := newMemDSN()

	open := func() (*sql.DB, error) {
		h, err := sql.Open(driver, dsn)
		if err != nil {
			return nil, err
		}
		// One pooled connection per handle: a shared-cache memory DB
		// behind a multi-connection pool would serialize and could see
		// schema locking surprises; single-connection matches
		// internal/db's writer pool shape.
		h.SetMaxOpenConns(1)
		if err := h.PingContext(ctx); err != nil {
			h.Close()
			return nil, err
		}
		return h, nil
	}

	keeper, err := open()
	if err != nil {
		return nil, nil, fmt.Errorf("sdk: failed to open keeper connection for in-memory database: %w", err)
	}
	main, err := open()
	if err != nil {
		keeper.Close()
		return nil, nil, fmt.Errorf("sdk: failed to open main connection for in-memory database: %w", err)
	}

	// Belt and braces: both drivers also accept the DSN _pragma params,
	// but the explicit Exec guarantees the one pooled connection has
	// them regardless of driver DSN quirks.
	for _, name := range sortedPragmaNames(memPragmas) {
		if _, perr := main.ExecContext(ctx, fmt.Sprintf("PRAGMA %s = %s;", name, memPragmas[name])); perr != nil {
			main.Close()
			keeper.Close()
			return nil, nil, fmt.Errorf("sdk: failed to apply pragma %s to in-memory database: %w", name, perr)
		}
	}

	// internal/db's package init already pointed goose at db.FS
	// (goose.SetBaseFS), and internal/db only sets the dialect lazily on
	// its first file Connect, so library mode must set it itself
	// (SetDialect is idempotent).
	if err := goose.SetDialect("sqlite3"); err != nil {
		main.Close()
		keeper.Close()
		return nil, nil, fmt.Errorf("sdk: failed to set goose dialect for in-memory database: %w", err)
	}
	if err := goose.UpContext(ctx, main, "migrations"); err != nil {
		main.Close()
		keeper.Close()
		return nil, nil, fmt.Errorf("sdk: failed to run migrations on in-memory database: %w", err)
	}

	return []*sql.DB{main, keeper}, main, nil
}

// sortedPragmaNames returns the pragma names in deterministic order so
// the DSN and the Exec loop are reproducible.
func sortedPragmaNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
