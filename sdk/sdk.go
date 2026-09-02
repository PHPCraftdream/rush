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
//   - One application-mode Client per process. MCP client state
//     (internal/agent/tools/mcp) is process-wide package state — one
//     registry keyed by server name, plus process-wide
//     initialization-complete signaling — so two simultaneous
//     application-mode Clients would share one MCP layer instead of each
//     owning one. Library mode (Options.Mode == ModeLibrary) starts no
//     MCP servers at all, and multiple simultaneous library-mode Clients
//     are supported and tested (each ephemeral client gets its own
//     isolated in-memory database). Run one process per workspace for
//     application mode — the same model `rush run` already uses, with
//     lock-file + heartbeat, the battle-tested path in the sessions_*
//     CLI family.
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
	"errors"
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
	"github.com/PHPCraftdream/rush/internal/permission"
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

// Folder-scope aliases (RunOverrides.FolderScopes): aliases onto the
// canonical types in internal/permission that permission.BuildFolderScope
// compiles and the fs_* tools consume, so a host's scope spec is directly
// assignable with zero conversion code — the same aliasing pattern as
// CredentialSet above.
type (
	// FolderScope is one scope entry: a directory subtree plus the
	// operations granted inside it. An entry with no Ops is a deny
	// carve-out that excludes that subtree from every enclosing grant.
	// See permission.FolderScopeEntry.
	FolderScope = permission.FolderScopeEntry
	// FileOp is one filesystem operation a folder scope can grant.
	FileOp = permission.FileOp
)

// FileOp values for FolderScope.Ops, mirroring permission.FileOp*
// one-to-one.
const (
	FileOpList       = permission.FileOpList
	FileOpFind       = permission.FileOpFind
	FileOpGrep       = permission.FileOpGrep
	FileOpRead       = permission.FileOpRead
	FileOpCreate     = permission.FileOpCreate
	FileOpOverwrite  = permission.FileOpOverwrite
	FileOpWriteLines = permission.FileOpWriteLines
	FileOpReplace    = permission.FileOpReplace
	FileOpDelete     = permission.FileOpDelete
)

// ErrClientClosed is returned by Client.Run, Client.RunWithCredentials,
// Client.Messages, and Client.Session once Close has started on the
// Client — either already finished, or still draining the calls it
// admitted before closing began (see Close for the exact ordering). The
// Subscribe methods return no error; on a closed Client they return an
// already-closed channel instead (see their docs).
var ErrClientClosed = errors.New("sdk: client is closed")

