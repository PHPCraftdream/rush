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
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
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
	// Per-call credentials (RunWithCredentials): aliases onto the
	// canonical types in internal/agent — the coordinator consumes them
	// directly, so no conversion layer exists anywhere.
	CredentialSet   = agent.CredentialSet
	Credential      = agent.Credential
	CredentialModel = agent.CredentialModel
	ModelChoice     = agent.ModelChoice
	ProviderType    = agent.ProviderType
	Role            = agent.Role
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

// Role names the model slot a ModelChoice fills in a CredentialSet.
// Values mirror config.SelectedModelType* literally, so casting between
// sdk.Role and config.SelectedModelType needs no conversion.
const (
	RoleSmart    = agent.RoleSmart
	RoleFast     = agent.RoleFast
	RoleWorker   = agent.RoleWorker
	RoleReviewer = agent.RoleReviewer
)

// ProviderType values for Credential.Type, mirroring catwalk.Type
// (charm.land/catwalk) one-to-one: a fixed closed enum, not a free-form
// string.
const (
	ProviderTypeOpenAI       = agent.ProviderTypeOpenAI
	ProviderTypeOpenAICompat = agent.ProviderTypeOpenAICompat
	ProviderTypeOpenRouter   = agent.ProviderTypeOpenRouter
	ProviderTypeAnthropic    = agent.ProviderTypeAnthropic
	ProviderTypeGoogle       = agent.ProviderTypeGoogle
	ProviderTypeGoogleVertex = agent.ProviderTypeGoogleVertex
	ProviderTypeAzure        = agent.ProviderTypeAzure
	ProviderTypeBedrock      = agent.ProviderTypeBedrock
	ProviderTypeVercel       = agent.ProviderTypeVercel
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
	// Mode selects how Open resolves configuration and persistence. The
	// zero value (ModeApplication) is today's exact behavior: WorkingDir
	// is required and rush.json/.mcp.json/global config are
	// auto-discovered from disk. Existing callers who never set Mode are
	// completely unaffected.
	Mode OpenMode
	// LibraryConfig is the fully-explicit, zero-disk-configuration used
	// when Mode == ModeLibrary: every provider and every model role comes
	// from here instead of any config file. Ignored (may be nil) in
	// application mode. See OpenMode and LibraryConfig.
	LibraryConfig *LibraryConfig
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

	// Conns to close after the App shutdown on Close; only set for
	// library-mode ephemeral in-memory clients, whose *sql.DB is not
	// owned by the db pool.
	closeConns []*sql.DB
}

// Open wires up a full Rush instance for the given working directory:
// config load (project rush.json discovery from WorkingDir), data
// directory creation, project registration, database connect plus
// migrations, and app construction with the requested MCP mode. It is the
// library equivalent of internal/cmd's setupApp, minus os.Chdir,
// unconditional logging setup, and cobra.
func Open(ctx context.Context, o Options) (*Client, error) {
	if o.Mode == ModeLibrary {
		return openLibrary(ctx, o)
	}
	return openApplication(ctx, o)
}

// resolveWorkingDir validates and absolutizes the raw working directory
// path the caller handed to Options.WorkingDir.
func resolveWorkingDir(raw string) (string, error) {
	workDir, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("sdk: failed to resolve working directory %q: %w", raw, err)
	}
	if info, statErr := os.Stat(workDir); statErr != nil {
		if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("sdk: working directory %q is not accessible: %w", workDir, statErr)
		}
		// WorkingDir stays a required parameter -- this is not a default
		// path, just tolerance for a fresh workspace the host hasn't
		// created yet (e.g. a freshly-provisioned per-tenant directory).
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return "", fmt.Errorf("sdk: failed to create working directory %q: %w", workDir, err)
		}
	} else if !info.IsDir() {
		return "", fmt.Errorf("sdk: working directory %q is not a directory", workDir)
	}
	return workDir, nil
}

// openApplication is Open's application-mode path: the exact setup
// sequence the CLI performs, from config discovery down to app
// construction.
func openApplication(ctx context.Context, o Options) (*Client, error) {
	if o.WorkingDir == "" {
		return nil, fmt.Errorf("sdk: Options.WorkingDir is required")
	}
	workDir, err := resolveWorkingDir(o.WorkingDir)
	if err != nil {
		return nil, err
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
//
// Unlike `rush run` and the web server (which queue a second message
// behind a running turn), Run sets req.FailIfSessionBusy: a concurrent
// Run/RunWithCredentials on the SAME ContinueSessionID fails immediately
// with an error wrapping agent.ErrSessionBusy. Concurrent runs on
// DIFFERENT sessions are unaffected.
func (c *Client) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.stdout
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.stderr
	}
	// SDK semantics differ from `rush run`/web: fail fast on an
	// in-process busy session instead of queueing behind it.
	req.FailIfSessionBusy = true
	return c.app.ExecuteRun(ctx, req)
}

// RunWithCredentials is Run's per-tenant counterpart for concurrent
// multi-tenant use: ONE Client may have several RunWithCredentials
// calls in flight at once, each with its own creds and its own
// req.ContinueSessionID, and each turn is built and served entirely
// from that call's CredentialSet — fresh provider clients per call,
// never cached, and nothing is read from (or merged with) rush.json
// providers, environment credentials, or any other call's state.
//
// creds replaces model+provider resolution for every role it covers:
// smart drives the turn, fast drives title generation, worker drives
// sub-agent spawns made BY THIS CALL; reviewer is accepted but, like
// the config slot of the same name, has no live runtime consumer yet.
// Roles absent from creds.Models fall back to the ordinary resolution
// path. The API key is used literally — OAuth/token-refresh providers
// are out of scope. ModelChoice.Model is deliberately NOT validated
// against Credential.Models: an unknown id fails on the first real
// provider call, exactly like `--model` today.
//
// Sessions and credentials are independent axes: req.ContinueSessionID
// keeps its normal get-or-create semantics (the session lives in the
// Client's shared data directory), while creds only decides WHICH
// provider serves this call. Like Run, it sets req.FailIfSessionBusy: a
// second concurrent call on the SAME ContinueSessionID fails fast with
// an error wrapping agent.ErrSessionBusy instead of queueing; different
// sessions run concurrently.
func (c *Client) RunWithCredentials(ctx context.Context, req RunRequest, creds CredentialSet) (*RunResult, error) {
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.stdout
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.stderr
	}
	req.Credentials = &creds
	// Same fail-fast busy semantics as Run (see its doc).
	req.FailIfSessionBusy = true
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
		for _, conn := range c.closeConns {
			if err := conn.Close(); err != nil {
				slog.Error("sdk: failed to close in-memory database connection", "error", err)
			}
		}
	})
	return c.closeResult
}
