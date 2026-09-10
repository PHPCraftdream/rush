package app

import (
	"io"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
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
