// The non-interactive run path: run modes and per-invocation overrides,
// session resolution, the panic-isolated agent-turn wrapper, and the
// ExecuteRun event loop that computes a run and streams output; the thin
// RunNonInteractive wrapper renders the final envelope.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/format"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// cleanupTimeout bounds the best-effort DB writes in RunNonInteractive's
// post-run defers (A-2, task #779). Chosen as a small multiple of ordinary
// SQLite lock contention (a single writer holding the DB for a query or two,
// typically single-digit milliseconds) while staying far short of the 30s
// busy_timeout configured in internal/db/connect.go -- long enough that a
// momentarily busy DB still gets the write, short enough that a genuinely
// stuck writer can't reproduce the "looks like a hang after output already
// printed" symptom this fixes.
const cleanupTimeout = 2 * time.Second

// messageEventsClosedSeam is a test-only hook fired once, the first time
// RunNonInteractive's event loop observes messageEvents closed (H-2, task
// #779). nil in production. Lets tests assert the closed-channel branch was
// actually taken exactly once (not spun on) rather than only inferring it
// from wall-clock loop termination.
var messageEventsClosedSeam func()

// RunMode picks the output format for RunNonInteractive.
type RunMode int

const (
	// RunModeTerse: tool-call names on stderr, final assistant message on
	// stdout. Default — small output, friendly to wrapper scripts.
	RunModeTerse RunMode = iota
	// RunModeStream: every assistant token streams to stdout as it arrives.
	// Legacy behaviour; useful when a human is watching.
	RunModeStream
	// RunModeJSON: stdout gets exactly one JSON object summarising the run
	// (session id, final text, tool-call counts, token usage, duration,
	// exit reason). Tool-call heartbeat still goes to stderr so wrappers
	// can show progress without parsing JSON deltas.
	RunModeJSON
)

// RunOverrides bundles the optional per-invocation overrides for
// RunNonInteractive so the signature doesn't keep growing.
//
// Persistence: every non-empty field is written to the session BEFORE
// the agent runs, so a subsequent `rush run --session <same>` without
// those flags continues with the same overrides. Empty fields are
// left alone (they don't reset what's already on the session).
type RunOverrides struct {
	SmartModel   string // "model" or "provider/model"; overrides selected large
	FastModel    string // same as SmartModel, for the fast slot
	SystemPrompt string // persisted on the session (Sessions.UpdateSystemPrompt)
	// ReasoningEffort applies to whichever slot is "active" for this run —
	// the smart one if RoleSmart is true, the fast one otherwise. Persisted
	// via Sessions.UpdateReasoningEffort.
	ReasoningEffort string
	RoleSmart       bool
	// ModelRole is the resolved --role slot for this invocation (smart,
	// fast, worker, reviewer). "" (e.g. non-`rush run` paths) is treated
	// as smart by the coordinator. Threaded through to
	// AgentCoordinator.SetActiveModelRole so sub-agent spawns can decide
	// whether to prefer the cheaper Worker slot instead of blindly
	// inheriting the parent's Smart model. Fork patch (reviewer/worker
	// roles).
	ModelRole config.SelectedModelType
	// Fork patch (orchestrator UX): DisableSubAgents drops the `agent`
	// and `agentic_fetch` tools from the coder agent for this run so a
	// `rush run --agents single` invocation cannot fan out. Mutation
	// is per-process — `rush run` is single-shot, so the change does
	// not leak across invocations. StripJSONFences asks
	// RunNonInteractive to post-process the envelope's final_text
	// (markdown fence + prose preamble removal); the unstripped
	// original is preserved in RunResult.AssistantNotes.
	DisableSubAgents bool
	StripJSONFences  bool
	// AggregationMode controls how sub-agent fan-out output reaches
	// the orchestrator. "" / "summary" = upstream default (parent
	// composes a wrap-up, sub-agent details live in the DB only).
	// "concat" = the user prompt carries a nudge asking the parent to
	// include each sub-agent's reply verbatim in final_text. "attach"
	// = after Run the app collects each sub-session's last assistant
	// text into RunResult.SubAgentOutputs so the orchestrator gets
	// the structured set even if parent over-summarised.
	// See run_format.go and the 2026-05-17 session-#3 audit feedback.
	AggregationMode string
	// CheckpointInterval, when > 0, enables mid-stream auto-checkpointing
	// of the in-progress assistant Parts to DB. Bounds text loss on
	// SIGTERM during final composition. 0 (default) = disabled.
	// Fork patch: batch 8.
	CheckpointInterval time.Duration
	// TimeoutExtendsOnProgress, when true, makes the stream watchdog
	// reset its deadline every time streaming progress occurs.
	// Fork patch: batch 8.
	TimeoutExtendsOnProgress bool
	// TimeoutHardCap is the maximum wall-clock time the watchdog will
	// allow even with continuous progress. 0 = no cap.
	// Fork patch: batch 8.
	TimeoutHardCap time.Duration
	// TimeoutOptionsSet marks the two timeout fields above as THIS
	// invocation's deliberate policy, even when both are zero ("no
	// watchdog extension, no hard cap, on purpose"). It exists for
	// in-process (SDK) callers, which construct RunOverrides as a Go
	// struct and can set it explicitly; the CLI never sets it, because
	// cobra flags cannot distinguish "not passed" from "passed as
	// false/0" — ExecuteRun keeps deriving presence from the two fields
	// when this bit is clear. Not persisted on the session. Fork patch
	// (F3, 2026-09-01 SDK review).
	TimeoutOptionsSet bool
	// OnFinishHook is an optional shell command to execute after the run
	// completes. Environment variables are set with run metadata.
	// Errors from the hook are printed to stderr but don't affect exit code.
	// Fork patch: batch 24.
	OnFinishHook string
	// MaxCost aborts the run if total session cost (USD) exceeds this value.
	// 0 = no cap. Fork patch: batch 30.
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this
	// value. 0 = no cap. Fork patch: batch 30.
	MaxTokens int64
	// AllowPeakHours, when true, bypasses the per-provider peak_hours refusal
	// for this single invocation. `rush run --allow-peak-hours` sets it.
	// There is intentionally NO config-level "always allow" equivalent: the
	// whole point is a conscious one-off override. Fork patch (peak-hours
	// bypass).
	AllowPeakHours bool
	// Timeout is the original --timeout duration, carried for budget
	// persistence so `sessions show` / `sessions locks` can display it.
	// The context-level deadline is applied separately by the caller.
	// Fork patch (operator UX).
	Timeout time.Duration
	// RestrictedRun enables the restricted-run permission model for
	// this non-interactive invocation, merged with
	// permissions.run.restrict from config. When armed, only allowlist
	// matches are auto-approved; everything else is denied cleanly.
	// Fork patch (run allowlist).
	RestrictedRun bool
	// AllowBash appends bash command patterns for this run, merged with
	// permissions.run.allow_bash from config. Fork patch (run allowlist).
	AllowBash []string
	// AllowTools appends tool (or tool:action) entries for this run,
	// merged with permissions.run.allow_tools from config.
	// Fork patch (run allowlist).
	AllowTools []string
	// Origin marks the entry channel of this invocation. This is the
	// CLI's transport for the origin: `rush run` sets OriginCLI here and
	// RunNonInteractive forwards it into the RunRequest it builds
	// (RunNonInteractive takes parameters directly, not a RunRequest).
	// Empty = unspecified.
	Origin message.Origin
}

