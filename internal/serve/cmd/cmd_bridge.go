// Package cmd compatibility bridges for lane K1 of the sprint0 cmd split.
//
// lane K1 created this; lanes M1–M6 delete it.
//
// Every helper below forwards to the identifier's new home (cli/cmdkit,
// cli/cmdkit/out, cli/cmdkit/proc, cli/cmdkit/journal) under the original
// lowercase name, so the rest of package cmd — production code and tests
// alike — keeps compiling unchanged. Each cliout/clijournal wrapper also
// re-installs the live command-tree objects the moved code needs (they used
// to be package globals here). Never exports a mutable variable.
package cmd

import (
	"context"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/spf13/cobra"
)

// installCfgSeams installs the config option builder the moved cmdkit
// setup path needs. It touches no command-tree variables, so referencing it
// from webAppFactory's initializer creates no initialization cycle.
func installCfgSeams() {
	cmdkit.UseCommandLoadOptions(commandLoadOptions)
}

// ---- cli/cmdkit (package cmdkit) ----

func setupApp(cmd *cobra.Command) (*app.App, error) {
	installCfgSeams()
	return cmdkit.SetupApp(cmd)
}

func setupAppLite(cmd *cobra.Command) (*app.App, error) {
	installCfgSeams()
	return cmdkit.SetupAppLite(cmd)
}

func setupAppWithOptions(cmd *cobra.Command, opts []app.Option) (*app.App, error) {
	installCfgSeams()
	return cmdkit.SetupAppWithOptions(cmd, opts)
}

func mcpAppOptions(cmd *cobra.Command) []app.Option { return cmdkit.McpAppOptions(cmd) }

func setSettingsAudit(cmd *cobra.Command) { cmdkit.SetSettingsAudit(cmd) }

func checkAppPassword(cmd *cobra.Command, store *config.ConfigStore) error {
	return cmdkit.CheckAppPassword(cmd, store)
}

func initCommandConfig(cmd *cobra.Command) (*config.ConfigStore, string, bool, error) {
	installCfgSeams()
	return cmdkit.InitCommandConfig(cmd)
}

func ResolveCwd(cmd *cobra.Command) (string, error) { return cmdkit.ResolveCwd(cmd) }

func applyGlobalDir(flagValue string) { cmdkit.ApplyGlobalDir(flagValue) }

func resolveSessionID(ctx context.Context, svc session.Service, id string) (session.Session, error) {
	return cmdkit.ResolveSessionID(ctx, svc, id)
}

func scopeFromFlags(cmd *cobra.Command, def config.Scope) (config.Scope, error) {
	return cmdkit.ScopeFromFlags(cmd, def)
}

func cmdFlagString(cmd *cobra.Command, name string) string { return cmdkit.CmdFlagString(cmd, name) }

func parseSinceDuration(s string) (time.Duration, error) { return cmdkit.ParseSinceDuration(s) }

func parsePlainInt(s string) (int, error) { return cmdkit.ParsePlainInt(s) }

// ---- cli/cmdkit/out (package cliout) ----

func short(hash string) string { return cliout.Short(hash) }

func truncate(s string, n int) string { return cliout.Truncate(s, n) }

func dash(s string) string { return cliout.Dash(s) }

func maskKey(k string) string { return cliout.MaskKey(k) }

func age(info os.FileInfo) int { return cliout.Age(info) }

func loadEnvelope(path string) ([]byte, *app.RunResult, map[string]any, error) {
	return cliout.LoadEnvelope(path)
}

func resolveEnvelopePath(ref string) (string, error) { return cliout.ResolveEnvelopePath(ref) }

func apiExitCode(err error) (int, bool) {
	return cliout.ErrorExitCode(err, "api")
}

func tokensExitCode(args []string, err error) (int, bool) {
	return cliout.TokensExitCode(args, err)
}

func tokenStoreError(err error) error { return cliout.TokenStoreError(err) }

func newCLIProgram(model tea.Model, options ...tea.ProgramOption) *tea.Program {
	return cliout.NewCLIProgram(model, options...)
}

// ---- cli/cmdkit/proc (package cliproc) ----

func requestProcessInterrupt() bool { return proc.RequestProcessInterrupt() }

// ---- cli/cmdkit/journal (package clijournal) ----

func journalCommandStart(cmd *cobra.Command, args []string) {
	journal.JournalCommandStart(cmd, args)
}

func journalExecutionStarted() { journal.JournalExecutionStarted() }

func journalEndForPanic() {
	journal.JournalEndForPanic()
}

func finalizeCommandJournalWithCode(rawArgs []string, runErr error, code int) {
	journal.FinalizeCommandJournalWithCode(rawArgs, runErr, code)
}

func exitWithAudit(code int) {
	journal.ExitWithAudit(code)
}

func ensureAuditDirFunc() { journal.EnsureAuditDirFunc() }

func resetCommandJournalForTests() { journal.ResetCommandJournalForTests() }

func tokenCLIExitCode(err error) (int, bool) {
	return cliout.ErrorExitCode(err, "tokens")
}

func maybeDetachConsole() { proc.MaybeDetachConsole() }

func classifyJournalFallback(rawArgs []string, runErr error) (path, kind, fixedErr string, suppressed bool) {
	return journal.ClassifyJournalFallback(rawArgs, runErr)
}

func journalCommandSuppressed(cmd *cobra.Command, path string) bool {
	return journal.JournalCommandSuppressed(cmd, path)
}

func finalizeCommandJournal(rawArgs []string, runErr error) {
	journal.FinalizeCommandJournal(rawArgs, runErr)
}

// InstallProcessStop is the package's public entry point (main.go); the
// implementation moved to cli/cmdkit/proc in lane K1.
func InstallProcessStop() func() { return proc.InstallProcessStop() }

// SetupApp is exported for package cli (lane K2 crossing): cli's
// webAppFactory test seam needs the cfg-seam-aware setup path that lives in
// this package. Forwards to cmdkit.SetupApp with commandLoadOptions installed.
func SetupApp(c *cobra.Command) (*app.App, error) {
	installCfgSeams()
	return cmdkit.SetupApp(c)
}

func isServeInvocation(args []string) bool {
	return cliout.IsInvocation(args, "serve")
}

func isTokensInvocation(args []string) bool {
	return cliout.IsInvocation(args, "tokens")
}
