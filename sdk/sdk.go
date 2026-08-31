// Package sdk is the public, embeddable surface of Rush (phase 4 of
// docs/plans/2026-08-29-embeddable-library-refactoring.md): it exposes the
// engine behind `rush run` as a plain Go library call — Open a Client
// against a working directory, Run non-interactive agent turns, and get a
// typed result envelope back.
//
// The package is a thin facade over internal/app (legal: the Go internal/
// rule is module-scoped, and this package lives in the same module). It
// reproduces the CLI's setupApp sequence from internal/cmd/root.go minus
// the three things a library must not do:
//
//   - it never calls os.Chdir — the working directory comes in through
//     Options.WorkingDir and the process-wide cwd is left untouched;
//   - it does not touch slog.Default() unless explicitly asked via
//     Options.SetupLogging;
//   - it has no cobra/flag dependency — every parameter arrives through
//     Options and RunRequest.
//
// v1 boundaries, stated honestly:
//
//   - One Client per process. MCP server startup
//     (internal/agent/tools/mcp) is guarded by a process-wide sync.Once,
//     so a second Open in the same process would not start its own MCP
//     servers. The primary embedding scenario — one working directory per
//     process — is unaffected; run one process per workspace (the same
//     model `rush run` already uses, with lock-file + heartbeat, the
//     battle-tested path in the sessions_* CLI family).
//
//   - Core logging is redirected only if you ask for it. With
//     SetupLogging false (the default) Open does not hijack the host's
//     slog.Default(), but the rush core still logs through package-level
//     slog.Info/Warn/Error in many places, so internal log lines go to
//     whatever the host's current slog.Default() is. See the SetupLogging
//     field's doc for the full caveat.
package sdk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	rushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/projects"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
)

// Type aliases onto internal/app's wire-stable run types. Aliases (not
// new types) so sdk and internal/app values are fully interchangeable
// with zero conversion code; the JSON shapes are unchanged.
type (
	RunRequest       = app.RunRequest
	CloseResult      = app.ShutdownResult
	RunResult        = app.RunResult
	RunOverrides     = app.RunOverrides
	RunMode          = app.RunMode
	UsageInfo        = app.UsageInfo
	SessionUsageInfo = app.SessionUsageInfo
	ToolCallStat     = app.ToolCallStat
	SubAgentOutput   = app.SubAgentOutput
	RecoveredPartial = app.RecoveredPartial
)

// Structured-event aliases onto the same raw types the web UI's WS layer
// consumes through app.App's Sessions/Messages brokers (see
// internal/server/events.go). These are pass-through aliases, deliberately
// not a new wire contract: message.Message and session.Session carry no
// JSON tags on purpose — internal/server re-shapes them into its own
// browser-specific wire structs, while sdk hands consumers the raw Go
// values so each consumer serialises them for its own API as it sees fit.
type (
	Message      = message.Message
	Session      = session.Session
	MessageEvent = pubsub.Event[Message]
	SessionEvent = pubsub.Event[Session]
)

// Run output modes, mirroring app.RunMode* (see internal/app for the
// per-mode streaming behaviour).
const (
	RunModeTerse  = app.RunModeTerse
	RunModeStream = app.RunModeStream
	RunModeJSON   = app.RunModeJSON
)

// MCPMode selects which MCP servers Open starts.
type MCPMode int

const (
	// MCPEnabledInCLI (default, zero value) starts only MCP servers whose
	// config sets enabled_in_cli — mirrors `rush run`'s default.
	MCPEnabledInCLI MCPMode = iota
	// MCPAll starts every non-disabled MCP server — mirrors
	// `rush run --all-mcp` and the interactive web UI's behaviour.
	MCPAll
)