// RunRequest bundles everything one ExecuteRun invocation needs,
// including the streams it may write to. Nil Stdout/Stderr fall back
// to io.Discard so a library caller can opt out of streaming output.
type RunRequest struct {
	Prompt            string
	Overrides         RunOverrides
	Mode              RunMode
	ContinueSessionID string
	UseLast           bool
	Stdout            io.Writer // nil → io.Discard
	Stderr            io.Writer // nil → io.Discard
	HideSpinner       bool

	// Credentials, when non-nil, runs THIS invocation on the given
	// provider credentials instead of whatever rush.json/env would
	// resolve — the per-tenant entry point of the embeddable SDK
	// (sdk.Client.RunWithCredentials). The set replaces model/provider
	// resolution for every role it covers; nothing is merged with config
	// or environment credentials, and roles it does not cover fall back
	// to the ordinary resolution path. Ordinary Run leaves it nil and
	// behaves exactly as before.
	Credentials *agent.CredentialSet
	// FailIfSessionBusy changes what happens when the resolved session
	// already has an in-process owner (AgentCoordinator.IsSessionBusy).
	// The zero value (false) keeps the historical behaviour: the prompt
	// is silently queued behind the running turn — the mailbox queue the
	// CLI and the web server rely on. true rejects the request
	// immediately, before any turn goroutine is started, with an error
	// wrapping agent.ErrSessionBusy. The SDK's Run/RunWithCredentials set
	// it so embedders get a fail-fast answer instead of a hidden queue.
	FailIfSessionBusy bool
	// Origin marks the entry channel of this request
	// (message.OriginCLI/Web/SDK). Persisted on the session this run
	// creates or attaches to, and stamped on the user message the turn
	// creates. The zero value (unspecified) preserves the historical
	// behaviour. The SDK's Run/RunWithCredentials default it to
	// message.OriginSDK when the caller left it unspecified.
	Origin message.Origin
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	origin := agent.CallOriginFrom(ctx)
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err == nil {
			if sess.ParentSessionID != "" {
				return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
			}
			return sess, nil
		}
		// Get-or-create semantics: --session <id> with an unknown id creates
		// a brand-new top-level session with that exact id. Lets CI / scripts
		// pick a deterministic key (e.g. an issue number) and re-run idempotently.
		var created session.Session
		var createErr error
		if oc, ok := app.Sessions.(session.OriginCreator); ok {
			created, createErr = oc.CreateWithIDAndOrigin(ctx, continueSessionID, continueSessionID, origin)
		} else {
			// Test fakes implementing session.Service without the
			// OriginCreator seam keep the legacy behaviour.
			created, createErr = app.Sessions.CreateWithID(ctx, continueSessionID, continueSessionID)
		}
		if createErr == nil {
			slog.Info("Created session on demand from --session id", "session_id", created.ID)
			return created, nil
		}
		// Session-creation race (task #605): several `rush run --session
		// <id>` processes can all miss the Get above for an id that has
		// NEVER existed before (first use of that id) and then all race
		// CreateWithID's INSERT. SQLite's own single-writer serialization
		// still guarantees exactly one INSERT wins — the losers get back a
		// PRIMARY KEY/UNIQUE constraint violation on sessions.id, e.g.
		// "constraint failed: UNIQUE constraint failed: sessions.id
		// (1555)". Before this fix that raw driver error propagated
		// straight to the operator/orchestrator instead of the friendly
		// "session busy, use `sessions inject`" message the already-exists
		// race already gets (see sessionBusyGuidance in app_run_errors.go)
		// — an orchestrator parsing stderr would misclassify a transient,
		// retryable race as a permanent failure.
		//
		// Fix: re-Get once. By the time our own INSERT has been rejected
		// by the UNIQUE/PRIMARY KEY index, the winner's INSERT has
		// necessarily already committed (SQLite's single-writer model
		// serializes the two transactions; ours only sees the conflict
		// because theirs finished first) — so the row is guaranteed to be
		// there for a losing process to attach to. Re-Get is the shape
		// that reuses ALL of the existing, tested busy-rejection path:
		// once attached, this losing process proceeds exactly like the
		// already-exists case above, and the pre-existing OS-level
		// session lock (internal/agent/agent_run.go, checked once this
		// function returns and AgentCoordinator.Run acquires it) is what
		// actually produces the clean "session busy" guidance — no new
		// error-classification branch or wrapping needed here.
		//
		// This intentionally only swallows the constraint violation for
		// THIS table+column (sessions.id) via isSessionsIDConstraintError
		// below, not any database error: a genuinely broken DB (disk
		// full, corruption, permission denied) must still surface as-is
		// rather than being silently retried into a confusing "not
		// found" on the follow-up Get.
		if isSessionsIDConstraintError(createErr) {
			if sess, getErr := app.Sessions.Get(ctx, continueSessionID); getErr == nil {
				if sess.ParentSessionID != "" {
					return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
				}
				slog.Info("Session creation raced another process; attached to the winner's row",
					"session_id", continueSessionID)
				return sess, nil
			}
			// The re-Get failed too (extremely unlikely — the row that
			// caused our constraint violation vanished again, e.g. a
			// concurrent `sessions kill`/delete). Fall through to the
			// original wrapped error below rather than hiding the
			// surprise.
		}
		return session.Session{}, fmt.Errorf("session %q not found and could not be created: %w", continueSessionID, createErr)

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		if oc, ok := app.Sessions.(session.OriginCreator); ok {
			return oc.CreateWithOrigin(ctx, agent.DefaultSessionName, origin)
		}
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}

// agentTurnResponse carries the outcome of one asynchronous
// AgentCoordinator.Run call back to RunNonInteractive's event loop, via the
// done channel below runAgentTurnRecovered writes into.
type agentTurnResponse struct {
	result *fantasy.AgentResult
	err    error
}

// runAgentTurnRecovered runs runFn (normally app.AgentCoordinator.Run) on the
// calling goroutine and always sends exactly one agentTurnResponse into done,
// including when runFn panics.
//
// Without this, a panic anywhere inside AgentCoordinator.Run — which
// includes every tool call it executes synchronously, e.g. bash, edit,
// grep, or a sub-agent delegation via the `agent` tool (runSubAgent calls
// params.Agent.Run synchronously on this same call stack, see
// coordinator.go's runSubAgent) — would crash the entire rush.exe process
// via Go's default panic handler, which writes only to os.Stderr and never
// through slog. For `rush run` invocations whose stderr isn't captured by
// whatever launched them, that's a silent death with zero log output. It
// also left RunNonInteractive's `case result := <-done` select blocked
// forever, since nothing would ever send on the channel.
//
// This mirrors the panic-isolation pattern already used for WebSocket
// handlers in internal/server/hub.go (runRecovered / Client.dispatch): log
// via slog.Error with a full stack trace, then report a normal error result
// instead of letting the panic propagate.
func runAgentTurnRecovered(
	ctx context.Context,
	sessionID, prompt string,
	runFn func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error),
	done chan<- agentTurnResponse,
) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("agent turn panic",
				"session_id", sessionID,
				"panic", r,
				"stack", string(debug.Stack()))
			done <- agentTurnResponse{
				err: fmt.Errorf("agent turn panicked (recovered, see rush.log for stack trace): %v", r),
			}
		}
	}()

	result, err := runFn(ctx, sessionID, prompt)
	if err != nil {
		done <- agentTurnResponse{
			err: fmt.Errorf("failed to start agent processing stream: %w", err),
		}
		return
	}
	if result == nil {
		// sessionAgent.Run's ONLY (nil, nil) return is the legacy
		// queueing path (mailbox.submit's "caller queues and returns
		// nil" branch, agent_run.go): every fail-fast busy rejection
		// — the FailIfSessionBusy branch right next to it, and every
		// earlier fast-path check in ExecuteRun (IsSessionBusy,
		// ReserveExclusive) — already wraps agent.ErrSessionBusy in a
		// non-nil err, caught by the branch above. So reaching here
		// with a nil err means this call queued behind the current
		// owner and returned immediately, exactly as the R1-4 legacy
		// queueing contract intends: nothing to report YET (the
		// eventual queued turn runs under the owner's own dispatcher
		// loop and its messages/results land in the SAME session,
		// picked up by messageEvents below) — not a failure. Treating
		// it as agent.ErrSessionBusy here (as this used to) broke
		// exactly the callers this contract exists for: two
		// legacy (non-fail-fast) ExecuteRun calls on one busy session
		// would have the loser's call fail hard instead of queueing,
		// silently regressing R1-4 the instant a caller's timing
		// landed it in the genuine mid-turn queueing window rather
		// than racing in fresh after the owner released (round-3
		// review, R3 follow-up: CI's macOS runner hit the queueing
		// window; a fast idle dev machine almost never does, which is
		// why this went unnoticed until load-sensitive CI exposed it).
		done <- agentTurnResponse{}
		return
	}
	done <- agentTurnResponse{
		result: result,
	}
}