// Structured-event aliases onto the same raw types the web UI's WS layer
// consumes through app.App's Sessions/Messages brokers (see
// internal/server/events.go). These are pass-through aliases, deliberately
// not a new wire contract: message.Message and session.Session carry no
// JSON tags on purpose — internal/server re-shapes them into its own
// browser-specific wire structs, while sdk hands consumers the raw Go
// values so each consumer serialises them for its own API as it sees fit.
//
// Message and Session are NOT a stable wire contract across Rush
// versions — unlike RunRequest/RunResult, their field set may change
// without a semver-major bump; do not persist their JSON encoding as a
// durable format.
type (
	Message = message.Message
	Session = session.Session
	// Origin is the entry-channel marker (message.Origin) attached to
	// sessions and individual user messages; see the Origin* constants
	// below.
	Origin       = message.Origin
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

// Origin values mark where a session or an individual user message
// entered the system (session origin + per-message origin). Mirrors
// message.Origin* one-to-one.
const (
	OriginCLI = message.OriginCLI
	OriginWeb = message.OriginWeb
	OriginSDK = message.OriginSDK
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
	// completely unaffected. Any value other than ModeApplication or
	// ModeLibrary makes Open return an error.
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

	// Admission state, shared by every public method and Close (review
	// round-2 finding R2-2): a bare closed flag checked on entry only
	// guarded calls that STARTED after Close returned, leaving the
	// check-then-act race open where a call reads closed==false, is
	// descheduled, and then enters c.app against an App that Close has
	// already torn down. admissionMu guards the fields below as one
	// state machine: admit either registers a call as in-flight before
	// closing starts or rejects it; Close flips closing, waits for
	// inflight to drain (abandoning that wait when the shutdown goes
	// forced — see Close), and flips closed only once the App shutdown
	// has fully returned. See admit, release, and beginShutdown.
	admissionMu sync.Mutex
	// closing is set once by Close and never unset; from that instant
	// every new admission is refused (ErrClientClosed, or an
	// already-closed channel for the Subscribe methods).
	closing bool
	// inflight counts admitted calls that have not returned yet. The
	// Subscribe methods hold a count only for the duration of the call
	// itself, never for the subscription's lifetime (a subscription is
	// bound to the caller's ctx, which outlives the call by design).
	inflight int
	// drained is created when closing flips to true and is closed once
	// inflight reaches zero — Close's signal that the last admitted
	// call has returned and teardown may begin.
	drained chan struct{}

	// closed is set once by Close after the wrapped App's shutdown has
	// fully returned — not when Close starts. CloseEphemeralConnsForced
	// keys its ordering guard on this rather than on closing: between
	// the two moments Close may still be draining admitted calls that
	// are executing against the in-memory handles, and a reclaim inside
	// that window would close the database under them (review round-4
	// finding F5).
	closed bool

	// connsMu guards connsClosed, the once-only bookkeeping for
	// closeConns: Close's graceful path and CloseEphemeralConnsForced
	// can reach the close from different goroutines.
	connsMu sync.Mutex
	// connsClosed records whether closeConns has already been closed.
	connsClosed bool

	// Conns to close after a GRACEFUL App shutdown on Close; only set
	// for library-mode ephemeral in-memory clients, whose *sql.DB is
	// not owned by the db pool. On a FORCED shutdown they are left
	// open (live writers) until CloseEphemeralConnsForced.
	closeConns []*sql.DB
}

// Open wires up a full Rush instance for the given working directory:
// config load (project rush.json discovery from WorkingDir), data
// directory creation, project registration, database connect plus
// migrations, and app construction with the requested MCP mode. It is the
// library equivalent of internal/cmd's setupApp, minus os.Chdir,
// unconditional logging setup, and cobra.
func Open(ctx context.Context, o Options) (*Client, error) {
	switch o.Mode {
	case ModeApplication:
		return openApplication(ctx, o)
	case ModeLibrary:
		return openLibrary(ctx, o)
	default:
		return nil, fmt.Errorf("sdk: unknown Options.Mode %d (want ModeApplication or ModeLibrary)", int(o.Mode))
	}
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
//
// Trust model: Run performs no ownership or authorization check on
// req.ContinueSessionID — any caller who knows the id continues that
// session with whatever credentials the process is configured with.
// Generating opaque session ids and mapping them to your own callers
// is the host's job. See the README's trust-model section.
//
// Returns ErrClientClosed once Close has started. An admitted Run gets
// one grace period against a fully live App, but Close no longer
// guarantees to wait for it before anything is released: on a forced
// shutdown (work still busy after the grace period — an admitted run
// included) cleanup proceeds around calls still in flight, and only the
// database and the in-memory handles are held back. Close's doc is the
// authoritative description of this graceful-vs-forced split.
func (c *Client) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if !c.admit() {
		return nil, ErrClientClosed
	}
	defer c.release()
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.stdout
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.stderr
	}
	// SDK semantics differ from `rush run`/web: fail fast on an
	// in-process busy session instead of queueing behind it.
	req.FailIfSessionBusy = true
	// SDK turns are SDK-origin by default; an explicit origin set by the
	// caller on the request is respected.
	if req.Origin == message.OriginUnspecified {
		req.Origin = OriginSDK
	}
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
// Strict isolation is the default: the smart role is required in
// creds.Models, and a smart/fast role the set does not cover is a
// hard error before any provider traffic. Set
// creds.AllowConfiguredRoleFallback to serve uncovered roles from the
// Client's configured providers instead — a deliberate crossing of
// the tenant-credential boundary (see agent.CredentialSet). The API
// key is used literally — OAuth/token-refresh providers
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
//
// Like Run, no ownership or authorization check is performed on
// req.ContinueSessionID — see the README's trust-model section.
//
// Returns ErrClientClosed once Close has started. An admitted call gets
// one grace period against a fully live App, but Close no longer
// guarantees to wait for it before anything is released: on a forced
// shutdown (work still busy after the grace period — an admitted call
// included) cleanup proceeds around calls still in flight, and only the
// database and the in-memory handles are held back. Close's doc is the
// authoritative description of this graceful-vs-forced split.
func (c *Client) RunWithCredentials(ctx context.Context, req RunRequest, creds CredentialSet) (*RunResult, error) {
	if !c.admit() {
		return nil, ErrClientClosed
	}
	defer c.release()
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.stdout
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.stderr
	}
	req.Credentials = &creds
	// Same fail-fast busy semantics as Run (see its doc).
	req.FailIfSessionBusy = true
	if req.Origin == message.OriginUnspecified {
		req.Origin = OriginSDK
	}
	return c.app.ExecuteRun(ctx, req)
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
//
// No tenant filtering: events for every session reach every
// subscriber; filter by ev.Payload.SessionID in the host (see the
// README's trust-model section).
//
// Admission counts only the Subscribe call itself, never the
// subscription's lifetime: a subscription admitted before Close started
// stays bound to the caller's ctx (it simply stops receiving events once
// the App has shut down), while a call that races Close either completes
// against the live broker or returns an already-closed channel.
//
// On a closed Client the returned channel is already closed.
func (c *Client) SubscribeMessages(ctx context.Context) <-chan MessageEvent {
	if !c.admit() {
		closedCh := make(chan MessageEvent)
		close(closedCh)
		return closedCh
	}
	defer c.release()
	return c.app.Messages.Subscribe(ctx)
}

// SubscribeSessions is SubscribeMessages' session-lifecycle counterpart
// (created/updated/deleted); same pass-through semantics and the same raw
// session.Session payload, with no wire reshaping.
//
// No tenant filtering either: filter by ev.Payload.SessionID in the
// host (see the README's trust-model section).
//
// Admission counts only the Subscribe call itself, never the
// subscription's lifetime — see SubscribeMessages.
//
// On a closed Client the returned channel is already closed.
func (c *Client) SubscribeSessions(ctx context.Context) <-chan SessionEvent {
	if !c.admit() {
		closedCh := make(chan SessionEvent)
		close(closedCh)
		return closedCh
	}
	defer c.release()
	return c.app.Sessions.Subscribe(ctx)
}

// Messages returns the full message history of sessionID, in
// chronological order -- every role (user/assistant/tool/system).
// This is the only way to retrieve history after a Run/RunWithCredentials
// call has already returned if SubscribeMessages was not used before
// that call started (the subscription channel carries no backlog).
//
// For an ephemeral in-memory session (Options.Mode == ModeLibrary with
// no WorkingDir, see LibraryConfig), this is ALSO the only way to see
// history at all once Run returns -- and it becomes permanently
// unavailable the moment Close is called: nothing survives on disk.
//
// No ownership check is performed on sessionID: any caller who knows
// the id gets the full history (see the README's trust-model
// section).
//
// Returns ErrClientClosed once Close has started; an admitted call is
// served from the live store while Close is still draining it. But
// Close no longer guarantees to wait for every admitted call before
// anything is released: on a forced shutdown (work still busy after
// the grace period) cleanup proceeds around calls still in flight, and
// only the database and the in-memory handles are held back. Close's
// doc is the authoritative description of this graceful-vs-forced
// split.
func (c *Client) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	if !c.admit() {
		return nil, ErrClientClosed
	}
	defer c.release()
	return c.app.Messages.List(ctx, sessionID)
}