// Options configures Open.
type Options struct {
	// WorkingDir is the working directory the client operates in
	// (project config discovery, tool paths, sessions). Relative paths
	// are resolved to absolute at Open. Open NEVER calls os.Chdir, so
	// the process-wide working directory — and anything in the host
	// process that depends on it — is unaffected.
	WorkingDir string
	// DataDir overrides the data directory (sessions DB, logs). Empty
	// means <WorkingDir>/.rush, same as the CLI default.
	DataDir string
	// Debug enables debug logging and config verbosity.
	Debug bool
	// MCP selects which MCP servers are started. Default
	// MCPEnabledInCLI. There is deliberately no "off" mode: app.New has
	// no such concept, and adding one would be a new feature, not a
	// refactor.
	MCP MCPMode
	// Stdout is the default destination for run output when a RunRequest
	// does not carry its own Stdout. When neither is set, ExecuteRun
	// discards output. Set it once here to cover every subsequent
	// Client.Run.
	Stdout io.Writer
	// Stderr is the default destination for run diagnostics (tool-call
	// heartbeat, progress, guidance) when a RunRequest does not carry its
	// own Stderr.
	Stderr io.Writer
	// SetupLogging, when true, makes Open call internal/log.Setup — the
	// same call the CLI makes — pointing slog.Default() at
	// <DataDir>/logs/rush.log.
	//
	// Honest caveat: log.Setup is a process-wide sync.Once singleton. The
	// first call in a process wins; several sdk.Open calls in one process
	// with different SetupLogging values or log paths all get logs from
	// the first one only. When SetupLogging is false (the default) Open
	// does not touch slog.Default() at all — that is the core v1
	// guarantee: embedding Rush never silently steals the host's logger.
	SetupLogging bool
}

// Client is a handle to one embedded Rush instance rooted at
// Options.WorkingDir.
type Client struct {
	app    *app.App
	stdout io.Writer
	stderr io.Writer

	// closeOnce guarantees the wrapped App's shutdown runs at most once
	// per Client, no matter how many times Close is called. closeResult
	// caches that single run's outcome for repeat callers.
	closeOnce   sync.Once
	closeResult CloseResult
}

// Open wires up a full Rush instance for the given working directory:
// config load (project rush.json discovery from WorkingDir), data
// directory creation, project registration, database connect plus
// migrations, and app construction with the requested MCP mode. It is the
// library equivalent of internal/cmd's setupApp, minus os.Chdir,
// unconditional logging setup, and cobra.
func Open(ctx context.Context, o Options) (*Client, error) {
	if o.WorkingDir == "" {
		return nil, fmt.Errorf("sdk: Options.WorkingDir is required")
	}
	workDir, err := filepath.Abs(o.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("sdk: failed to resolve working directory %q: %w", o.WorkingDir, err)
	}
	if info, statErr := os.Stat(workDir); statErr != nil {
		return nil, fmt.Errorf("sdk: working directory %q is not accessible: %w", workDir, statErr)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("sdk: working directory %q is not a directory", workDir)
	}

	store, err := config.Init(workDir, o.DataDir, o.Debug)
	if err != nil {
		return nil, fmt.Errorf("sdk: failed to load config for %q: %w", workDir, err)
	}
	cfg := store.Config()

	// Same data-directory preparation as the CLI's createDotRushDir,
	// minus the .gitignore bookkeeping (a CLI-workspace nicety, not a
	// library concern).
	if err := os.MkdirAll(cfg.Options.DataDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("sdk: failed to create data directory %q: %w", cfg.Options.DataDirectory, err)
	}

	if o.SetupLogging {
		// See Options.SetupLogging for the process-singleton caveat.
		rushlog.Setup(filepath.Join(cfg.Options.DataDirectory, "logs", "rush.log"), o.Debug)
	}

	// Register the project so `rush projects` / `rush dirs` see embedded
	// sessions the same way they see CLI ones. Non-fatal, exactly like
	// setupApp.
	if err := projects.Register(workDir, cfg.Options.DataDirectory); err != nil {
		slog.Warn("sdk: failed to register project", "error", err)
	}

	conn, err := db.Connect(ctx, cfg.Options.DataDirectory)
	if err != nil {
		return nil, fmt.Errorf("sdk: failed to connect database in %q: %w", cfg.Options.DataDirectory, err)
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
		// Ownership split: app.New releases only the ConnectRead
		// reference it acquired internally on this error path; it never
		// took ownership of our conn, so this reference must be released
		// here or the writer pool leaks (on Windows, its file handle
		// with it). Mirrors setupApp in internal/cmd/root.go.
		if relErr := db.Release(cfg.Options.DataDirectory); relErr != nil {
			slog.Error("sdk: failed to release DB connection after app init failure", "error", relErr)
		}
		return nil, fmt.Errorf("sdk: failed to create app instance: %w", err)
	}

	return &Client{app: application, stdout: o.Stdout, stderr: o.Stderr}, nil
}

