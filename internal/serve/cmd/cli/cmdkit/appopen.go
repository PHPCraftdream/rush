package cmdkit

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/app/projects"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/audit"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
)

// commandLoadOptions is installed by the cmd package (it needs the CLI
// models + discovery scope of the command tree); UseCommandLoadOptions is
// the single installer (lane K1 crossing: initCommandConfig needs the
// cmd-local option builder).
var commandLoadOptions func(*cobra.Command) []config.LoadOption

// UseCommandLoadOptions installs the config.Init option builder used by
// InitCommandConfig. It must be called before any command runs.
func UseCommandLoadOptions(f func(*cobra.Command) []config.LoadOption) {
	commandLoadOptions = f
}

// LoadOptionsInstalled reports whether the cmd package installed the option
// builder; InitCommandConfig cannot run without it.
func LoadOptionsInstalled() bool { return commandLoadOptions != nil }

// guarded by the write path itself. Same refusal shape as the pre-run check.
func CheckAppPassword(cmd *cobra.Command, store *config.ConfigStore) error {
	password, _ := cmd.Flags().GetString("password")
	if password == "" {
		return nil
	}
	if err := store.CheckProcessPassword(); err != nil {
		_ = audit.Write(audit.Event{"kind": "command_denied", "cmd": cmd.CommandPath(), "reason": "wrong password"})
		return err
	}
	return nil
}

func SetSettingsAudit(cmd *cobra.Command) {
	journal.EnsureAuditDirFunc()
	config.SetSettingsAuditSink(func(ev config.SettingsEvent) {
		_ = audit.Write(audit.Event{"kind": "settings_change", "scope": ev.Scope, "source": ev.Source, "path": ev.Path, "keys": ev.Keys, "models": ev.Models, "outcome": ev.Outcome, "reason": ev.Reason, "cmd": cmd.CommandPath()})
	})
}

// the absolute, cleaned flag value (flag wins over a pre-set env var),
// then eagerly creates the DIR/config, DIR/data, DIR/cache layout. An
// empty value is a no-op — an env-only setting is honored directly by the
// config resolvers, which create their own directories as they do today.
func ApplyGlobalDir(flagValue string) {
	if flagValue == "" {
		return
	}
	abs, err := filepath.Abs(flagValue)
	if err != nil {
		abs = filepath.Clean(flagValue)
	}
	os.Setenv(config.GlobalDirEnv, abs)
	config.EnsureGlobalDirTree()
}

// mcpAppOptions decides whether app.New should restrict MCP startup to

// intermediate group command as its parent. That single distinction is
// what "CLI mode" means here: no code path other than runWebMode's own
// setupApp call reaches this function with a nil parent.
//
// The "all-mcp" flag only exists on `rush run` (see run.go); every other
// command's GetBool call below returns false (with an ignored error) as
// if the flag were never set, which is exactly the right default — those
// commands have no equivalent override and should stay gated.
func McpAppOptions(cmd *cobra.Command) []app.Option {
	if cmd.Parent() == nil {
		return nil
	}
	allMCP, _ := cmd.Flags().GetBool("all-mcp")
	if allMCP {
		return nil
	}
	return []app.Option{app.RestrictMCPToCLI()}
}

// cmdFlagString reads a string flag, returning "" when the command has
// no such flag — used for best-effort attribution (process start log

func InitCommandConfig(cmd *cobra.Command) (*config.ConfigStore, string, bool, error) {
	SetSettingsAudit(cmd)
	password, _ := cmd.Flags().GetString("password")
	config.SetProcessPassword(password)
	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, "", false, err
	}
	dataDir, _ := cmd.Flags().GetString("data-dir")
	debug, _ := cmd.Flags().GetBool("debug")
	store, err := config.Init(cwd, dataDir, debug, commandLoadOptions(cmd)...)
	if err != nil {
		return nil, "", false, err
	}
	if err := CheckAppPassword(cmd, store); err != nil {
		return nil, "", false, err
	}
	return store, cwd, debug, nil
}

// setupApp handles the common setup logic for both interactive and non-interactive modes.

func SetupApp(cmd *cobra.Command) (*app.App, error) {
	return SetupAppWithOptions(cmd, McpAppOptions(cmd))
}

