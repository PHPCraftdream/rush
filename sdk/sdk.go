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
//   - Application-mode MCP ownership is two-tier. The first
//     application-mode Open in a process takes the process-wide MCP Owner
//     exactly as before. A second application-mode Open reserves a
//     standalone Owner (mcp.AcquireStandalone) instead of failing with
//     mcp.ErrOwnerBusy, so two application-mode Clients coexist, each
//     routing its MCP operations through its own Owner. The underlying
//     registry is still name-keyed process state: two Clients configuring
//     the same server name share one registry entry, so disjoint server
//     configs remain the contract — a server configured in the first
//     Client is never visible to the second's surfaces. Library
//     mode (Options.Mode == ModeLibrary) starts no MCP servers and does
//     not acquire an owner, so it coexists with any application-mode
//     Client; multiple simultaneous library-mode Clients are supported and
//     tested (each ephemeral client gets its own isolated in-memory
//     database).
//
//   - Core logging is redirected only if you ask for it. With
//     SetupLogging false (the default) Open does not hijack the host's
//     slog.Default(), but the rush core still logs through package-level
//     slog.Info/Warn/Error in many places, so internal log lines go to
//     whatever the host's current slog.Default() is. See the SetupLogging
//     field's doc for the full caveat.
package sdk

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/message"
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

// Disk-provider aliases (RunOverrides.DiskProvider): aliases onto the
// canonical types in internal/agent/tools that the fs_* tools consume, so
// a host's implementation is directly assignable with zero conversion
// code — the same aliasing pattern as FolderScope above.
type (
	// DiskProvider is the borrowed filesystem capability for this run's
	// fs_* tools. Rush does not close it; keep it usable until associated
	// executions, including detached/held work, terminate and dispose.
	// Concurrent calls may overlap. Cancellation requests are not proof of
	// provider-operation termination. Process restart never resumes old work;
	// supply a provider explicitly to a new SDK run. Missing bindings fail
	// with ErrDiskProviderUnavailable, never OS-disk fallback.
	DiskProvider = tools.DiskProvider

	// DiskListRequest/DiskListResult are List's request/response shapes.
	DiskListRequest = tools.ListRequest
	DiskListResult  = tools.ListResult
	// DiskFindRequest/DiskFindResult are Find's request/response shapes.
	DiskFindRequest = tools.FindRequest
	DiskFindResult  = tools.FindResult
	// DiskSearchRequest/DiskSearchResult/DiskSearchLine are Search's
	// request/response shapes. Named DiskSearchResult (not the bare
	// SearchResult FolderScope-style naming might suggest) because
	// internal/agent/tools already declares an unrelated SearchResult
	// type for the legacy web-search tool.
	DiskSearchRequest = tools.SearchRequest
	DiskSearchResult  = tools.DiskSearchResult
	DiskSearchLine    = tools.SearchLine
)

// OSDiskProvider returns the real-filesystem DiskProvider — the default a
// run gets when RunOverrides.DiskProvider is nil. Exposed so a host that
// wants "real disk, but log every write" (or similar decoration) can wrap
// it instead of reimplementing ripgrep dispatch and gitignore-aware
// directory walking from scratch.
func OSDiskProvider() DiskProvider { return tools.OSDisk() }

// ErrClientClosed is returned by Client.Run, Client.RunWithCredentials,
// Client.Messages, and Client.Session once Close has started on the
// Client — either already finished, or still draining the calls it
// admitted before closing began (see Close for the exact ordering). The
// Subscribe methods return no error; on a closed Client they return an
// already-closed channel instead (see their docs).
var ErrClientClosed = errors.New("sdk: client is closed")

// ErrSessionBusy and ErrDiskProviderNotDurable are stable re-exports of the
// sentinel errors Run/RunWithCredentials wrap. Aliased because
// internal/agent is not importable by external consumers. The third sentinel
// of the trio, ErrDiskProviderUnavailable, lives in sentinels.go.
var (
	ErrSessionBusy            = agent.ErrSessionBusy
	ErrDiskProviderNotDurable = agent.ErrDiskProviderNotDurable
)

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
	// MCP selects which MCP servers application-mode Open starts. Default
	// MCPEnabledInCLI. There is no off value in application mode: use
	// ModeLibrary when the client must skip MCP entirely. Library mode passes
	// app.SkipMCP, so it starts no MCP servers and does not acquire an MCP
	// owner. Application mode takes the process-wide MCP owner for the first
	// client in the process; a second application-mode Open reserves a
	// standalone owner (see mcp.AcquireStandalone) so the two clients keep
	// independent MCP registries — keep each client's server names disjoint.
	MCP MCPMode
	// Stdout is the default destination for run output when a RunRequest
	// does not carry its own Stdout. The SDK serializes writes through all
	// Options-level default writers belonging to one Client, so a writer
	// such as bytes.Buffer is safe when concurrent Run and
	// RunWithCredentials calls use these defaults. Writes from different
	// runs may still be interleaved. When neither is set, ExecuteRun
	// discards output. Set it once here to cover every subsequent
	// Client.Run.
	Stdout io.Writer
	// Stderr is the default destination for run diagnostics (tool-call
	// heartbeat, progress, guidance) when a RunRequest does not carry its
	// own Stderr. It shares the Client's serialization with the Options-
	// level Stdout default, so concurrent calls cannot write to either
	// default concurrently. Writes from different runs may still be
	// interleaved.
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
// a Client without an App returns the zero CloseResult and transitions to
// the closed admission state, so its later public methods remain safe.
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
		c.beginShutdown()
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

// defaultWriter protects one Options-level writer for the duration of each
// Write call. The same Client mutex is shared by stdout and stderr defaults,
// including when both fields refer to the same underlying writer. This
// guarantees writer safety without promising run-level output grouping or
// ordering.
func (c *Client) defaultWriter(writer io.Writer) io.Writer {
	return &synchronizedWriter{mu: &c.outputMu, writer: writer}
}

type synchronizedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
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