// drainOutcomeError translates DrainSessionNow's (result, drainErr) pair into the
// final error that RunNonInteractive should return. It enforces the contract
// that DrainPartial and DrainFailed must always carry a non-nil drainErr,
// converting a violation into a wrapped ErrDrainFailureUnspecified so that the
// run exits non-zero rather than silently treating a nil drain as success.
//
// Task #616/P2-2 (2026-08-20 read-only release review): DrainNoWork now has
// its OWN case rather than falling into default alongside a genuinely
// invalid DrainResult. Before this, an unrecognized future DrainResult value
// (a bug: e.g. a fifth enum member added to session.DrainResult without a
// matching case here) would silently inherit DrainNoWork's "return
// originalErr unchanged" behavior instead of being caught -- exactly the
// kind of contract-drift this file's other two cases already guard against
// for DrainComplete/DrainPartial/DrainFailed. See session.DrainNoWork's own
// doc for why originalErr (not drainErr) is the right thing to return here:
// DrainSessionNow's own doc records that a few of its early-exit paths pair
// DrainNoWork with a non-nil, call-scoped drainErr (this call's own ctx
// already done, its own lease attempt failing, etc.) that describes why
// DrainSessionNow itself did not run anything -- not a row's outcome -- so
// surfacing THAT would be reporting information about DrainSessionNow's
// internal retry loop instead of the run's actual outcome. originalErr is
// guaranteed non-nil on this function's sole production call site
// (app_run.go's isCanceled gate below requires runErr != nil before
// DrainSessionNow is ever invoked), so this is traced, not live-tested, the
// same way task #607/commit 732155ad already recorded for this exact path.
func drainOutcomeError(sessID string, result session.DrainResult, drainErr, originalErr error) error {
	switch result {
	case session.DrainComplete:
		// DrainComplete is the ONLY DrainResult DrainSessionNow ever pairs
		// with a nil error (see its own doc), so drainErr is guaranteed nil
		// here; asserted defensively rather than trusted blindly.
		if drainErr != nil {
			slog.Error("run: DrainSessionNow reported DrainComplete with a non-nil error -- contract violation, treating as failure", "session_id", sessID, "err", drainErr)
			return drainErr
		}
		return nil
	case session.DrainPartial, session.DrainFailed:
		// At least one row genuinely executed, but the call did not end in a
		// full, confirmed success -- surface that as the run's outcome. This
		// guard closes the consumer-side hole where a nil drainErr would be
		// forwarded as-is, causing finish(nil) to exit 0 despite a
		// Partial/Failed result.
		if drainErr == nil {
			slog.Error("run: DrainSessionNow reported a partial/failed drain with a nil error -- contract violation, treating as failure", "session_id", sessID, "result", result.String())
			return fmt.Errorf("%w (session=%s)", session.ErrDrainFailureUnspecified, sessID)
		}
		return drainErr
	case session.DrainNoWork:
		// Nothing ran here. This case covers BOTH the shapes task
		// #624/F-5 distinguishes: a call that never observed a same-pump
		// admission, AND a call that DID observe one — the latter keeps
		// its DrainNoWork even when an outstanding leased row exists,
		// because that row is plausibly the local admission holder's
		// in-flight work. Either nothing was pending, or something was
		// but a live owner WITHIN THIS PUMP's admission held the session,
		// or this call stopped for a call-scoped reason
		// of its own (ctx already done, its own lease attempt failing at
		// the DB layer, etc -- see session.DrainNoWork's own doc; drainErr
		// may be non-nil in that case, but it describes why
		// DrainSessionNow itself did not run anything, not this run's
		// outcome). All of those mean the same thing to this command: no
		// continuation completed in this process, so the original
		// cancellation stands. It was real.
		//
		// NOTE (task #624/F-5): a DIFFERENT kind of "different live
		// owner" -- another PROCESS's pump holding a live DB lease on a
		// row of this session, with no admission this call could ever
		// observe -- no longer reaches this case. Leasing a row never
		// consults the OS session lock, so such a row is indistinguishable
		// from an orphaned one by the only query available, and
		// DrainSessionNow now reports it as DrainFailed/
		// ErrOutstandingRunQueueEntry instead (mirroring task #610's
		// already-accepted reporting for calls that DID execute
		// something). That lands in the DrainPartial/DrainFailed case
		// above and returns drainErr instead of originalErr -- both are
		// non-nil here, so the run exits non-zero either way; what changes
		// is WHICH error the operator sees, and the outstanding-row one
		// names the undrained durable work rather than repeating the
		// original cancellation.
		return originalErr
	default:
		// An unrecognized DrainResult -- a programming error (a new
		// session.DrainResult value added without a matching case here), not
		// a reachable production outcome today. Silently falling through to
		// originalErr (the pre-fix behavior) would let a future enum
		// addition inherit DrainNoWork's semantics by accident, hiding a
		// contract violation exactly like the DrainComplete/DrainPartial/
		// DrainFailed guards above already refuse to do for THEIR contracts.
		// Log it and return a non-nil sentinel so the run exits non-zero
		// rather than risking a false success.
		slog.Error("run: DrainSessionNow reported an unrecognized DrainResult -- contract violation, treating as failure", "session_id", sessID, "result", result.String(), "err", drainErr)
		return fmt.Errorf("%w (session=%s, result=%s)", session.ErrDrainFailureUnspecified, sessID, result.String())
	}
}