func SetupAppWithOptions(cmd *cobra.Command, opts []app.Option) (*app.App, error) {
	ctx := cmd.Context()
	store, cwd, debug, err := InitCommandConfig(cmd)
	if err != nil {
		return nil, err
	}
	if err := store.ModelsIntegrity(); err != nil {
		return nil, err
	}

	cfg := store.Config()
	if err := createDotRushDir(cfg.Options.DataDirectory); err != nil {
		return nil, err
	}

	// Fork merge note: when we dropped upstream's serverCmd + connectToServer
	// during the May-16 merge we accidentally dropped the only two callers
	// of rushlog.Setup(). The slog.Default() handler then stayed at the
	// terminal-writing default, so .rush/logs/rush.log silently stopped
	// receiving new entries for both `rush` (web) and `rush run`. Wiring
	// the call here in setupApp re-points the default logger at the same
	// file path the WUI/Logs modal already expects to read from.
	rushlog.Setup(filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log"), debug)
	rushlog.ProcessStart(cmd.CommandPath(), CmdFlagString(cmd, "session"))

	// Register this project in the centralized projects list.
	if err := projects.Register(cwd, cfg.Options.DataDirectory); err != nil {
		slog.Warn("Failed to register project", "error", err)
		// Non-fatal: continue even if registration fails
	}

	// Connect to DB; this will also run migrations.
	conn, err := connectAppDB(ctx, store, cfg)
	if err != nil {
		return nil, err
	}

	appInstance, err := app.New(ctx, conn, store, opts...)
	if err != nil {
		slog.Error("Failed to create app instance", "error", err)
		// Ownership split (task #778): app.New only ever releases the
		// ConnectRead reference IT acquired internally on this error
		// path; it never took ownership of conn (our own db.Connect
		// above), so we must release that reference ourselves here or
		// it leaks the writer's pool reference — and, on Windows, its
		// underlying file handle — for the life of the process.
		if relErr := db.ReleaseConn(conn); relErr != nil {
			slog.Error("Failed to release DB connection after app init failure", "error", relErr)
		}
		return nil, err
	}

	return appInstance, nil
}

// setupAppLite is identical to setupApp except the returned App skips
// recoverInterruptedTurns, mcp.Initialize, and InitCoderAgent (task #773,
// via app.SkipAgentSetup) — AgentCoordinator and RunQueuePump are left nil.
//
// Use this ONLY for commands that never read `a.AgentCoordinator` and never
// depend on the startup recovery sweep having already run: the entire
// `rush models *` family (state/list/use/bump/unset), `rush providers *`,
// `rush mcp *`, `rush login`, `rush queue add/list/show/rm/clear`, and
// `rush sessions compact` (Config()/DB() only; it must never start the
// run-queue pump or the coder agent)
// (task #772's classification (b), config-only / DB-only, zero App-service
// dependency beyond a.Config()/a.Store()/a.DB()).
//
// Do NOT use this for: `rush` (web), `rush run`, `rush system-prompt`
// (needs AgentCoordinator.BuildSystemPrompt), the rest of the
// `rush sessions *` family, or `rush queue run` (task #772's
// classification (c) — these read live session/message state that
// recoverInterruptedTurns keeps sane, even though they too skip

func SetupAppLite(cmd *cobra.Command) (*app.App, error) {
	ctx := cmd.Context()
	store, cwd, debug, err := InitCommandConfig(cmd)
	if err != nil {
		return nil, err
	}

	cfg := store.Config()
	if err := createDotRushDir(cfg.Options.DataDirectory); err != nil {
		return nil, err
	}

	rushlog.Setup(filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log"), debug)
	rushlog.ProcessStart(cmd.CommandPath(), CmdFlagString(cmd, "session"))

	if err := projects.Register(cwd, cfg.Options.DataDirectory); err != nil {
		slog.Warn("Failed to register project", "error", err)
	}

	conn, err := connectAppDB(ctx, store, cfg)
	if err != nil {
		return nil, err
	}

	appInstance, err := app.New(ctx, conn, store, app.SkipAgentSetup())
	if err != nil {
		slog.Error("Failed to create app instance", "error", err)
		// See the matching comment in setupApp: app.New only releases the
		// ConnectRead reference it took itself on this path; our own
		// db.Connect reference (conn) is ours to release.
		if relErr := db.ReleaseConn(conn); relErr != nil {
			slog.Error("Failed to release DB connection after app init failure", "error", relErr)
		}
		return nil, err
	}

	return appInstance, nil
}

// stdinReadGraceDefault bounds how long MaybePrependStdin waits for a named
// pipe to produce data before giving up on it. A `< file` redirect (regular
// file) is always fully available immediately and is read directly with
// no bound. A `|` pipe, though, can be connected to a writer that never
// sends anything and never closes — observed in practice when a
// background shell job's stdin fd is left open but unused (e.g. a
// launcher that runs `rush run "$(...)"` without an explicit `< file`
// redirect, so the process's stdin resolves to a dangling pipe instead of
// a closed/empty one). io.ReadAll would then block forever: this happens
// BEFORE --timeout's context deadline is even wired up, and without an
// explicit --timeout the hard-kill backstop defaults to 6h — so rush
// would sit doing nothing, invisible to `sessions list` (no session row
// exists yet), for hours. rush must never hang like that regardless of
// how it was invoked.
//
// It is a const (not a mutable package var) so tests inject a shorter grace
// via the parameterized maybePrependStdin below instead of mutating shared
// state — a mutable package var here would be the same
// test-isolation footgun already eliminated elsewhere in this session
// (internal/agent/agent.go's sessionPreambleMaxDuration /
// titleGenerationMaxDuration): fine only as long as no test using it ever

func ResolveCwd(cmd *cobra.Command) (string, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd != "" {
		// Absolutise before chdir: callers re-resolve the returned path,
		// and a relative one would be applied twice from the new cwd.
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return "", fmt.Errorf("failed to resolve directory: %v", err)
		}
		if err := os.Chdir(abs); err != nil {
			return "", fmt.Errorf("failed to change directory: %v", err)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %v", err)
	}
	return cwd, nil
}

// ResolveSessionID resolves a session by exact UUID or hash prefix.
func ResolveSessionID(ctx context.Context, svc session.Service, id string) (session.Session, error) {
	if s, err := svc.Get(ctx, id); err == nil {
		return s, nil
	}

	sessions, err := svc.List(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	if len(matches) == 0 {
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "session ID '%s' is ambiguous. Matches:\n\n", id)
	for _, s := range matches {
		fmt.Fprintf(&sb, "  %s  %s\n", session.HashID(s.ID), s.Title)
	}
	return session.Session{}, fmt.Errorf("%s", sb.String())
}
