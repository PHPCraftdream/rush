// The non-interactive run path: run modes and per-invocation overrides,
// session resolution, the panic-isolated agent-turn wrapper, and the
// RunNonInteractive event loop that streams output and emits the final
// envelope.

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
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/format"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

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
// the agent runs, so a subsequent `crush run --session <same>` without
// those flags continues with the same overrides. Empty fields are
// left alone (they don't reset what's already on the session).
type RunOverrides struct {
	LargeModel   string // "model" or "provider/model"; overrides selected large
	SmallModel   string // same as LargeModel, for the small slot
	SystemPrompt string // persisted on the session (Sessions.UpdateSystemPrompt)
	// ReasoningEffort applies to whichever slot is "active" for this run —
	// the large one if RoleLarge is true, the small one otherwise. Persisted
	// via Sessions.UpdateReasoningEffort.
	ReasoningEffort string
	RoleLarge       bool
	// ModelRole is the resolved --role slot for this invocation (large,
	// small, worker, reviewer). "" (e.g. non-`crush run` paths) is treated
	// as smart/large by the coordinator. Threaded through to
	// AgentCoordinator.SetActiveModelRole so sub-agent spawns can decide
	// whether to prefer the cheaper Worker slot instead of blindly
	// inheriting the parent's Large model. Fork patch (reviewer/worker
	// roles).
	ModelRole config.SelectedModelType
	// Fork patch (orchestrator UX): DisableSubAgents drops the `agent`
	// and `agentic_fetch` tools from the coder agent for this run so a
	// `crush run --agents single` invocation cannot fan out. Mutation
	// is per-process — `crush run` is single-shot, so the change does
	// not leak across invocations. StripJSONFences asks
	// RunNonInteractive to post-process the envelope's final_text
	// (markdown fence + prose preamble removal); the unstripped
	// original is preserved in runResult.AssistantNotes.
	DisableSubAgents bool
	StripJSONFences  bool
	// AggregationMode controls how sub-agent fan-out output reaches
	// the orchestrator. "" / "summary" = upstream default (parent
	// composes a wrap-up, sub-agent details live in the DB only).
	// "concat" = the user prompt carries a nudge asking the parent to
	// include each sub-agent's reply verbatim in final_text. "attach"
	// = after Run the app collects each sub-session's last assistant
	// text into runResult.SubAgentOutputs so the orchestrator gets
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
	// for this single invocation. `crush run --allow-peak-hours` sets it.
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
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
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
		created, createErr := app.Sessions.CreateWithID(ctx, continueSessionID, continueSessionID)
		if createErr != nil {
			return session.Session{}, fmt.Errorf("session %q not found and could not be created: %w", continueSessionID, createErr)
		}
		slog.Info("Created session on demand from --session id", "session_id", created.ID)
		return created, nil

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
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
// coordinator.go's runSubAgent) — would crash the entire crush.exe process
// via Go's default panic handler, which writes only to os.Stderr and never
// through slog. For `crush run` invocations whose stderr isn't captured by
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
				err: fmt.Errorf("agent turn panicked (recovered, see crush.log for stack trace): %v", r),
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
		done <- agentTurnResponse{
			err: fmt.Errorf("failed to start agent processing stream: %w", agent.ErrSessionBusy),
		}
		return
	}
	done <- agentTurnResponse{
		result: result,
	}
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	largeModel := overrides.LargeModel
	smallModel := overrides.SmallModel
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

	if largeModel != "" || smallModel != "" {
		if err := app.overrideModelsForNonInteractive(ctx, largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
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
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// Fork patch (orchestrator UX): --agents single. Drop the agent /
	// agentic_fetch tools from the coder agent's AllowedTools BEFORE
	// UpdateModels rebuilds the toolset so the model literally cannot
	// fan out. Mutation is in-process only (crush run is a single-shot
	// process — exit drops the change), so this is safe even though
	// it touches the global config. See run.go and run_format.go.
	//
	// Plan phase 2 exception: when this run is --role smart with a Worker
	// model configured, restore ONLY the `agent` tool — see
	// shouldBypassSubAgentBan. This applies even if the operator passed
	// `--agents single` explicitly: a configured worker means delegation
	// is the intended workflow. `agentic_fetch` stays stripped either
	// way: it's a separate concern (web-fetch delegation that always
	// runs on the small model, see
	// internal/agent/agentic_fetch_tool.go's "Use small model for both"
	// comment) and has nothing to do with delegating hands-on work to a
	// worker.
	if overrides.DisableSubAgents {
		if shouldBypassSubAgentBan(overrides.ModelRole, app.config.Config()) {
			app.disableToolsInConfig([]string{"agentic_fetch"})
		} else {
			app.disableSubAgentToolsInConfig()
		}
	}

	// Fork patch (reviewer/worker roles): record which named model slot is
	// driving this top-level run so sub-agent spawns (coordinator's
	// buildAgentModels) can decide whether to prefer the cheaper Worker
	// slot. Called unconditionally (even for ModelRole == "") so a resumed
	// session without going through this code path again still gets a
	// defined value.
	app.AgentCoordinator.SetActiveModelRole(overrides.ModelRole)

	// force update of agent models before running so mcp tools are loaded
	app.AgentCoordinator.UpdateModels(ctx)

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to create session for non-interactive mode: %w", err)
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
	if systemPrompt != "" {
		if err := app.Sessions.UpdateSystemPrompt(ctx, sess.ID, systemPrompt); err != nil {
			return fmt.Errorf("failed to set system prompt for session: %w", err)
		}
	}

	// Persist reasoning effort onto the active slot. We pass the current
	// stored value for the *other* slot through so we don't clobber it —
	// UpdateReasoningEffort takes both fields as a single transaction.
	if overrides.ReasoningEffort != "" {
		large := sess.LargeModelReasoningEffort
		small := sess.SmallModelReasoningEffort
		if overrides.RoleLarge {
			large = overrides.ReasoningEffort
		} else {
			small = overrides.ReasoningEffort
		}
		if err := app.Sessions.UpdateReasoningEffort(ctx, sess.ID, large, small); err != nil {
			return fmt.Errorf("failed to set reasoning effort: %w", err)
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	app.Permissions.AutoApproveSession(sess.ID)

	// Fork patch (run allowlist): (re)arm the restricted-run allowlist
	// by merging the config-derived spec with this invocation's CLI
	// overrides. Even when no override is passed we rebuild from config
	// so the gate stays consistent with whatever permissions.run was on
	// disk at run time. SetRunAllowlist only affects the auto-approve
	// path exercised above; interactive sessions never run this code.
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
	app.Permissions.SetRunAllowlist(compiled)

	// Fork patch: batch 8 — wire per-invocation timeout extension flags to
	// the coordinator's agent before the run starts.
	if overrides.TimeoutExtendsOnProgress || overrides.TimeoutHardCap > 0 {
		app.AgentCoordinator.SetAgentTimeoutOptions(
			overrides.TimeoutExtendsOnProgress,
			overrides.TimeoutHardCap,
		)
	}

	// Fork patch: batch 30 — clear stale cancel flag and set run limits.
	if err := app.Sessions.ClearCancelRequest(ctx, sess.ID); err != nil {
		slog.Warn("Failed to clear cancel request flag", "session_id", sess.ID, "err", err)
	}
	if overrides.MaxCost > 0 || overrides.MaxTokens > 0 {
		app.AgentCoordinator.SetRunLimits(overrides.MaxCost, overrides.MaxTokens)
	}

	// Fork patch (peak-hours bypass): arm the one-shot flag before Run so
	// runInternal's checkPeakHours gate is skipped for this invocation.
	if overrides.AllowPeakHours {
		app.AgentCoordinator.SetAllowPeakHours(true)
	}

	// Fork patch (operator UX): persist budget at run start so
	// `sessions show` / `sessions locks` can display "cost vs limit".
	// Also clear ended_reason since the session is being (re)started.
	if err := app.Sessions.SetBudget(ctx, sess.ID, overrides.MaxCost, overrides.MaxTokens, int64(overrides.Timeout.Seconds())); err != nil {
		slog.Warn("Failed to persist budget", "session_id", sess.ID, "err", err)
	}
	if err := app.Sessions.SetEndedReason(ctx, sess.ID, ""); err != nil {
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
		if err := app.Sessions.Rename(ctx, sess.ID, autoTitle); err != nil {
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
	defer func() {
		reason := hookExitReason
		if reason == "" {
			reason = "done"
		}
		if setErr := app.Sessions.SetEndedReason(context.Background(), sess.ID, reason); setErr != nil {
			slog.Warn("Failed to persist ended_reason", "session_id", sess.ID, "reason", reason, "err", setErr)
		}
	}()
	if overrides.OnFinishHook != "" {
		defer func() {
			if freshSess, err := app.Sessions.Get(context.Background(), sess.ID); err == nil {
				hookTokens = freshSess.PromptTokens + freshSess.CompletionTokens - tokensBefore
				hookCost = freshSess.Cost - costBefore
			}
			duration := time.Since(runStart)
			runOnFinishHook(overrides.OnFinishHook, sess.ID, hookExitReason, hookCost, hookTokens, duration)
		}()
	}

	done := make(chan agentTurnResponse, 1)

	go runAgentTurnRecovered(ctx, sess.ID, prompt, func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		return app.AgentCoordinator.Run(ctx, sessionID, prompt)
	}, done)

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
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}

		// JSON mode emits its own trailing newline via json.Encoder; the
		// terse/stream modes need a bare \n so a follow-up shell prompt
		// doesn't overwrite the last token.
		if mode != RunModeJSON {
			_, _ = fmt.Fprintln(output)
		}
	}()

	// finish builds the final envelope/error from runErr plus whatever
	// finalText/finalReason/toolCallCounts have accumulated via messageEvents
	// so far, and is the sole return point for a completed run. Extracted
	// (task #421/P0-1) from the body of `case result := <-done:` below so
	// BOTH that case AND drainDone's case (a durable continuation's outcome,
	// possibly arriving well after the original done fired) can reach it —
	// see the select loop's own doc for why this split exists.
	finish := func(runErr error) error {
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
			// the common small-model failure mode: prose preamble + JSON,
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
			var subOutputs []subAgentOutput
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
			enc := json.NewEncoder(output)
			if encErr := enc.Encode(summary); encErr != nil {
				return fmt.Errorf("failed to encode JSON result: %w", encErr)
			}
			// The envelope (incl. exit_reason + error) is already on
			// stdout. Drive the PROCESS exit code off the outcome so
			// orchestrators / CI branch on success without parsing stdout:
			// a clean end_turn exits 0; an in-band error finish (stall,
			// provider error, empty stream), a cancellation/timeout, or a
			// max_tokens truncation exit non-zero.
			if runFailed(finalReason, runErr, isCanceled) {
				return &runIncompleteError{reason: summary.ExitReason, detail: summary.Error}
			}
			return nil
		}

		if runErr != nil {
			if guidance := sessionBusyGuidance(sess.ID, runErr); guidance != "" {
				slog.Warn("Non-interactive run rejected because session is already locked",
					"session_id", sess.ID,
					"guidance", guidance,
					"err", runErr)
				fmt.Fprintf(os.Stderr, "\n%s\n\n", guidance)
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
				fmt.Fprintf(os.Stderr, "\n%s\n\n", agent.PeakHoursGuidance(peakErr))
			}
			if isCanceled {
				slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
				hookExitReason = "cancelled"
				return cancelledRunError(runErr, finalReason, finalErrTitle, finalErrDetails)
			}
			hookExitReason = "error"
			return fmt.Errorf("agent processing failed: %w", runErr)
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
			return &runIncompleteError{reason: reason, detail: detail}
		}
		hookExitReason = "stop"
		return nil
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
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			runErr := result.err
			isCanceled := runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, agent.ErrRequestCancelled))

			// P0-1 fix (task #421): a cross-process interrupt landing on a
			// busy session (crush sessions inject --interrupt) cancels the
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
			// a no-op (drained=false) when nothing is pending, so a plain
			// user/--timeout cancellation with no durable continuation is
			// unaffected — the drainDone case below restores the ORIGINAL
			// runErr in that case, rather than fabricating a success.
			//
			// Runs in its OWN goroutine (see drainDone's doc above for why
			// synchronous-in-place doesn't work) — this select loop keeps
			// servicing messageEvents (and ctx.Done()) the whole time.
			if isCanceled && app.RunQueuePump != nil {
				go func(originalErr error) {
					drained, drainErr := app.RunQueuePump.DrainSessionNow(ctx, sess.ID)
					switch {
					case drainErr != nil:
						// The drain itself failed (ctx expired, a poison
						// entry exceeded max attempts, a DB error) —
						// surface that as the run's outcome; it is more
						// actionable than the stale cancellation it would
						// otherwise replace.
						drainDone <- drainErr
					case drained:
						// The durable continuation ran in this process.
						// Its messages already updated finalText/
						// finalReason/toolCallCounts via the SAME
						// messageEvents subscription this loop keeps
						// servicing — finish() just needs a nil error
						// instead of the original interrupt-induced
						// cancellation.
						drainDone <- nil
					default:
						// Nothing was pending. Restore the original
						// cancellation — it was real, not a P0-1 artifact.
						drainDone <- originalErr
					}
				}(runErr)
				continue
			}

			return finish(runErr)

		case drainErr := <-drainDone:
			return finish(drainErr)

		case event := <-messageEvents:
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
						fmt.Fprintf(os.Stderr, prefix+"▶ %s\n", tc.Name)
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
						fmt.Fprint(output, text)
					}
				case RunModeStream:
					content := msg.FullText()
					readBytes := messageReadBytes[msg.ID]
					if len(content) < readBytes {
						slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
						return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
					}
					part := content[readBytes:]
					if readBytes == 0 {
						part = strings.TrimLeft(part, " \t")
					}
					if printed || strings.TrimSpace(part) != "" {
						printed = true
						fmt.Fprint(output, part)
					}
					messageReadBytes[msg.ID] = len(content)
				}
			}

		case <-ctx.Done():
			stopSpinner()
			hookExitReason = "cancelled"
			return ctx.Err()
		}
	}
}
