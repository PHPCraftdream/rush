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

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/app/projects"
	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Client is a handle to one embedded Rush instance rooted at
// Options.WorkingDir.
type Client struct {
	app    *app.App
	stdout io.Writer
	stderr io.Writer
	// outputMu serializes writes made through the Options-level stdout and
	// stderr defaults. Request-level writers are deliberately not routed
	// through this mutex because they remain caller-owned.
	outputMu sync.Mutex

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
// migrations, and app construction with the requested MCP mode. In
// application mode, Open also acquires an MCP owner: the first client in
// the process takes the process-wide owner, and a second application-mode
// Open reserves a standalone owner instead of failing, so the two clients
// coexist with independent MCP registries (disjoint server configs are the
// contract). In library mode, Open skips MCP and does not acquire an
// owner. Open is the library equivalent of internal/cmd's setupApp, minus
// os.Chdir, unconditional logging setup, and cobra.
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

	store, err := config.Init(workDir, o.DataDir, o.Debug, config.WithCLIModels(cliprovider.AvailableCLIModels))
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
		rushlog.ProcessStart("sdk", "")
	}

	// Register the project so `rush projects` / `rush dirs` see embedded
	// sessions the same way they see CLI ones. Non-fatal, exactly like
	// setupApp.
	if err := projects.Register(workDir, cfg.Options.DataDirectory); err != nil {
		slog.Warn("sdk: failed to register project", "error", err)
	}

	// Shared data directory (SD-D #1143): when the SDK host resolved the
	// linked worktree's shared <main>/.rush, pin the marker exactly like
	// the CLI's setupApp, and bar a dev build from migrating the shared DB.
	if store.DataDirSource() == config.DataDirSourceShared {
		if err := config.MarkSharedWorkspace(cfg.Options.DataDirectory, config.WorkspaceRoot(workDir)); err != nil {
			slog.Warn("sdk: failed to mark shared workspace", "error", err)
		}
	}
	migrateOpts := []db.MigrateOption{}
	if !store.MayMigrateData() {
		migrateOpts = append(migrateOpts, db.WithMayMigrate(false))
	}
	conn, err := db.Connect(ctx, cfg.Options.DataDirectory, migrateOpts...)
	if err != nil {
		return nil, fmt.Errorf("sdk: failed to connect database in %q: %w", cfg.Options.DataDirectory, err)
	}

	var mcpOpts []app.Option
	if o.MCP == MCPAll {
		mcpOpts = nil
	} else {
		mcpOpts = []app.Option{app.RestrictMCPToCLI()}
	}

	// Application mode owns an MCP lifecycle for this client. The first
	// client in the process takes the process-wide owner exactly as the CLI
	// does; ErrOwnerBusy means a live peer holds that slot, so this client
	// reserves a standalone owner instead and both run independent MCP
	// registries. Every MCP operation the App performs is routed through
	// this owner (app.WithMCPOwner), never through the package-level
	// current-owner resolution.
	mcpOwner, err := mcp.Acquire()
	if errors.Is(err, mcp.ErrOwnerBusy) {
		mcpOwner, err = mcp.AcquireStandalone()
	}
	if err != nil {
		if relErr := db.ReleaseConn(conn); relErr != nil {
			slog.Error("sdk: failed to release DB connection after MCP owner acquisition failure", "error", relErr)
		}
		return nil, fmt.Errorf("sdk: failed to acquire MCP owner: %w", err)
	}
	mcpOpts = append(mcpOpts, app.WithMCPOwner(mcpOwner))

	application, err := app.New(ctx, conn, store, mcpOpts...)
	if err != nil {
		slog.Error("sdk: failed to create app instance", "error", err)
		if closeErr := mcpOwner.Close(context.Background()); closeErr != nil {
			slog.Error("sdk: failed to close MCP owner after app init failure", "error", closeErr)
		}
		// Ownership split: app.New releases only the ConnectRead
		// reference it acquired internally on this error path; it never
		// took ownership of our conn, so this reference must be released
		// here or the writer pool leaks (on Windows, its file handle
		// with it). Mirrors setupApp in internal/cmd/root.go.
		if relErr := db.ReleaseConn(conn); relErr != nil {
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
// with an error wrapping ErrSessionBusy. Concurrent runs on
// DIFFERENT sessions are unaffected.
//
// Trust model: Run performs no ownership or authorization check on
// req.ContinueSessionID — any caller who knows the id continues that
// session with whatever credentials the process is configured with.
// Generating opaque session ids and mapping them to your own callers
// is the host's job. See the README's trust-model section.
//
// A non-nil req.Stdout or req.Stderr is passed through unchanged. If the
// caller shares such a request-level writer across concurrent calls, the
// caller is responsible for making it concurrency-safe. Options-level
// defaults have SDK-provided per-Write synchronization.
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
		req.Stdout = c.defaultWriter(c.stdout)
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.defaultWriter(c.stderr)
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
// A non-nil request-level Stdout or Stderr is passed through unchanged and
// remains caller-owned when shared across calls; Options-level defaults are
// synchronized by the Client.
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
// an error wrapping ErrSessionBusy instead of queueing; different
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
	creds = creds.Clone()
	if req.Stdout == nil && c.stdout != nil {
		req.Stdout = c.defaultWriter(c.stdout)
	}
	if req.Stderr == nil && c.stderr != nil {
		req.Stderr = c.defaultWriter(c.stderr)
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
// session's output. The returned channel is closed when ctx is done or when
// the App is shut down, including through Client.Close (see
// pubsub.Broker.Subscribe).
//
// No tenant filtering: events for every session reach every
// subscriber; filter by ev.Payload.SessionID in the host (see the
// README's trust-model section).
//
// Admission counts only the Subscribe call itself, never the
// subscription's lifetime: a subscription admitted before Close started
// stays bound to the caller's ctx and is closed when the App shuts down,
// while a call that races Close either completes
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