// credentialsRunner is the slice of the coordinator that supports
// per-call credential isolation (agent's RunWithCredentials). A
// consuming-package interface on purpose, and type-asserted rather than
// added to agent.Coordinator, so that interface's many existing test
// fakes stay untouched.
type credentialsRunner interface {
	RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *agent.CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

// ExecuteRun computes a single agent turn and returns the result envelope
// without rendering it (see RunMode for the output shapes that shape the
// streaming behaviour). Streaming output goes to req.Stdout, diagnostics to
// req.Stderr; nil falls back to io.Discard. For RunModeJSON the caller is
// responsible for encoding the returned *RunResult.
func (app *App) ExecuteRun(ctx context.Context, req RunRequest) (*RunResult, error) {
	prompt := req.Prompt
	overrides := req.Overrides
	mode := req.Mode
	continueSessionID := req.ContinueSessionID
	useLast := req.UseLast
	hideSpinner := req.HideSpinner
	stdout := io.Writer(io.Discard)
	if req.Stdout != nil {
		stdout = req.Stdout
	}
	stderr := io.Writer(io.Discard)
	if req.Stderr != nil {
		stderr = req.Stderr
	}
	smartModel := overrides.SmartModel
	fastModel := overrides.FastModel
	systemPrompt := overrides.SystemPrompt
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Fork patch: batch 14 — mark the agent context as non-interactive.
	// cliprovider.Stream reads this and forces bypass-permissions on the
	// inner CLI sub-process (claude / codex / gemini) so it doesn't hang
	// waiting for a permission prompt that no human is there to answer.
	// See cliprovider.NonInteractiveContextKey.
	ctx = context.WithValue(ctx, cliprovider.NonInteractiveContextKey, true)
	// Tag the entry-channel origin on the turn context: resolveSession
	// reads it to persist the origin on a freshly created session, and
	// buildCall stamps it onto the call so createUserMessage persists it
	// on the user message. Empty/unspecified leaves both untouched.
	ctx = agent.WithCallOrigin(ctx, req.Origin)

	// Per-call credentials (sdk.Client.RunWithCredentials): validate the
	// bundle before any session work so a malformed set fails fast, and
	// keep the credentials-capable coordinator for the run handoff below.
	var credsRunner credentialsRunner
	if req.Credentials != nil {
		if err := req.Credentials.Validate(); err != nil {
			return nil, fmt.Errorf("invalid per-call credentials: %w", err)
		}
		cr, ok := app.AgentCoordinator.(credentialsRunner)
		if !ok {
			return nil, fmt.Errorf("per-call credentials are not supported by this coordinator")
		}
		credsRunner = cr
	}

	if smartModel != "" || fastModel != "" {
		if err := app.overrideModelsForNonInteractive(ctx, smartModel, fastModel); err != nil {
			return nil, fmt.Errorf("failed to override models: %w", err)
		}
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		spinner = format.NewSpinner()
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Wait for MCP initialization to complete before reading MCP tools.
	if err := mcp.WaitForInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// R1-1 (P0): build this run's IMMUTABLE per-call execution context
	// and attach it to the run's context. Everything below that used to
	// pin per-invocation settings onto SHARED coordinator/permission
	// state — SetActiveModelRole, SetAgentTimeoutOptions, SetRunLimits,
	// SetAllowPeakHours, the published-config DisableSubAgents
	// mutation, and the process-wide SetRunAllowlist — now travels in
	// this one value instead. On one *App (web server, sdk.Client) two
	// overlapping runs each carry their own policy: previously they
	// raced for every one of those shared fields, and a run could
	// execute under another run's role, caps, bypass, allowlist or
	// stripped toolset. The coordinator's Set* methods remain the
	// fallback path for legacy (non-ExecuteRun) callers and are
	// deliberately untouched.
	//
	// Attached before session resolution on purpose: the coordinator's
	// per-call toolset build (resolveSessionModels -> buildTools) reads
	// the DisableSubAgents filter and ModelRole from this context, and
	// buildAgent captures the role synchronously at registration time.
	callOpts := &agent.CallOptions{
		ModelRole:                overrides.ModelRole,
		TimeoutExtendsOnProgress: overrides.TimeoutExtendsOnProgress,
		TimeoutHardCap:           overrides.TimeoutHardCap,
		// R3-6: RunOverrides cannot distinguish "flag not passed" from
		// "flag passed as false/0" (plain bool/duration fields; the CLI
		// reads them with GetBool/GetString defaults), so presence is
		// still derived from the same predicate the turn-time check
		// used, and CLI behaviour is unchanged. F3 (2026-09-01 SDK
		// review): an in-process caller can additionally set
		// Overrides.TimeoutOptionsSet to make even an all-zero policy
		// deliberate instead of an inheritance of the sessionAgent's
		// shared fields. The derived predicate stays in the OR because
		// removing it would silently break the CLI's
		// --timeout-extends-on-progress / --timeout-hard-cap flags,
		// whose only presence signal is this predicate. See
		// CallOptions.TimeoutOptionsSet.
		TimeoutOptionsSet: overrides.TimeoutOptionsSet || overrides.TimeoutExtendsOnProgress || overrides.TimeoutHardCap > 0,
		MaxCost:           overrides.MaxCost,
		MaxTokens:         overrides.MaxTokens,
		AllowPeakHours:    overrides.AllowPeakHours,
		DisableSubAgents:  overrides.DisableSubAgents,
		FailIfSessionBusy: req.FailIfSessionBusy,
	}
	ctx = agent.WithCallOptions(ctx, callOpts)

	// Fork patch (orchestrator UX): --agents single. The agent /
	// agentic_fetch tools are stripped from the coder's toolset for THIS
	// run only, via CallOptions.DisableSubAgents consumed inside the
	// coordinator's per-build toolset filter (applyCallDisableSubAgents).
	// The former implementation mutated the PUBLISHED coder AllowedTools
	// and restored it via defer (the R3 fix): on one *App that write raced
	// every concurrent run's buildTools for the whole duration of the
	// mutating run, so a DisableSubAgents:false call could observe the
	// delegating call's stripped toolset until the restore fired. Nothing
	// shared is touched anymore — there is nothing to snapshot or restore
	// (shouldBypassSubAgentBan/disableToolsInConfig stay in
	// app_run_gates.go for their direct tests and as the config-level
	// gate utility).
	//
	// Plan phase 2 exception is preserved with identical semantics: when
	// this run is --role smart with a Worker model configured, the
	// filter restores ONLY the `agent` tool — a configured worker means
	// delegation is the intended workflow, even when --agents single was
	// passed explicitly. `agentic_fetch` stays stripped either way.

	// R3-1: the former per-run UpdateModels(ctx) call is GONE. It was the
	// publisher that leaked THIS run's per-call tool filter onto shared
	// state: it rebuilt the coder toolset with this ctx's CallOptions and
	// SetTools'd the result onto the ONE shared currentAgent — before
	// session resolution and before ReserveExclusive — so a same-session
	// fail-fast loser clobbered the winner's live toolset, and calls with
	// opposite DisableSubAgents raced each other's in-flight turns (which
	// re-read the shared slice at every PrepareStep). Per-call toolsets are
	// now built and pinned inside the coordinator's resolveSessionModels;
	// UpdateModels remains only as a global config/MCP refresh and no
	// longer consumes per-call state at all.

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return nil, fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}

	// FailIfSessionBusy (sdk.Client.Run/RunWithCredentials): reject the
	// request when the session already has an in-process owner, instead
	// of silently queueing behind it. Opt-in on purpose: `rush run` and
	// the web server keep their intentional queueing behaviour.
	//
	// This pre-check is only the FAST path — it avoids spawning a turn
	// goroutine in the common already-busy case. It cannot close the
	// check-then-act window by itself: for FailIfSessionBusy callers the
	// atomic mailbox reservation immediately below both decides admission
	// and carries the ownership era into the turn; for queueing callers the
	// contract is still enforced at the session's mailbox reservation inside
	// sessionAgent.Run (mailbox.submit), whose single check-and-set returns
	// without queueing for such a call, so sessionAgent.Run reports
	// ErrSessionBusy (R1-4).
	if req.FailIfSessionBusy && app.AgentCoordinator.IsSessionBusy(sess.ID) {
		slog.Warn("Run rejected: session already has an in-process owner", "session_id", sess.ID)
		return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
	}

	// R2-1/R2-3 (round-2 SDK review): for fail-fast callers claim the
	// session's mailbox ownership ATOMICALLY right here — before ANY
	// per-run mutation below (system prompt, reasoning effort, auto-approve,
	// the per-session permission policy, cancel flag, budget, ended_reason,
	// title). The old shape mutated shared session state first and let the
	// real admission decision happen much later, deep inside mailbox.submit
	// (sessionAgent.Run): two simultaneous FailIfSessionBusy callers on one
	// idle session both passed the fast-path check above, both mutated
	// shared state, and the eventual loser's deferred
	// ClearSessionRunAllowlist then deleted the WINNER's armed policy
	// (R2-1), while a caller that was about to be rejected could rewrite
	// the running session's prompt, budget or title before learning it had
	// lost (R2-3). ReserveExclusive is the same atomic mbIdle->mbOwned
	// check-and-set mailbox.submit performs, reused per the review's
	// recommendation ("Move atomic run admission ahead of side effects.
	// This can also provide the owner token needed to solve R2-1
	// cleanly").
	//
	// FailIfSessionBusy == false (`rush run`, the web server) keeps the
	// intentional queueing contract untouched (R1-4): no reservation is
	// attempted, the mutations below run at queue time exactly as before,
	// and mailbox.submit decides ownership/queueing when the turn
	// goroutine reaches it.
	var (
		reservedEpoch   uint64
		reservedCancel  context.CancelFunc
		reservedHandoff atomic.Bool
		// reservedHold is the holdCtx returned by ReserveExclusive
		// (context.WithCancel-derived); Cancel(sessionID)/CancelAll landing
		// during the hold cancels it via the session mailbox.
		reservedHold context.Context
	)
	if req.FailIfSessionBusy {
		holdCtx, epoch, cancel, ok := app.AgentCoordinator.ReserveExclusive(ctx, sess.ID)
		if !ok {
			slog.Warn("Run rejected: session already has an in-process owner (atomic reservation)", "session_id", sess.ID)
			return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
		}
		reservedEpoch = epoch
		reservedCancel = cancel
		reservedHold = holdCtx
		// Ownership continues into the turn: the token makes sessionAgent.Run
		// CONTINUE this era (the same rebindDispatcher mechanism /compact and
		// rerun already use) instead of racing a fresh submit() that would
		// find the mailbox owned by this very caller. onHandoff disarms the
		// bail-out release below exactly when the turn takes over:
		// ExecuteRun can return on cancellation while the turn goroutine is
		// still running, so a blind release defer would end a live era under
		// a running turn. Every error return BETWEEN here and the handoff
		// (UpdateSystemPrompt and friends) releases via this defer instead.
		ctx = agent.WithReservedOwnership(ctx, sess.ID, reservedEpoch, reservedCancel, func() {
			reservedHandoff.Store(true)
		})
		defer func() {
			if !reservedHandoff.Load() {
				app.AgentCoordinator.ReleaseExclusive(sess.ID, reservedEpoch, reservedCancel)
			}
		}()
	}

	// R3-3: mutCtx governs every pre-handoff session mutation below. On
	// the reserved path it is reservedHold, the context ReserveExclusive
	// derives via context.WithCancel and wires into the mailbox as the
	// era's cancel target, so a Cancel(sessionID)/CancelAll landing during
	// the hold window cancels it while caller-ctx cancellation still
	// propagates (it derives from ctx). Previously ExecuteRun discarded
	// holdCtx, so mailbox-directed cancellation in this window fired a
	// placeholder nobody observed and the mutations and turn proceeded
	// anyway. After the handoff holdCtx is intentionally dead
	// (RunWithReservedOwnership / the pre-reserved-ownership path fires it
	// once the turn's own cancel is live), so the turn goroutine and
	// everything downstream keeps the original ctx.
	mutCtx := ctx
	if reservedHold != nil {
		mutCtx = reservedHold
	}

	// checkHoldCanceled is checked before every pre-handoff mutation block
	// and immediately before the handoff, so a canceled hold bails out
	// through the armed ReleaseExclusive defer without mutating session
	// state or starting a turn.
	//
	// F6 (2026-09-01 SDK review): admissionAborted records that a
	// bail-out actually fired. The ended_reason and on-finish-hook
	// defers below are registered before the LAST gate and write
	// through Background-derived contexts on purpose (a started run's
	// cleanup must complete even when its own ctx is already gone), so
	// ctx cancellation cannot stop them in the window where the whole
	// run never started; they check this flag instead. A plain bool is
	// enough: every checkHoldCanceled call site and both defers run on
	// ExecuteRun's goroutine.
	admissionAborted := false
	checkHoldCanceled := func() error {
		if reservedHold == nil {
			return nil
		}
		err := reservedHold.Err()
		if err == nil {
			return nil
		}
		admissionAborted = true
		slog.Warn("Run abandoned: cancellation landed during admission hold", "session_id", sess.ID, "err", err)
		return fmt.Errorf("session %q canceled during run admission: %w", sess.ID, err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Persist the requested system prompt for this session. Coordinator's
	// resolveSessionSystemPrompt will pick it up on the next Run(); leaving
	// systemPrompt empty preserves whatever was previously stored (or causes
	// the default prompt to be built and stored on first run).
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if systemPrompt != "" {
		if err := app.Sessions.UpdateSystemPrompt(mutCtx, sess.ID, systemPrompt); err != nil {
			return nil, fmt.Errorf("failed to set system prompt for session: %w", err)
		}
	}

	// Persist reasoning effort onto the active slot. We pass the current
	// stored value for the *other* slot through so we don't clobber it —
	// UpdateReasoningEffort takes both fields as a single transaction.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if overrides.ReasoningEffort != "" {
		smart := sess.SmartModelReasoningEffort
		fast := sess.FastModelReasoningEffort
		if overrides.RoleSmart {
			smart = overrides.ReasoningEffort
		} else {
			fast = overrides.ReasoningEffort
		}
		if err := app.Sessions.UpdateReasoningEffort(mutCtx, sess.ID, smart, fast); err != nil {
			return nil, fmt.Errorf("failed to set reasoning effort: %w", err)
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	// checkHoldCanceled guards both calls below: they have no ctx parameter
	// to carry cancellation, so the hold check is the only gate.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	app.Permissions.AutoApproveSession(sess.ID)

	// Fork patch (run allowlist): build the restricted-run allowlist by
	// merging the config-derived spec with this invocation's CLI
	// overrides. Even when no override is passed we rebuild from config
	// so the gate stays consistent with whatever permissions.run was on
	// disk at run time. Only affects the auto-approve path exercised
	// above; interactive sessions never run this code. R2-1's epoch
	// binding has been superseded by the per-call binding (R3-4), which
	// covers the reserved path too: the reserved call also carries its
	// policy and arms it at turn start.
	runSpec := runAllowlistSpecFromConfig(app.config.Config().Permissions)
	if overrides.RestrictedRun {
		runSpec.Restrict = true
	}
	runSpec.AllowBash = append(runSpec.AllowBash, overrides.AllowBash...)
	runSpec.AllowTools = append(runSpec.AllowTools, overrides.AllowTools...)
	compiled, allowErr := permission.BuildRunAllowlist(runSpec)
	if allowErr != nil {
		slog.Warn("Restricted-run allowlist has invalid patterns (skipping them)", "err", allowErr)
	}
	// F2: pin THIS run's compiled policy as the session baseline. It was
	// once the mechanism that kept a durably restarted turn restricted, but
	// it is now a DEMOTED legacy-row fallback: the durable run queue
	// persists each call's own policy spec (WithRunAllowlistSpec below) and
	// the pump recompiles and re-arms it per call, keyed by LogicalCallID —
	// Request consults the per-call entry first — so the baseline no longer
	// governs any NEW row. It now only judges turns rebuilt from rows
	// persisted before the spec field existed (no spec in their JSON), for
	// which nothing better can be reconstructed in-process. Residual: after
	// a real process restart even the in-memory baseline is gone, and a
	// legacy row then falls to the unrestricted process-wide gate — an
	// accepted, documented migration-window behavior that any re-run
	// through ExecuteRun heals. Never cleared on run end — a later run on
	// the same session re-arms it.
	if mgr, ok := app.Permissions.(permission.SessionRunAllowlistBaselineManager); ok {
		mgr.SetSessionRunAllowlistBaseline(sess.ID, compiled)
	}
	// R2-1: the unconditional process-wide SetRunAllowlist write is GONE
	// from this path.
	//
	// R3-4: the compiled policy is no longer armed here at call time —
	// that write raced the mailbox: a queued (FailIfSessionBusy=false)
	// call overwrote the ACTIVE owner's entry at queue time and its
	// front-end defer cleared the only entry before the queued turn ever
	// ran, and even the reserved path's entry armed before the turn
	// goroutine had been admitted by the mailbox. The policy now travels
	// with the call (agent.WithRunAllowlist below, stamped onto
	// SessionAgentCall by buildCall/runInternal) and the turn loop arms it
	// — bound to the call's LogicalCallID — only when the call actually
	// becomes the active turn, for BOTH the reserved and the legacy
	// queueing path, and clears it with that same call id at loop end.
	// FailIfSessionBusy==false keeps the queueing contract (R1-4)
	// untouched: queueing no longer has ANY global side effect at call
	// time.
	ctx = agent.WithRunAllowlist(ctx, &compiled)
	// R4-1/R4-2/R4-3: the UNCOMPILED spec travels with the call too, so it
	// is serialized onto any durable run-queue row this call produces
	// (ToSessionAgentCallData). The pump rebuilds the call, recompiles the
	// spec, and arms the restarted turn's OWN policy keyed by its
	// LogicalCallID — replacing the per-session baseline below as the
	// mechanism that keeps a durable restart restricted.
	ctx = agent.WithRunAllowlistSpec(ctx, &runSpec)

	// Fork patch: batch 8/30 + peak-hours bypass (R1-1). This run's
	// timeout-extension policy, cost/token caps and peak-hours bypass now
	// travel in callOpts (WithCallOptions above) and are consumed per call
	// by runInternal/agent_turn — the former SetAgentTimeoutOptions /
	// SetRunLimits / SetAllowPeakHours calls wrote coordinator-wide state
	// that a concurrent run could overwrite before this run's turn read
	// it.

	// Fork patch: batch 30 — clear stale cancel flag.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	if err := app.Sessions.ClearCancelRequest(mutCtx, sess.ID); err != nil {
		slog.Warn("Failed to clear cancel request flag", "session_id", sess.ID, "err", err)
	}

	// Fork patch (operator UX): persist budget at run start so
	// `sessions show` / `sessions locks` can display "cost vs limit".
	// Also clear ended_reason since the session is being (re)started.
	if err := app.Sessions.SetBudget(mutCtx, sess.ID, overrides.MaxCost, overrides.MaxTokens, int64(overrides.Timeout.Seconds())); err != nil {
		slog.Warn("Failed to persist budget", "session_id", sess.ID, "err", err)
	}
	if err := app.Sessions.SetEndedReason(mutCtx, sess.ID, ""); err != nil {
		slog.Warn("Failed to clear ended_reason", "session_id", sess.ID, "err", err)
	}

	// Fork patch (operator UX): auto-title from first user prompt. If the
	// session title is empty or "Untitled Session", set it to the first 60
	// chars of the user prompt. Makes `sessions list` immediately useful
	// without requiring the orchestrator to pass a title.
	if prompt != "" && (sess.Title == "" || sess.Title == "Untitled Session" || sess.Title == sess.ID) {
		autoTitle := prompt
		if len(autoTitle) > 60 {
			autoTitle = autoTitle[:60] + "…"
		}
		// Strip newlines so it fits in one line in `sessions list`.
		autoTitle = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, autoTitle)
		if err := app.Sessions.Rename(mutCtx, sess.ID, autoTitle); err != nil {
			slog.Warn("Failed to auto-title session", "session_id", sess.ID, "err", err)
		}
	}

	// Fork patch: batch 24 — on-finish hook support. Captures run
	// metadata as it becomes available and executes the hook on return.
	var (
		hookExitReason string
		hookCost       float64
		hookTokens     int64
	)
	runStart := time.Now()
	tokensBefore := sess.PromptTokens + sess.CompletionTokens
	costBefore := sess.Cost

	// Fork patch (operator UX): persist ended_reason when the run finishes.
	// hookExitReason is always set before return, so this defer fires after it.
	//
	// A-2 (task #779): these defers deliberately use context.Background()
	// rather than the run's own ctx -- by the time they fire, ctx is
	// already cancelled (cancel() above, or a --timeout/interrupt), so
	// reusing it would make the write fail instantly every time, silently
	// dropping ended_reason/usage bookkeeping on every cancelled run. But
	// an unbounded Background() context can block for the full SQLite
	// busy_timeout (30s, see internal/db/connect.go's pragma map) waiting
	// on a lock -- to the user, who already has their result printed, that
	// looks like a hang. cleanupTimeout gives these best-effort writes a
	// short budget of their own: long enough to clear an ordinary,
	// momentary busy_timeout contention window, short enough that a
	// genuinely stuck writer can't reproduce the 30s freeze this fixes.
	defer func() {
		if admissionAborted {
			slog.Info("Skipping ended_reason write: the run never started (canceled during admission)", "session_id", sess.ID)
			return
		}
		reason := hookExitReason
		if reason == "" {
			reason = "done"
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cleanupCancel()
		if setErr := app.Sessions.SetEndedReason(cleanupCtx, sess.ID, reason); setErr != nil {
			slog.Warn("Failed to persist ended_reason", "session_id", sess.ID, "reason", reason, "err", setErr)
		}
	}()
	if overrides.OnFinishHook != "" {
		defer func() {
			if admissionAborted {
				slog.Info("Skipping on-finish hook: the run never started (canceled during admission)", "session_id", sess.ID)
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cleanupCancel()
			if freshSess, err := app.Sessions.Get(cleanupCtx, sess.ID); err == nil {
				hookTokens = freshSess.PromptTokens + freshSess.CompletionTokens - tokensBefore
				hookCost = freshSess.Cost - costBefore
			} else {
				slog.Warn("Failed to refresh session for on-finish hook usage", "session_id", sess.ID, "err", err)
			}
			duration := time.Since(runStart)
			runOnFinishHook(overrides.OnFinishHook, sess.ID, hookExitReason, hookCost, hookTokens, duration)
		}()
	}

	done := make(chan agentTurnResponse, 1)

	runFn := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return app.AgentCoordinator.Run(ctx, sessionID, prompt)
	}
	if credsRunner != nil {
		creds := req.Credentials
		runFn = func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
			return credsRunner.RunWithCredentials(ctx, sessionID, prompt, creds)
		}
	}
	// Last gate before the turn goroutine launches. A cancel landing in
	// the irreducible window between this check and the turn's mailbox
	// rebind is the accepted design race (identical to the rerun
	// handler's): the turn must not run under holdCtx, since the handoff
	// deliberately retires it.
	if err := checkHoldCanceled(); err != nil {
		hookExitReason = "cancelled"
		return nil, err
	}
	go runAgentTurnRecovered(ctx, sess.ID, prompt, runFn, done)

	messageEvents := app.Messages.Subscribe(ctx)
	messageReadBytes := make(map[string]int)
	seenToolCalls := make(map[string]bool)
	toolCallCounts := make(map[string]int)    // name → count, for JSON output
	printedFinal := make(map[string]bool)     // for terse mode: print once per finished assistant msg
	var finalText string                      // last assistant FullText seen, for JSON output
	var finalReason string                    // last assistant Finish.Reason seen, for JSON output
	var finalErrTitle, finalErrDetails string // Finish.Message + Finish.Details, surfaced into envelope.Error when reason=error
	var printed bool

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(stderr, ansi.ResetProgressBar)
		}

		// JSON mode emits its own trailing newline via json.Encoder; the
		// terse/stream modes need a bare \n so a follow-up shell prompt
		// doesn't overwrite the last token.
		if mode != RunModeJSON {
			_, _ = fmt.Fprintln(stdout)
		}
	}()

	// finish builds the final envelope/error from runErr plus whatever
	// finalText/finalReason/toolCallCounts have accumulated via messageEvents
	// so far, and is the sole return point for a completed run. Extracted
	// (task #421/P0-1) from the body of `case result := <-done:` below so
	// BOTH that case AND drainDone's case (a durable continuation's outcome,
	// possibly arriving well after the original done fired) can reach it —
	// see the select loop's own doc for why this split exists.
	finish := func(runErr error) (*RunResult, error) {
		stopSpinner()
		isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

		if mode == RunModeJSON {
			// Re-fetch the session row so the usage delta reflects
			// the writes the agent made during the run.
			freshSess, _ := app.Sessions.Get(ctx, sess.ID)
			// Fork patch (orchestrator UX): when the caller asked
			// for JSON, defang the persistent "model wrapped its
			// final JSON in a ```json fence and added prose" case
			// here so wrappers can pipe final_text straight into
			// jq. The original is preserved in assistant_notes.
			//
			// Fork patch (orchestrator UX): stripAndExtractJSON handles
			// the common fast-model failure mode: prose preamble + JSON,
			// or even multiple JSON values separated by prose (observed
			// with GLM-5-turbo). Returns a wrapped JSON array when N≥2
			// valid values are found, a single value for N=1, and
			// ErrInvalidStripJSON for N=0 (original text preserved in
			// final_text so the orchestrator can inspect what the model
			// actually said).
			finalTextOut := finalText
			assistantNotes := ""
			strippedBytes := 0
			stripErr := ""
			stripErrReason := ""
			if overrides.StripJSONFences && finalReason != "error" && finalReason != "canceled" {
				cleaned, notes, vErr := stripAndExtractJSON(finalText)
				finalTextOut = cleaned
				assistantNotes = notes
				strippedBytes = len(finalText) - len(cleaned)
				if strippedBytes < 0 {
					strippedBytes = 0
				}
				if vErr != nil {
					stripErr = vErr.Error()
					stripErrReason = "invalid_json"
				}
			}
			// Fork patch (orchestrator UX): sub-agent aggregation.
			// session-#3 (2026-05-17) feedback measured a 7×
			// reduction where parent collapsed sub-agent outputs
			// into a one-paragraph wrap-up. Two responses:
			//
			// 1. ALWAYS-ON warning when reduction ratio is bad
			//    (≥3 sub-agents emitted output AND final_text is
			//    <40% of their combined chars). Operator sees it
			//    in envelope.warnings without flipping a flag.
			// 2. OPT-IN --aggregation=attach: collect each
			//    sub-agent's last assistant text into
			//    envelope.SubAgentOutputs so the orchestrator
			//    recovers the lost detail.
			var subOutputs []SubAgentOutput
			var reductionWarning string
			subAgentCalls := toolCallCounts["agent"] + toolCallCounts["agentic_fetch"]
			if subAgentCalls > 0 {
				count, totalChars := app.subAgentSummaryStats(ctx, sess.ID)
				if count >= 2 && totalChars > 0 {
					parentChars := len(finalTextOut)
					ratio := float64(parentChars) / float64(totalChars)
					if ratio < 0.4 {
						reductionWarning = fmt.Sprintf(
							"reduction-loss: final_text is %d chars (%.0f%% of %d combined sub-agent chars across %d sub-session(s)). The parent likely summarised away detail. Re-run with --aggregation=attach or --aggregation=concat to recover; or query the sub-sessions directly.",
							parentChars, ratio*100, totalChars, count,
						)
					}
				}
			}
			if overrides.AggregationMode == "attach" {
				subOutputs = app.collectSubAgentOutputs(ctx, sess.ID)
			}
			summary := buildRunResult(
				sess.ID, finalTextOut, assistantNotes, finalReason, runErr, isCanceled,
				toolCallCounts,
				freshSess.PromptTokens+freshSess.CompletionTokens-tokensBefore,
				freshSess.Cost-costBefore,
				time.Since(runStart),
				finalErrTitle, finalErrDetails,
				strippedBytes, stripErr, stripErrReason,
				subOutputs, reductionWarning,
			)
			// Per-message token/cache accounting for the session (task
			// #480). Best-effort: an orchestrator losing statistics must
			// never turn a successful run into a failed one.
			if report, uErr := app.Messages.UsageBySession(ctx, sess.ID); uErr != nil {
				slog.Warn("run: failed to read per-message usage for the JSON envelope", "session", sess.ID, "err", uErr)
			} else {
				summary.Usage.Session = buildSessionUsageInfo(report)
			}
			// Fork patch: batch 8 — surface orphan partial text.
			if partial := app.findOrphanPartial(ctx, sess.ID); partial != nil {
				summary.RecoveredPartial = partial
				summary.Warnings = append(summary.Warnings, fmt.Sprintf(
					"recovered %d chars of partial assistant text from session %s — model run was interrupted",
					partial.Chars, sess.ID,
				))
			}
			hookExitReason = summary.ExitReason
			if runFailed(finalReason, runErr, isCanceled) {
				return &summary, &runIncompleteError{reason: summary.ExitReason, detail: summary.Error, cause: runErr}
			}
			return &summary, nil
		}

		if runErr != nil {
			if guidance := sessionBusyGuidance(sess.ID, runErr); guidance != "" {
				slog.Warn("Non-interactive run rejected because session is already locked",
					"session_id", sess.ID,
					"guidance", guidance,
					"err", runErr)
				fmt.Fprintf(stderr, "\n%s\n\n", guidance)
			}
			// Peak-hours refusal carries multiline orchestrator
			// guidance (RESUME AT + don't-retry instructions) that
			// fang's ERROR box truncates at the first newline. Print
			// the guidance to stderr separately BEFORE the ERROR box
			// so the operator / orchestrator actually sees it.
			// Reuses agent.PeakHoursGuidance so the stderr text stays
			// identical to the DB finish-message details recorded by
			// peakHoursStoppedFinishText (sessions why / diff, etc.).
			var peakErr *agent.PeakHoursError
			if errors.As(runErr, &peakErr) {
				fmt.Fprintf(stderr, "\n%s\n\n", agent.PeakHoursGuidance(peakErr))
			}
			if isCanceled {
				slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
				hookExitReason = "cancelled"
				return nil, cancelledRunError(runErr, finalReason, finalErrTitle, finalErrDetails)
			}
			hookExitReason = "error"
			return nil, fmt.Errorf("agent processing failed: %w", runErr)
		}
		// runErr == nil, but the turn may still have ended in-band on an
		// error / canceled / max_tokens finish — not a clean completion,
		// so exit non-zero (the final text is already on stdout).
		if runFailed(finalReason, runErr, isCanceled) {
			reason := finalReason
			if reason == "" {
				reason = "error"
			}
			hookExitReason = reason
			detail := finalErrTitle
			if finalErrDetails != "" {
				if detail != "" {
					detail += ": "
				}
				detail += finalErrDetails
			}
			return nil, &runIncompleteError{reason: reason, detail: detail}
		}
		hookExitReason = "stop"
		return nil, nil
	}

	// drainDone carries the outcome of a P0-1 durable-continuation drain
	// (see the `case result := <-done` branch below) back into this same
	// select loop, on its OWN turn through the loop rather than synchronously
	// inside done's case body. This matters: DrainSessionNow can take
	// seconds (a real second provider round-trip) and, while it runs, the
	// continuation's OWN assistant messages are published to the same
	// message broker messageEvents is subscribed to — those messages MUST
	// still be read by `case event := <-messageEvents` (that's what updates
	// finalText/finalReason/toolCallCounts, and what streams live output in
	// RunModeStream/Terse) while the drain is in flight. Calling
	// DrainSessionNow synchronously inside done's own case body would block
	// this entire select for the drain's whole duration, starving
	// messageEvents and leaving finalText/finalReason stuck at whatever the
	// CANCELLED first generation had produced — confirmed directly: an
	// earlier, synchronous-in-place version of this fix passed a superficial
	// smoke test but failed a stricter end-to-end regression test
	// (TestRunNonInteractive_P0_1_LiveContinuation) with the continuation's
	// own content never reaching the envelope.
	drainDone := make(chan error, 1)

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			runErr := result.err
			isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

			// P0-1 fix (task #421): a cross-process interrupt landing on a
			// busy session (rush sessions inject --interrupt) cancels the
			// in-flight generation and durably enqueues its replacement
			// (handleInterruptTick), deliberately WITHOUT a live mailbox
			// handoff — the durable run_queue row is the only remaining
			// owner (see mailbox.go's FromDurableQueue guard). Without this,
			// that row sits pending until the background RunQueuePump's
			// next tick (3s in production) happens to fire before this
			// process exits — a race this short-lived process routinely
			// loses, since the rest of this select fires within
			// milliseconds of the cancellation. DrainSessionNow runs any
			// such pending continuation to completion, in THIS process,
			// before the envelope is built from what would otherwise be a
			// stale, cancelled-generation result.
			//
			// isCanceled gates this deliberately: DrainSessionNow itself is
			// a no-op (DrainNoWork) when nothing is pending, so a plain
			// user/--timeout cancellation with no durable continuation is
			// unaffected — the drainDone case below restores the ORIGINAL
			// runErr in that case, rather than fabricating a success.
			//
			// Runs in its OWN goroutine (see drainDone's doc above for why
			// synchronous-in-place doesn't work) — this select loop keeps
			// servicing messageEvents (and ctx.Done()) the whole time.
			if isCanceled && app.RunQueuePump != nil {
				go func(originalErr error) {
					result, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sess.ID)
					drainDone <- drainOutcomeError(sess.ID, result, drainErr, originalErr)
				}(runErr)
				continue
			}

			return finish(runErr)

		case drainErr := <-drainDone:
			return finish(drainErr)

		case event, ok := <-messageEvents:
			if !ok {
				// H-2 (task #779): app.Messages.Subscribe's channel closes
				// either when ctx is cancelled OR when the broker itself is
				// closed independently of ctx (see pubsub.Broker.Close /
				// Broker.Subscribe). A receive on a closed channel succeeds
				// immediately and forever with a zero-value event, so
				// without this guard this branch would spin hot on
				// zero-value events until (or unless) ctx.Done() happened
				// to be the one picked by `select` — and if the broker
				// closed independently of ctx, ctx.Done() might never fire
				// at all, so the loop would never terminate. Nil-ing the
				// local channel variable makes this case block forever
				// from here on, so `select` falls through cleanly to the
				// `done`/`drainDone`/`ctx.Done()` cases instead of spinning
				// — the other cases still decide when the loop actually
				// exits.
				messageEvents = nil
				if messageEventsClosedSeam != nil {
					messageEventsClosedSeam()
				}
				continue
			}
			msg := event.Payload
			if msg.SessionID == sess.ID && msg.Role == message.Assistant && len(msg.Parts) > 0 {
				stopSpinner()

				// Tool-call names always go to stderr — one short line per
				// new call. This gives wrappers and humans a heartbeat
				// without exposing inputs / outputs.
				for _, p := range msg.Parts {
					if tc, ok := p.(message.ToolCall); ok && tc.Name != "" && !seenToolCalls[tc.ID] {
						seenToolCalls[tc.ID] = true
						toolCallCounts[tc.Name]++
						prefix := ""
						if stderrTTY {
							prefix = "\r" + ansi.EraseEntireLine
						}
						fmt.Fprintf(stderr, prefix+"▶ %s\n", tc.Name)
					}
				}

				// Track final state for JSON mode regardless of which
				// output mode is active — JSON output materialises after
				// the run completes, so we accumulate as we go.
				if msg.IsFinished() {
					finalText = msg.FullText()
					for _, p := range msg.Parts {
						if f, ok := p.(message.Finish); ok {
							finalReason = string(f.Reason)
							finalErrTitle = f.Message
							finalErrDetails = f.Details
							break
						}
					}
				}

				switch mode {
				case RunModeJSON:
					// Suppress per-message stdout entirely; the summary is
					// printed below after `done` fires.
				case RunModeTerse:
					if !msg.IsFinished() || printedFinal[msg.ID] {
						continue
					}
					text := strings.TrimLeft(msg.FullText(), " \t\n")
					if text != "" {
						printedFinal[msg.ID] = true
						printed = true
						fmt.Fprint(stdout, text)
					}
				case RunModeStream:
					content := msg.FullText()
					readBytes := messageReadBytes[msg.ID]
					if len(content) < readBytes {
						slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
						return nil, fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
					}
					part := content[readBytes:]
					if readBytes == 0 {
						part = strings.TrimLeft(part, " \t")
					}
					if printed || strings.TrimSpace(part) != "" {
						printed = true
						fmt.Fprint(stdout, part)
					}
					messageReadBytes[msg.ID] = len(content)
				}
			}

		case <-ctx.Done():
			stopSpinner()
			hookExitReason = "cancelled"
			return nil, ctx.Err()
		}
	}
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes. It is a thin
// wrapper over ExecuteRun: it supplies the process streams as defaults
// and renders the JSON envelope for RunModeJSON.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	summary, err := app.ExecuteRun(ctx, RunRequest{
		Prompt:            prompt,
		Overrides:         overrides,
		Mode:              mode,
		ContinueSessionID: continueSessionID,
		UseLast:           useLast,
		Origin:            overrides.Origin,
		Stdout:            output,
		Stderr:            os.Stderr,
		HideSpinner:       hideSpinner,
	})
	if mode == RunModeJSON && summary != nil {
		enc := json.NewEncoder(output)
		if encErr := enc.Encode(summary); encErr != nil {
			return fmt.Errorf("failed to encode JSON result: %w", encErr)
		}
	}
	return err
}