// Wrap adapts an already-constructed *app.App (e.g. one built by a
// host's own setup sequence, or by the CLI's setupApp) into a Client.
// Use this when the caller needs full control over app construction
// (custom cobra flags, a bespoke MCP-selection policy, etc.) and only
// wants sdk's typed Run/Close surface on top — as opposed to Open,
// which builds the App itself for the common embedding case.
//
// Wrap performs no setup of its own — no config load, no DB connect,
// no MCP startup: the App is taken over exactly as handed over, and
// Close on the returned Client calls the App's Shutdown.
func Wrap(a *app.App, stdout, stderr io.Writer) *Client {
	return &Client{app: a, stdout: stdout, stderr: stderr}
}

// Run executes one non-interactive agent turn and returns the typed
// result envelope. It is a thin pass-through to app.App.ExecuteRun: when
// req.Stdout or req.Stderr is nil, the Options-level default from Open is
// substituted; when that is nil too, output is discarded. For RunModeJSON
// the caller encodes the returned *RunResult itself (the envelope's JSON
// shape is stable).
func (c *Client) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.stdout
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.stderr
	}
	return c.app.ExecuteRun(ctx, req)
}

// RunNonInteractive is the render-inclusive counterpart to Run: it has
// the same signature and behaviour as app.App.RunNonInteractive (JSON
// envelope encoded into output for RunModeJSON, run-incomplete error
// mapping for the process exit code) and is a thin pass-through to it.
// The CLI's `rush run` goes through this method via Wrap, so the binary
// and the library share one code path by construction.
func (c *Client) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	return c.app.RunNonInteractive(ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
}

// SubscribeMessages streams every message create/update/delete across ALL
// sessions this Client's App knows about — the same raw event stream
// internal/server's WS layer consumes before its own reshaping for the
// browser. ExecuteRun/RunNonInteractive publish through this same broker
// regardless of who is subscribed, so open the subscription before or
// during a Run and events arrive independently of the call that produced
// them. Filter by ev.Payload.SessionID if you only care about one
// session's output. The returned channel is closed when ctx is done (see
// pubsub.Broker.Subscribe).
func (c *Client) SubscribeMessages(ctx context.Context) <-chan MessageEvent {
	return c.app.Messages.Subscribe(ctx)
}

// SubscribeSessions is SubscribeMessages' session-lifecycle counterpart
// (created/updated/deleted); same pass-through semantics and the same raw
// session.Session payload, with no wire reshaping.
func (c *Client) SubscribeSessions(ctx context.Context) <-chan SessionEvent {
	return c.app.Sessions.Subscribe(ctx)
}

// Close shuts the client down (agent cancellation, run-queue pump stop,
// bounded cleanup, DB release) and returns a CloseResult describing how
// it went: CloseResult.Forced is true when agents were still busy after
// the grace period, in which case the database was deliberately NOT
// released (see app.ShutdownResult), and CloseResult.CleanupErrors
// carries cleanup failures (also logged).
//
// Close is idempotent: the underlying App shutdown runs at most once per
// Client — the first call's result is cached and returned unchanged by
// every later call, so a double defer or a defensive second Close never
// re-runs cleanup or re-releases database references. A nil receiver or
// a Client without an App returns the zero CloseResult and does nothing.
func (c *Client) Close() CloseResult {
	if c == nil || c.app == nil {
		return CloseResult{}
	}
	c.closeOnce.Do(func() {
		c.closeResult = c.app.ShutdownWithResult()
	})
	return c.closeResult
}
