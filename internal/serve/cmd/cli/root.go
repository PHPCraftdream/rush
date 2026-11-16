// Package cli assembles the Rush command tree. Lane K2 of the sprint0 cmd
// split: command construction moved here from package cmd's init() phase —
// NewRootCmd() builds a fresh root on every call (no package-level rootCmd),
// still single-threaded and before fang.Execute and any RunE. Design:
// docs/plans/2026-11-15-sprint0-split-cmd.md §2.3/§3.2.
package cli

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/audit"
	cmdpkg "github.com/PHPCraftdream/rush/internal/serve/cmd"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	journal "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/journal"
	out "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/serve/server"
	rushweb "github.com/PHPCraftdream/rush/web"
	"github.com/atotto/clipboard"
	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

// webCmd is an explicit alias for the bare `rush` command. Both start the
// embedded web server — webCmd just makes the intent obvious in scripts.
// (Merged from the former cmd/web.go, design §2.2 row 162.)
var webCmd = &cobra.Command{
	Use:   "web",
	Args:  cobra.NoArgs,
	Short: "Start the Rush web UI (same as running 'rush' with no subcommand)",
	Long: `Start the Rush web UI.

Launches an HTTP server with a React UI and a WebSocket endpoint for
real-time communication with the agent. Sessions, permissions, model
selection, logs, message queueing, and turn-interruption (the yellow
"Interrupt" button that cancels the running turn while keeping
everything produced so far) all happen in the browser.

Remote browsers cannot connect to a non-loopback listener: WebSocket origins
are restricted to loopback. For remote access, use SSH port forwarding with
--host localhost.

At startup the server prints the URL and a one-time access token. The
token is also copied to your clipboard. Paste it in the browser to
authenticate — it is never transmitted as part of the URL.

  rush web
  rush web --port 8080 --no-open
  rush web --host 0.0.0.0 --allow-cleartext --port 9000   # bind on all interfaces; remote browsers cannot connect
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWebMode(cmd)
	},
}

// NewRootCmd builds the whole Rush command tree. Every call returns a FRESH
// root command (the former package-level rootCmd is gone); family sub-trees
// come from the exported constructors of package cmd. Persistent flags,
// PersistentPreRunE chain order, and the full AddCommand set are byte-for-byte
// the former root.go init()/var contents.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "rush",
		Short:        "Run the Rush coding agent with a browser-based UI",
		SilenceUsage: true,
		Long: `Rush is an AI coding assistant. Running ` + "`rush`" + ` (or ` + "`rush web`" + `)
starts a local HTTP + WebSocket server, prints the URL and a one-time
access token, and opens your default browser to the UI.

The web UI lets you chat with the agent, switch models per session, inspect
and revoke tool permissions, browse logs, queue messages while the agent
is busy, and interrupt the running turn (yellow Interrupt button) to fold
a correction into the next step while keeping everything produced so far.

Companion CLI subcommands for scripting and CI:
  - ` + "`rush run`" + `             one-shot prompt; --session, --timeout, --max-cost,
                          --max-tokens, --on-finish, --json.
  - ` + "`rush sessions`" + `        list / show / delete / last / tail / locks / watch /
                          pick / grep / cost / diff / tree / fork / cancel / gc / compact.
  - ` + "`rush queue`" + `           batch task queue — add / list / run / rm / clear / show.
  - ` + "`rush models`" + `          use / list / set / unset — atom-based model selection
                          with short codes (o3x, s2h, hl, etc.).
  - ` + "`rush claude-init`" + `     install /rush, /rush-fallback, /wrush and /wcrush
                          slash-commands into ~/.claude/commands/ (global
                          by default; --local/--cwd selects a project).
                          /wrush requires git-worktree isolation; /wcrush
                          also defers tests to the orchestrator's phase 2.
  - ` + "`rush system-prompt`" + `   print the system prompt that would be sent.
  - ` + "`rush ping`" + `            health-check (verify API connectivity).
  - ` + "`rush lock`" + ` / ` + "`rush unlock`" + ` protect or restore settings.

See the FLAGS section below for every top-level flag (--color-scheme,
--cwd, --data-dir, --debug, ...) — each is documented once, on its own
flag registration, not duplicated here.`,
		Example: `
# Start the web UI on a random free port and open the browser
rush

# Pin the port and bind to all interfaces (e.g. for a remote dev box)
# Remote browsers cannot connect; use SSH port forwarding with --host localhost.
rush --host 0.0.0.0 --allow-cleartext --port 9000

# Start the server without opening the browser (useful for IDE integrations)
rush --no-open --port 8080

# Run with debug logging from a specific working directory
rush --debug --cwd /path/to/project

# Use a non-default data directory for state (.rush/)
rush --data-dir /path/to/custom/.rush

# Non-interactive one-shot prompt (see "rush run --help" for more)
rush run "Summarise the changes on this branch"

# Pipe stdin into a one-shot prompt
cat README.md | rush run "Make this more glamorous" > GLAMOROUS_README.md

# With cost cap and timeout (900 = 900 seconds)
rush run --max-cost 0.50 --max-tokens 100k --timeout 900 "refactor storage"

# Idempotent CI invocation with structured output
rush run --session "pr-42" --json "Review the diff" | jq .final_text

# Session management
rush sessions list                    # list all sessions
rush sessions last pr-42 --n 5       # last 5 messages with timestamps
rush sessions tree                    # parent-child hierarchy
rush sessions cancel pr-42            # graceful cancel via DB flag
rush sessions fork pr-42 --at 10     # branch from message 10
rush sessions grep "import error"    # search message text
rush sessions cost --by model        # cost breakdown by model
rush sessions diff pr-42             # files touched (from ToolCalls)
rush sessions pick                   # interactive TUI picker

# Task queue
rush queue add --role smart --max-cost 0.20 < task.prompt
rush queue run --concurrent 2 --stop-on-fail

# Model selection with short codes
rush models use o3x h1l            # Opus 4.7 xhigh + Haiku low
rush models use oh sl                # top Opus high + top Sonnet low

# Install slash-commands & sub-agents for Claude Code
rush claude-init --global
  `,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWebMode(cmd)
		},
	}
	root.PersistentFlags().String("password", "", "Password for protected settings")
	// Chain: the journal start record is written first, before the
	// password check, so denied commands are journalled too.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Apply --global-dir before journaling: the audit log lives under
		// GlobalConfigData and must already be relocated. (Execute() applies
		// it earlier from raw os.Args too; this re-applies the parsed value
		// so the flag is effective even when only cobra sees it.)
		if v, err := cmd.Flags().GetString("global-dir"); err == nil {
			cmdkit.ApplyGlobalDir(v)
		}
		journal.JournalCommandStart(cmd, args)
		if err := settingsPasswordPreRun(cmd, args); err != nil {
			return err
		}
		return cmdpkg.APITokenArgumentGuard(root, cmd, args)
	}
	root.PersistentFlags().StringP("cwd", "c", "", "Working directory rush operates in (absolute or relative). Applies to every subcommand; the .rush/ store and any tool-side relative paths resolve against it.")
	root.PersistentFlags().StringP("data-dir", "D", "",
		"Override the .rush/ data directory (sessions DB, logs, attachments). "+
			"Selection order: this flag; options.data_directory from the config; an existing "+
			"worktree-local .rush with a rush.db (legacy, kept as-is); inside a linked git "+
			"worktree the main checkout's <main>/.rush (shared -- sessions and logs live with "+
			"the project and survive `git worktree remove`); dev builds (go run / go build -o "+
			"in a checkout or temp) isolate to <wt>/.rush/dev so branch builds never touch the "+
			"shared DB; otherwise <cwd>/.rush.")
	root.PersistentFlags().String("global-dir", "",
		"Relocate ALL Rush-owned global state under DIR so an experimental build "+
			"runs side by side with a production install. Fixed layout: DIR/config "+
			"(rush.json config, Rush-owned skills/commands), DIR/data (data file, "+
			"oauth journals, provider/model-catalog caches, projects.json, audit "+
			"logs, heartbeat, bin), DIR/cache. When set, RUSH_GLOBAL_CONFIG, "+
			"RUSH_GLOBAL_DATA, RUSH_CACHE_DIR and XDG_* are ignored for Rush-owned "+
			"global paths — nothing outside DIR is read or written by Rush. Can "+
			"also be set via the RUSH_GLOBAL_DIR environment variable; the flag wins.")
	root.PersistentFlags().BoolP("debug", "d", false, "Debug")
	root.Flags().BoolP("help", "h", false, "Help")
	root.Flags().StringP("host", "H", "localhost", "Host to bind the web UI to")
	root.Flags().Bool("allow-cleartext", false, "Required to bind a non-loopback host; the UI uses plain HTTP and its per-process token is the only protection")
	root.Flags().IntP("port", "p", 0, "Port to bind the web UI to (0 = pick a free one)")
	root.Flags().Bool("no-open", false, "Do not open the browser after the server starts")
	// --color-scheme forces fang's help/error styling onto a light or dark
	// palette, working around terminals (WezTerm on Windows, git-bash) where
	// lipgloss.HasDarkBackground misdetects and renders grey-on-white. The
	// RUSH_COLOR_SCHEME env var does the same; the flag wins if both are set.
	// "auto" (the default) leaves fang's built-in detection untouched.
	//
	// This description string is the ONLY place this flag is documented —
	// cobra/fang auto-render it in every subcommand's FLAGS section, so do
	// NOT also hand-write a duplicate explanation in root.Long or
	// anywhere else; that drifts (it already did once — see git history).
	root.PersistentFlags().String(
		"color-scheme",
		"",
		"Force CLI help/error color palette: light, dark, or auto (default auto). "+
			"Use light on a white-background terminal where auto-detection "+
			"misrenders (e.g. WezTerm on Windows, git-bash). "+
			"Overrides RUSH_COLOR_SCHEME env var when set.",
	)

	// Build a fresh web command for each root construction (fresh-tree contract).
	web := &cobra.Command{Use: webCmd.Use, Short: webCmd.Short, Long: webCmd.Long, Args: webCmd.Args, RunE: webCmd.RunE}
	web.Flags().StringP("host", "H", "localhost", "Host to listen on")
	web.Flags().Bool("allow-cleartext", false, "Required to bind a non-loopback host; the UI uses plain HTTP and its per-process token is the only protection")
	web.Flags().IntP("port", "p", 0, "Port to listen on (0 = random free port)")
	web.Flags().Bool("no-open", false, "Do not open browser automatically")
	// The moved journal/exit-code helpers need the live tree objects before
	// any RunE runs (formerly installed by package cmd's init phase).
	apiCmd := cmdpkg.NewAPICmd()
	tokensCmd := cmdpkg.NewTokensCmd()
	serveCmd := cmdpkg.NewServeCmd()
	journal.UseJournalRoot(root)
	out.SetInvocationCommands(root, apiCmd, tokensCmd, serveCmd)

	root.AddCommand(
		cmdpkg.NewLockCmd(),
		cmdpkg.NewUnlockCmd(),
		cmdpkg.NewRunCmd(),
		cmdpkg.NewDirsCmd(),
		cmdpkg.NewProjectsCmd(),
		cmdpkg.NewUpdateProvidersCmd(),
		cmdpkg.NewLogsCmd(),
		cmdpkg.NewSchemaCmd(),
		cmdpkg.NewPsCmd(),
		cmdpkg.NewLoginCmd(),
		cmdpkg.NewStatsCmd(),
		tokensCmd,
		apiCmd,
		cmdpkg.NewSessionsCmd(),
		serveCmd,
		cmdpkg.NewModelsCmd(),
		cmdpkg.NewPingCmd(),
		cmdpkg.NewPingFastCmd(),
		cmdpkg.NewMigrateCmd(),
		cmdpkg.NewQueueCmd(),
		cmdpkg.NewBatchCmd(),
		cmdpkg.NewProvidersCmd(),
		cmdpkg.NewWorktreeCmd(),
		cmdpkg.NewMcpCmd(),
		cmdpkg.NewClaudeInitCmd(),
		cmdpkg.NewClaudeDelCmd(),
		cmdpkg.NewCliRefreshCmd(),
		cmdpkg.NewCodexInitCmd(),
		cmdpkg.NewCodexDelCmd(),
		cmdpkg.NewGeminiInitCmd(),
		cmdpkg.NewGeminiDelCmd(),
		cmdpkg.NewGrokInitCmd(),
		cmdpkg.NewGrokDelCmd(),
		cmdpkg.NewOmpInitCmd(),
		cmdpkg.NewOmpDelCmd(),
		cmdpkg.NewOpencodeInitCmd(),
		cmdpkg.NewOpencodeDelCmd(),
		cmdpkg.NewQwenInitCmd(),
		cmdpkg.NewQwenDelCmd(),
		cmdpkg.NewResultCmd(),
		cmdpkg.NewRolesCmd(),
		cmdpkg.NewSystemPromptCmd(),
		web,
	)
	return root
}

// settingsPasswordPreRun enforces the --password persistent flag (moved from
// root.go verbatim).
func settingsPasswordPreRun(cmd *cobra.Command, _ []string) error {
	cmdkit.SetSettingsAudit(cmd)
	password, _ := cmd.Flags().GetString("password")
	config.SetProcessPassword(password)
	if password == "" {
		return nil
	}
	if err := config.CheckPasswordAgainstDisk(password); err != nil {
		_ = audit.Write(audit.Event{"kind": "command_denied", "cmd": cmd.CommandPath(), "reason": "wrong password"})
		return err
	}
	return nil
}

// webAppFactory is a test seam (moved from root.go); production default is the
// cmd.SetupApp bridge, which re-installs this package's config options.
var webAppFactory = cmdpkg.SetupApp

func runWebMode(cmd *cobra.Command) error {
	host, _ := cmd.Flags().GetString("host")
	port, _ := cmd.Flags().GetInt("port")
	noOpen, _ := cmd.Flags().GetBool("no-open")
	allowCleartext, _ := cmd.Flags().GetBool("allow-cleartext")
	if !server.IsLoopbackHost(host) && !allowCleartext {
		return out.NewTokenCLIError(2, fmt.Errorf("refusing non-loopback --host %q: pass --allow-cleartext to bind the plain HTTP UI (the per-process token is its only protection); for remote access, use SSH port forwarding with --host localhost", host))
	}

	a, err := webAppFactory(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()
	a.AgentCoordinator.SetPersistentMode(true)

	if !server.IsLoopbackHost(host) {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: rush web is serving unencrypted HTTP; anyone who obtains the token can run commands and edit files as you. Use a tunnel for remote access. Remote browsers cannot connect because WebSocket origins are restricted to loopback.")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	srv := server.NewWithCleartext(a, addr, rushweb.FS(), allowCleartext)
	token := srv.Token()

	onReady := func(boundAddr string) {
		url := fmt.Sprintf("http://%s", boundAddr)
		fmt.Print(webReadyBanner(url, token, clipboard.WriteAll(token) == nil))

		if !noOpen {
			go func() {
				time.Sleep(200 * time.Millisecond)
				if err := browser.OpenURL(url); err != nil {
					slog.Debug("web: could not open browser", "err", err)
				}
			}()
		}
	}

	return srv.Start(cmd.Context(), onReady)
}

func webReadyBanner(url, token string, copied bool) string {
	suffix := ""
	if copied {
		suffix = " (copied to clipboard)"
	}
	return fmt.Sprintf("\n  rush web UI  ->  %s\n  Access token  ->  %s%s\n\n", url, token, suffix)
}