// Session returns sessionID's current metadata (title, token/cost
// counters, etc.) -- not its message history, see Messages for that.
//
// No ownership check is performed on sessionID either (see the
// README's trust-model section).
//
// Returns ErrClientClosed once Close has started; an admitted call is
// served from the live store while Close is still draining it. But
// Close no longer guarantees to wait for every admitted call before
// anything is released: on a forced shutdown (work still busy after
// the grace period) cleanup proceeds around calls still in flight, and
// only the database and the in-memory handles are held back. Close's
// doc is the authoritative description of this graceful-vs-forced
// split.
func (c *Client) Session(ctx context.Context, sessionID string) (Session, error) {
	if !c.admit() {
		return Session{}, ErrClientClosed
	}
	defer c.release()
	return c.app.Sessions.Get(ctx, sessionID)
}

// Close shuts the client down (agent cancellation, run-queue pump stop,
// bounded cleanup, DB release) and returns a CloseResult describing how
// it went: CloseResult.Forced is true when agent work or the run-queue
// pump was still busy after the grace period, in which case the
// database — and, on an
// ephemeral client, the in-memory handles — were deliberately NOT
// released (see app.ShutdownResult), and CloseResult.CleanupErrors
// carries cleanup failures (also logged).
//
// Close is idempotent: the underlying App shutdown runs at most once per
// Client — the first call's result is cached and returned unchanged by
// every later call, so a double defer or a defensive second Close never
// re-runs cleanup or re-releases database references. A nil receiver or
// a Client without an App returns the zero CloseResult and does nothing.
//
// Ephemeral in-memory clients (Options.Mode == ModeLibrary with no
// WorkingDir) follow the same policy as app.ShutdownWithResult: on a
// graceful Close the in-memory handles are closed here; on a forced
// Close they are deliberately LEFT OPEN, because closing them would pull
// the database out from under still-live writers — the exact hazard the
// forced-shutdown policy exists to avoid. The trade-off: an in-memory
// database whose handles stay open stays pinned (memory held, session
// data intact) — there is no OS to reclaim the handles at process exit
// inside a long-lived host process. Release them deliberately with
// CloseEphemeralConnsForced once every writer has finished.
//
// Close runs in three ordered phases:
//
// Phase 1 — admission closes: from the instant Close is called, new
// Run/RunWithCredentials/Messages/Session calls fail with ErrClientClosed
// and the Subscribe methods return an already-closed channel.
//
// Phase 2 — drain, cancelling on stall: the calls admitted before that
// instant get one grace period (agent.DefaultCancelAllGrace, the same
// value the coordinator's cancellation enforces) to finish against the
// fully live App — a call that finishes inside it is neither cancelled
// nor exposed to any released resource. Work still running when the
// grace period expires is cancelled immediately, while every resource is
// still open: a run stuck on a non-cancellable provider or tool call
// unwinds here instead of blocking Close forever (review round-3, R3-2).
// After cancellation, Close waits for agent work to unwind — bounded by
// the coordinator's own grace-bounded join — and then for the remaining
// admitted calls; work that ignores cancellation makes the shutdown
// forced and the drain is abandoned rather than waited on. "Work" here
// is broader than the admitted calls: even a drain that finishes
// cooperatively still runs one round of cancellation to join background
// agent work (session title generation, cache keep-alive replays) that
// no admitted call was blocked on — such work ignoring cancellation
// makes an otherwise fully drained Close forced — and a run-queue pump
// worker still busy after its own grace forces it with no agent work
// involved at all. The residual wait after agent work has fully
// unwound is unbounded, because
// cancellation cannot reach non-agent calls (a Messages read against the
// store): a host that needs a total bound should cancel the contexts it
// handed to its own in-flight calls.
//
// Phase 3 — release: only once the drain completed or was force-abandoned
// are resources released, under the same graceful-vs-forced policy as
// app.ShutdownWithResult: bounded parallel cleanup always; the database
// and the in-memory handles only on the graceful path.
//
// Do not call Close from within an in-flight call on the same Client —
// the drain, and the residual wait after a stall, waits for the very
// call Close is blocking.
//
// Once Close has started, Run, RunWithCredentials, Messages, and Session
// return ErrClientClosed and the Subscribe methods return an
// already-closed channel; Close itself remains idempotent.
func (c *Client) Close() CloseResult {
	if c == nil {
		return CloseResult{}
	}
	if c.app == nil {
		// Nothing to shut down, but record completion anyway so
		// CloseEphemeralConnsForced's ordering guard does not
		// report "before Close has finished" after Close was
		// called — such a client has no in-memory handles to
		// protect either way.
		c.admissionMu.Lock()
		c.closed = true
		c.admissionMu.Unlock()
		return CloseResult{}
	}
	// Phase 1: reject every new admission from this instant on.
	drained := c.beginShutdown()
	// Phases 2 and 3: drain with cancellation on stall, then release —
	// still at most once per Client (closeOnce), with the first result
	// cached for repeat callers. ShutdownAfterDrain keeps the App fully
	// live until the drain has had its grace period: cancellation fires
	// BEFORE any resource is released (R3-2). Release then happens once
	// the drain completed or was force-abandoned: a call that finishes
	// inside its grace never touches a torn-down App (the round-2
	// check-then-act guarantee), while on the forced path cleanup runs
	// around calls still unwinding, with only the database and
	// in-memory handles held back.
	c.closeOnce.Do(func() {
		c.closeResult = c.app.ShutdownAfterDrain(drained)
		// The App shutdown has returned: only now can no admitted
		// call still be executing against the in-memory handles,
		// so this — not the start of Close — is the moment
		// CloseEphemeralConnsForced's ordering guard may pass (F5).
		c.admissionMu.Lock()
		c.closed = true
		c.admissionMu.Unlock()
		if !c.closeResult.Forced {
			// Graceful shutdown: no live writers remain, the
			// in-memory handles can be released immediately.
			_ = c.closeEphemeralConns()
		}
		// Forced shutdown: leave the in-memory handles open (live
		// writers may still be active); CloseEphemeralConnsForced
		// releases them once the host knows the writers are done.
	})
	return c.closeResult
}

