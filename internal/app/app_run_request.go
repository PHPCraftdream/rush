package app

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
// Model and reasoning-effort overrides are invocation-only: they do not
// replace the session's saved model or reasoning-effort preferences.
type RunOverrides struct {
	SmartModel   string // "model" or "provider/model"; overrides selected large
	FastModel    string // same as SmartModel, for the fast slot
	SystemPrompt string // persisted on the session (Sessions.UpdateSystemPrompt)
	// ReasoningEffort applies to whichever slot is active for this run.
	ReasoningEffort string
	RoleSmart       bool
	// ModelRole is the built-in policy-driving slot for this invocation.
	// Custom profile selection uses RequestedRole and never enters this field.
	ModelRole config.SelectedModelType
	// RequestedRole selects an exact custom role profile, case-sensitively.
	RequestedRole string
	// roleContract is the immutable contract snapshot shared only within this
	// invocation's turns. A new Run resolves from current configuration.
	roleContract *session.RoleContract

	// Fork patch (orchestrator UX): DisableSubAgents drops the `agent`
	// and `agentic_fetch` tools from the coder agent for this run so a
	// `rush run --agents single` invocation cannot fan out. Mutation
	// is per-process — `rush run` is single-shot, so the change does
	// not leak across invocations. StripJSONFences asks
	// RunNonInteractive to post-process the envelope's final_text
	// (markdown fence + prose preamble removal); the unstripped
	// original is preserved in RunResult.AssistantNotes.
	DisableSubAgents bool
	// MaxAgentDepth narrows this invocation's delegation budget; nil uses default 2.
	MaxAgentDepth   *int
	StripJSONFences bool
	// NoSupervision disables the periodic root-session supervision check-in
	// (design doc §7) for this run (`rush run --no-supervision`). false
	// (default) defers to config's supervision_enabled (on by default).
	NoSupervision bool
	// SupervisionInterval overrides the check-in's silence interval
	// (`rush run --supervision-interval`). Zero = no override.
	SupervisionInterval time.Duration
	// BackgroundJobDefaultTimeout is the opt-in default terminate_and_wake
	// deadline for background bash/run_command jobs that pass no explicit
	// timeout (`rush run --job-timeout`). Zero = off.
	BackgroundJobDefaultTimeout time.Duration
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
	ReportRetries     int
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
	// IdleTimeout overrides the stream watchdog's idle-stall threshold
	// for this call (agent.CallOptions.IdleTimeout) and makes a stall
	// terminal instead of transparently retried — see that field's doc.
	// `rush run --idle-timeout` (default 15m) sets this; 0 means "no
	// override for this call" (agent.buildReviewerPassTurn's follow-up
	// turn leaves it unset on purpose, see below).
	IdleTimeout time.Duration
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
	// FolderScopes, when non-empty, replaces this run's default coder
	// toolset with the scoped, batch-capable fs_* family for THIS call:
	// the legacy single-target file tools and the escape-hatch tools are
	// stripped, only the fs_* tools whose operation the scope grants
	// survive, and every fs_* item is checked against the scope per item
	// (an out-of-scope item is a per-item denial inside the batch, not a
	// whole-call error). Unlike SmartModel/SystemPrompt this is per-call
	// only and NEVER persisted on the session — a later run on the same
	// session without it is unscoped again. Refused outright (hard error,
	// before any provider traffic) when the resolved provider is a CLI
	// provider, because those run file tools inside a subprocess that
	// cannot see the scope (T9, coordinator's
	// rejectScopedCallOnCLIProvider). Entries are compiled with
	// permission.BuildFolderScope; any malformed entry fails the whole
	// run. Fork patch (SDK folder scopes, T10).
	FolderScopes []permission.FolderScopeEntry
	// DiskProvider, when non-nil, replaces the real filesystem for THIS
	// run's fs_* tools (fs_read, fs_list, fs_find, fs_grep, fs_write,
	// fs_replace, fs_write_lines, fs_delete) and for the path resolution
	// their folder-scope checks run on. Per-call only and NEVER
	// persisted on the session, exactly like FolderScopes above.
	//
	// Two hard errors, both before any provider traffic: (1) a non-nil
	// DiskProvider with an empty FolderScopes — without a scope the
	// legacy single-target file tools (view/write/edit/multiedit/glob/
	// grep/ls) stay in the toolset and can silently touch the REAL disk
	// in the same turn a virtual one is being used; (2) a non-nil
	// DiskProvider combined with a scope that keeps command-executing
	// tools (RestrictedRun with bash patterns granted) — bash would see
	// the real disk while fs_* sees the virtual one, an invisible split
	// the model has no way to know about.
	//
	// A run carrying one is refused outright if it would be durably
	// queued (a caller-supplied Go value cannot be serialized onto a
	// run-queue row) — see agent.ErrDiskProviderNotDurable. Fork patch
	// (SDK disk provider, T13/#859).
	DiskProvider tools.DiskProvider
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
	// onSessionResolved lets the CLI wait for jobs even if this turn later fails,
	// and claims the session's durable driver marker. It runs right after the
	// session resolves, before draining pending work and before any session
	// write, so a non-nil error (the session is driven by another live loop)
	// aborts the run having changed nothing.
	onSessionResolved func(string) error
	// captureResult returns a structured outcome even in terse or stream mode.
	captureResult bool
	// loopTurn marks every turn the `rush run` loop issues (first turn, Drains,
	// reviewer pass): ExecuteRun does not write ended_reason for it, because the
	// column is empty while a run is in progress and the loop's exit is the only
	// writer (persistEndedReason, R8A-1). Callers that are not the loop keep the
	// per-turn write.
	loopTurn bool
	// drainTurn marks a `rush run` loop Drain iteration: mutation-free setup (the
	// invocation's own setup ran once with the first turn) and no ended_reason
	// write when it ran no turn.
	drainTurn bool
	// reviewerTurn makes this call the automatic reviewer pass and nothing else:
	// the `rush run` loop issues it once, from its scope-closed exit, as an
	// ordinary (non-Drain) turn on the session it already claimed. Setup is
	// mutation-free like a Drain's.
	reviewerTurn bool
	// nudgeTurn marks the loop's unfinished-todos reminder turn: a follow-up
	// of the same invocation, so its setup is mutation-free like a Drain's.
	nudgeTurn bool
	// reportTurn marks the loop's report-recovery turn: a follow-up of the
	// same invocation, so its setup is mutation-free like a Drain's.
	reportTurn bool
	// deferReviewer tells ExecuteRun not to run the reviewer pass itself: the
	// `rush run` loop runs it once when its scope closes (reviewerTurn).
	deferReviewer bool
	// reviewerConfig makes a Drain run on the reviewer pass's call options and
	// model (buildReviewerPassTurn): set for the loop's Drains after its reviewer
	// turn, which react to the async work that turn started.
	reviewerConfig bool
	reviewerReport string
	// reviewBasis is the deterministic evidence the reviewer pass needs: the
	// run's start time, prompt, working dir and a git snapshot taken before the
	// first turn. Captured once by the first turn and threaded to the loop's
	// reviewer turn, because ExecuteRun's mutation-free follow-up calls
	// (reviewerTurn/Drain) do not re-run the first turn's setup. Nil for every
	// call that is not a `rush run` loop's first turn.
	reviewBasis *reviewBasis
	// onTurnSubmitted runs right before the turn is launched: every setup step
	// that can fail has passed. It lets the loop tell a first turn that never
	// reached the model from one that ran and failed.
	onTurnSubmitted func()

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

// mutationFree reports a follow-up call of a `rush run` invocation (a Drain
// iteration, the loop's reviewer pass, todo reminder or report recovery): the
// invocation's own session setup --
// system prompt, reasoning effort, model slots, cancel flag, budget,
// ended_reason -- ran once with the first turn and is not repeated.
func (r RunRequest) mutationFree() bool {
	return r.drainTurn || r.reviewerTurn || r.nudgeTurn || r.reportTurn
}

type terminalReconciliation struct {
	message   message.Message
	toolCalls map[string]int
	// toolCallByID maps each distinct tool-call ID in this phase's rows
	// to its tool name (F8, 2026-09-22 audit): the run-wide tool
	// inventory merges the phases' reconciliations by ID, so a call
	// listed in both phases is still counted once.
	toolCallByID map[string]string
	// toolCallsNoID counts this phase's tool calls that carry no ID and
	// therefore cannot be ID-deduplicated across phases; each phase
	// contributes them as reported.
	toolCallsNoID map[string]int
	// combinedText, when non-empty, is the terminal message's text
	// prefixed with the texts of the failed assistant attempts a
	// successful continuation chain resumed from (see
	// continuationChainText). finish() prefers it over the terminal
	// message's own FullText so a run whose answer was interrupted
	// mid-stream and then completed returns both halves, not just the
	// continuation's tail.
	combinedText string
}

// reconcileTerminalMessage reads the committed assistant rows for this run.
// The baseline ID set fences out messages that existed before the turn,
// including rows created in the same second as the run.
//
// ownID, when non-empty, additionally restricts which row may become the
// terminal to the exact assistant message THIS call's own runInternal
// invocation produced (R8-3, 2026-09-22 audit): without it, a committed A
// that releases its session before its own reconciliation runs can have a
// legitimately later B's answer on the same session picked up instead,
// since both rows are equally "new" relative to A's baseline. ownID is the
// additional filter; baseline stays in effect either way. Empty ownID is
// accepted only for a durable continuation with explicit queue ownership.
func reconcileTerminalMessage(
	env RunEnv,
	ctx context.Context,
	sessionID string,
	baselineIDs map[string]struct{},
	baselineKnown bool,
	runStart time.Time,
	ownID string,
	ownIDs map[string]struct{},
) (terminalReconciliation, error) {
	messages, err := env.Messages().List(ctx, sessionID)
	if err != nil {
		return terminalReconciliation{}, fmt.Errorf("list run messages: %w", err)
	}

	var (
		terminal message.Message
		found    bool
		calls    = make(map[string]int)
		seen     = make(map[string]struct{})
		byID     = make(map[string]string)
		noID     = make(map[string]int)
		// runMessages collects this run's rows in commit order —
		// assistant steps, the coordinator's continuation prompts
		// between them, and tool results — so the continuation chain
		// walk below can tell a same-attempt tool-loop step apart from
		// a separate turn's boundary.
		runMessages []message.Message
		// pastOwn tracks whether this call's own row (ownID) has already
		// been observed (R8-3 residual, 2026-09-22 round-9 audit). A row
		// created AFTER this call's own terminal row cannot belong to
		// this call -- this call's own dispatcher returns as soon as it
		// writes that row -- so it must belong to a later, independent
		// caller on the same session. Without this, tool calls from that
		// later caller's own turn were folded into THIS reconciliation's
		// inventory even though the ownID filter above already correctly
		// keeps its FinalText out.
		pastOwn bool
		toolIDs = make(map[string]struct{})
	)
	for _, msg := range messages {
		if !isRunMessage(msg, baselineIDs, baselineKnown) {
			continue
		}
		if ownID == "" {
			continue
		}
		if pastOwn {
			continue
		}
		if msg.Role != message.Assistant {
			switch msg.Role {
			case message.Tool:
				for _, part := range msg.Parts {
					if result, ok := part.(message.ToolResult); ok {
						if _, belongs := toolIDs[result.ToolCallID]; belongs {
							runMessages = append(runMessages, msg)
							delete(toolIDs, result.ToolCallID)
						}
					}
				}
			case message.User:
				if agent.IsContinuationPrompt(msg.FullText()) {
					runMessages = append(runMessages, msg)
				}
			}
			continue
		}
		if _, owned := ownIDs[msg.ID]; !owned {
			toolIDs = make(map[string]struct{})
			continue
		}
		runMessages = append(runMessages, msg)
		for _, call := range msg.ToolCalls() {
			if call.ID != "" {
				toolIDs[call.ID] = struct{}{}
			}
			if call.ID != "" {
				if _, ok := seen[call.ID]; ok {
					continue
				}
				seen[call.ID] = struct{}{}
				byID[call.ID] = call.Name
			} else {
				noID[call.Name]++
			}
			calls[call.Name]++
		}
		if msg.IsFinished() && msg.ID == ownID {
			terminal = msg
			found = true
		}
		if msg.ID == ownID {
			pastOwn = true
		}
	}
	if !found {
		return terminalReconciliation{toolCalls: calls, toolCallByID: byID, toolCallsNoID: noID}, fmt.Errorf(
			"no committed terminal assistant message found for run started at %s",
			runStart.UTC().Format(time.RFC3339),
		)
	}
	return terminalReconciliation{
		message:       terminal,
		toolCalls:     calls,
		toolCallByID:  byID,
		toolCallsNoID: noID,
		combinedText:  continuationChainText(runMessages, terminal),
	}, nil
}

// continuationChainText combines a successful terminal assistant message
// with the failed attempts it resumed from. The coordinator's
// continuation retry (runInternal) deliberately leaves each failed
// attempt's partial text in its own history row and asks the model to
// continue in a fresh message, so the run's full answer is spread across
// several rows and the terminal row alone carries only the tail.
//
// The walk starts at the row before the terminal and moves backward over
// whole messages (not just assistants) so it can recognize the boundaries
// it crosses:
//   - a tool result row belongs to the tool-loop step of the attempt
//     being walked and is skipped without breaking the chain;
//   - a clean assistant row is skipped the same way only while it is a
//     tool-loop step (it carries the tool calls, or ends on
//     FinishReasonToolUse) — any other clean row is a completed turn and
//     stops the walk;
//   - a user row is a turn boundary. Only the coordinator's own
//     continuation prompt (agent.IsContinuationPrompt, matched against
//     the producer agent's continuationPrompt verbatim) keeps the chain
//     alive across it; a genuine user turn — including this run's own
//     original prompt — stops the walk, so text is never combined across
//     unrelated messages. A failed attempt's text joins the chain; a
//     tool step's narration does not. An attempt whose text is empty
//     or whitespace-only joins the chain too (R2-2, 2026-09-22
//     audit): the join is byte-exact, so a lone space cut between two
//     attempts is real content, not noise to strip.
//
// A terminal message that itself ended in error returns "": the run
// failed, and there is no successful continuation to attach anything to.
func continuationChainText(runMessages []message.Message, terminal message.Message) string {
	if fp := terminal.FinishPart(); fp != nil && fp.Reason == message.FinishReasonError {
		return ""
	}
	termIdx := -1
	for i, msg := range runMessages {
		if msg.ID == terminal.ID {
			termIdx = i
			break
		}
	}
	if termIdx < 0 {
		return ""
	}
	var parts []string
collect:
	for i := termIdx - 1; i >= 0; i-- {
		msg := runMessages[i]
		switch msg.Role {
		case message.Tool:
			// A tool result of the attempt being walked.
		case message.User:
			if !agent.IsContinuationPrompt(msg.FullText()) {
				break collect
			}
		case message.Assistant:
			if fp := msg.FinishPart(); fp != nil && fp.Reason == message.FinishReasonError {
				parts = append(parts, msg.FullText())
				continue
			}
			if len(msg.ToolCalls()) == 0 && msg.FinishReason() != message.FinishReasonToolUse {
				break collect
			}
		default:
			break collect
		}
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Reverse(parts)
	combined := ""
	for _, part := range parts {
		combined = joinContinuationText(combined, part)
	}
	return joinContinuationText(combined, terminal.FullText())
}

// joinContinuationText appends next to acc byte-for-byte. The fragments
// are pieces of ONE answer stream that a transient failure cut at an
// arbitrary byte offset, and the coordinator's continuation prompt tells
// the model to continue exactly where it left off, so whatever belonged
// at the cut — a space, a paragraph break, nothing at all mid-word — is
// the continuation's own first bytes, not something to synthesize here.
// A cut with no whitespace on either side is the common mid-token or
// mid-structure case: partial `{"text":"hel` plus continuation `lo"}`
// must become `{"text":"hello"}`, and a guessed `"\n\n"` corrupts the
// payload into invalid JSON (R2-2 / F3, 2026-09-22 audit). Nothing
// observable distinguishes a paragraph-boundary cut from a mid-token cut
// — a stalled stream carries the same error finish either way — so no
// separator heuristic can be reliable. Where a break is genuinely lost
// because the model did not re-emit one after a boundary cut, the cost
// is cosmetic; an injected separator is corruption.
func joinContinuationText(acc, next string) string {
	return acc + next
}

func isRunMessage(
	msg message.Message,
	baselineIDs map[string]struct{},
	baselineKnown bool,
) bool {
	if baselineKnown {
		_, existed := baselineIDs[msg.ID]
		return !existed
	}
	// Without the identity snapshot, a same-second older row cannot be
	// distinguished safely. Let the live-event fallback carry the result.
	return false
}