// admit atomically admits one method call: it either registers the call
// as in-flight before closing has started (true) or rejects it because
// Close has begun (false). The check and the registration share one
// critical section, so a call can never be counted as admitted and then
// enter c.app against an App that Close is tearing down.
func (c *Client) admit() bool {
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if c.closing {
		return false
	}
	c.inflight++
	return true
}

// release drops one admission made by admit. It runs at most once per
// successful admit, so inflight never goes negative; when it drops
// inflight to zero after Close has started closing, it signals Close
// that the last admitted call has returned.
func (c *Client) release() {
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	c.inflight--
	if c.closing && c.inflight == 0 {
		close(c.drained)
	}
}

// beginShutdown flips the Client into the closing state — rejecting
// every new admission from this instant — and returns the channel that
// is closed once no admitted call remains in flight. Idempotent: repeat
// and concurrent calls observe the state the first call created.
func (c *Client) beginShutdown() <-chan struct{} {
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if !c.closing {
		c.closing = true
		c.drained = make(chan struct{})
		if c.inflight == 0 {
			close(c.drained)
		}
	}
	return c.drained
}

// CloseEphemeralConnsForced force-closes the in-memory database handles
// of a library-mode ephemeral client (Options.Mode == ModeLibrary with
// no WorkingDir). It is the recovery step for a Close that returned a
// CloseResult with Forced=true: Close leaves the handles open so the
// database survives under still-live writers, and the host calls this
// once it KNOWS every writer has finished (e.g. all in-flight
// Run/RunWithCredentials goroutines have returned). Calling it while a
// writer is still active closes the database under that writer — the
// caller owns that judgement; database/sql's Close additionally blocks
// until queries already in flight on the pool complete.
//
// Order matters: until Close has FINISHED, the method refuses to run
// and returns an error instead. The guard keys on Close's completion,
// not on its start: between the two, Close may still be draining calls
// it admitted, and those calls are executing against the in-memory
// handles — reclaiming inside that window would pull the database out
// from under exactly them, the hazard this guard exists to prevent
// (F5). No legitimate caller is blocked by this: a host cannot know
// Close's Forced verdict before Close returns, which is the only state
// this method is documented to recover from. Before Close is called at
// all the refusal has its own reason: closing the handles would leave a
// still-open Client whose Run/Messages/Session calls are admitted and
// then fail with database/sql's "sql: database is closed" instead of
// ErrClientClosed — and a host finished with an idle client should call
// Close, which releases the handles itself on the graceful path. After
// a graceful Close it is a no-op (Close already closed the handles);
// repeated calls are no-ops. It returns the first handle-close error,
// if any (every error is also logged). Clients without in-memory
// handles (application mode) get a no-op and a nil error once Close has
// finished.
func (c *Client) CloseEphemeralConnsForced() error {
	if c == nil {
		return nil
	}
	c.admissionMu.Lock()
	closed := c.closed
	c.admissionMu.Unlock()
	if !closed {
		return fmt.Errorf("sdk: CloseEphemeralConnsForced before Close has finished: call Close and let it return first — until then the Client is either still open and admitting calls, or Close is still draining calls it already admitted, and reclaiming now would close the in-memory database handles out from under calls executing against them")
	}
	return c.closeEphemeralConns()
}

// closeEphemeralConns closes the in-memory handles exactly once, however
// many callers race it.
func (c *Client) closeEphemeralConns() error {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	if c.connsClosed {
		return nil
	}
	c.connsClosed = true
	var firstErr error
	for _, conn := range c.closeConns {
		if err := conn.Close(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Error("sdk: failed to close in-memory database connection", "error", err)
		}
	}
	return firstErr
}
