package agentcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

const DefaultSessionName = "Untitled Session"

var UserAgent = fmt.Sprintf("Rush/%s (https://github.com/PHPCraftdream/rush)", version.Version)

type SessionAgentCall struct {
	SessionID       string
	Prompt          string
	ProviderOptions fantasy.ProviderOptions
	Attachments     []message.Attachment
	// Origin marks the entry channel this call's user message arrived
	// through (message.OriginCLI/Web/SDK). Stamped by buildCall from the
	// call's context so it survives the InterruptAndSend queued-replacement
	// handoff; SessionAgentCall literals built outside buildCall (sub-agent
	// dispatch) leave it unspecified. Persisted on the created user
	// message by createUserMessage.
	Origin              message.Origin
	AutoResumed         bool
	BackgroundJobNotice bool
	// NoticeKind distinguishes the kind of notice this call's persisted
	// message carries -- see message.Message.NoticeKind's doc for the value
	// set (§5.3bis). "" for every ordinary call (legacy default).
	NoticeKind       string
	MaxOutputTokens  int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	NonInteractive   bool
	// RequestedModelRole and its source do not carry policy authority.
	RequestedModelRole   RequestedModelRole
	RequestedModelSource string
	// In-process capabilities only; durable IDs cannot reconstruct authority.
	FinalizationAuthorities session.AgentFinalizationContext `json:"-"`
	DriverGeneration        uint64                           `json:"-"`
	// SystemPromptOverride, if non-empty, replaces the agent's global system prompt
	// for this single call. Used to apply per-session system prompts from the DB.
	SystemPromptOverride string
	// MaxCost aborts the run if total session cost exceeds this value (0 = no cap).
	MaxCost float64
	// MaxTokens aborts the run if total prompt+completion tokens exceed this value
	// (0 = no cap).
	MaxTokens int64
	// ExistingMessageID, when non-empty, marks this call as referencing a
	// user message that already exists in the DB (created by another process,
	// e.g. `rush sessions inject --interrupt`). The queue-drain path in
	// Run's PrepareStep must then load that message by ID and splice it into
	// the prompt WITHOUT calling createUserMessage — otherwise the operator
	// would see the same message twice in history. Set by
	// QueueExistingMessage on the interrupt path.
	ExistingMessageID string

	// OnUserMessageCreated, if non-nil, is invoked once with the ID of the
	// user message createUserMessage creates for this call. Not invoked
	// when ExistingMessageID is already set (no new message was created).
	// Lets a caller capture the ID without a second DB round trip -- e.g.
	// coordinator_run.go's transient-retry loop sets this on the initial
	// attempt, then feeds the captured ID back in as ExistingMessageID on
	// each retry so a provider hiccup doesn't re-persist the same prompt
	// as a fresh row every attempt.
	// json:"-": in-process callback, never durable-queue-persisted (unlike
	// ExistingMessageID above, session.SessionAgentCallData has no mirror
	// for this field -- a retry always runs in the same process that set
	// it). Also lets code that shortcuts through json.Marshal(SessionAgentCall{...})
	// keep working now that not every field is a plain value type.
	OnUserMessageCreated func(messageID string) `json:"-"`

	// OnAssistantMessageCreated, if non-nil, is invoked once for EVERY
	// assistant message row the turn creates (PrepareStep runs once per
	// step, so a multi-step tool loop fires it once per step; the LAST
	// invocation names the turn's terminal assistant row). Lets a caller
	// identify the exact row THIS call's turn wrote instead of
	// re-deriving it from session-wide state -- coordinator_run.go's
	// transient-retry loop classifies only this row, because the
	// session's last assistant message at classification time can belong
	// to a concurrent caller's turn (R3-1, round 5).
	// json:"-": in-process callback, never durable-queue-persisted (same
	// rationale as OnUserMessageCreated above).
	OnAssistantMessageCreated func(messageID string) `json:"-"`

	// ReplacementHandoff marks a call extracted from mailbox.replacement.
	ReplacementHandoff bool `json:"-"`

	// CompactionContinuation marks an internal continuation of this call's turn.
	CompactionContinuation bool `json:"-"`
	// RecoveryContinuation is an internal same-logical-call continuation after
	// a certified pre-output Codex context rejection; it never creates a user row.
	RecoveryContinuation bool                `json:"-"`
	Recovery             *CodexRecoveryState `json:"-"`
	RecoverySpentTokens  int64               `json:"-"`

	// InjectID, when non-empty, is the ID of a pending_injects row that
	// must be deleted AFTER successful OS lock acquisition. Set by the
	// cross-process interrupt inject path (startDetachedRun) to
	// prevent data loss if the detached run loses the OS lock race.
	// P0-2 fix.
	InjectID string

	// SmartModel/FastModel/SystemPromptPrefix, when set, pin this call's
	// model and prompt configuration instead of reading the agent's shared,
	// mutable fields (task #265, P0-1).
	//
	// The agent's smartModel/fastModel/systemPromptPrefix are process-wide
	// and get REWRITTEN in place by coordinator.applyModelOverrides, which
	// any per-session model override triggers. Every turn re-reads them, so
	// with two sessions running concurrently — the whole point of this fork
	// — session A's override lands in session B's next turn, and a turn can
	// even observe a model change partway through its own run. These fields
	// let a caller hand the resolved values down with the call, so a run
	// stays self-consistent regardless of what another session does
	// meanwhile.
	//
	// Pointers, so "explicitly set" is distinguishable from "zero value":
	// nil means "use the agent's shared value", which is exactly what every
	// existing caller that never sets them keeps getting.
	SmartModel         *Model
	FastModel          *Model
	SystemPromptPrefix *string
	// Credentials keeps non-durable per-call provider credentials attached to
	// the in-process call snapshot. It is never serialized or rebuilt.
	Credentials *CredentialSet `json:"-"`

	// PersistSmartModel/PersistFastModel are the explicitly requested session
	// slots for an admitted ExecuteRun. They are written after the mailbox and
	// inter-process session lock are both acquired, so a rejected call cannot
	// alter durable session state. Nil means leave that slot untouched.
	PersistSmartModel *session.ModelSlotUpdate `json:"-"`
	PersistFastModel  *session.ModelSlotUpdate `json:"-"`

	// SystemPrompt pins the BASE system prompt — the one applyModelOverrides
	// rebuilds from the resolved provider/model. It is distinct from, and
	// lower precedence than, SystemPromptOverride: the override is the
	// per-session prompt persisted in the DB, which still wins when present.
	//
	// This one matters on the paths where the per-session prompt is empty
	// (resolveSessionSystemPrompt returning "" on a DB error, mainly) — the
	// turn then falls back to the agent's shared systemPrompt, which is
	// exactly the field another session's override rewrites.
	SystemPrompt *string

	// Tools pins THIS call's complete tool slice (R3-1). It is built per
	// call by resolveSessionModels/applyModelOverrides from the same
	// pinned config snapshot and context as SmartModel/SystemPrompt above,
	// so the per-call policy (CallOptions.DisableSubAgents, ModelRole)
	// decides it, and the turn consumes its own slice at start and at
	// every PrepareStep instead of re-reading the shared a.tools — which
	// any concurrent call's UpdateModels/buildTools rewrites in place.
	// nil (legacy callers; direct sessionAgent.Run tests) keeps the
	// historical shared-slice behavior, including PrepareStep's live
	// re-read. json:"-": never durable-queue-persisted — tool objects are
	// live in-process values, so pump-driven rebuilds fall back to the
	// shared path exactly like any other legacy caller.
	Tools []fantasy.AgentTool `json:"-"`

	// BatchQuestion freezes one delegation's route, live answer binding and
	// question depth across wake/replacement turns. The binding is not durable:
	// producers refuse live capability-bearing calls, while durable consumers
	// reconstruct deny-only state from durable delegation facts, including legacy rows.
	// A batch leaf restart independently reads its durable question_route.
	BatchQuestion *BatchChildQuestionState `json:"-"`

	// RunAllowlist pins THIS call's restricted-run permission policy
	// (R3-4): the compiled matcher, armed by the turn loop (runOwned) ONLY
	// when this call becomes the active turn — never at queue time. It is
	// stamped by buildCall/runInternal from the call's context, and
	// recompiled from the serialized spec by RebuildSessionAgentCall for
	// durable-queue restarts (see RunAllowlistSpec below). nil (legacy
	// in-process callers, and durable rows persisted before the spec field
	// existed) arms nothing: such turns fall back to the session baseline
	// ExecuteRun armed for their session (F2,
	// permission.SessionRunAllowlistBaselineManager — now a legacy-row
	// fallback only), and only then to the process-wide gate.
	// json:"-": the compiled matcher is an in-process value; the
	// serializable spec travels in RunAllowlistSpec below.
	RunAllowlist *permission.RunAllowlist `json:"-"`

	// RunAllowlistSpec pins THIS call's restricted-run policy in its
	// serializable, pre-compilation form (the spec RunAllowlist was compiled
	// from). Unlike RunAllowlist above, it is NOT json:"-": it is the field
	// ToSessionAgentCallData persists on the durable run queue, and
	// RebuildSessionAgentCall recompiles it into RunAllowlist so a
	// durably-restarted call runs under ITS OWN caller's declared policy —
	// keyed by LogicalCallID — instead of a session-wide last-writer-wins
	// fallback (review R4-1) and instead of leaving its sub-agents to
	// inherit auto-approval with no restriction (review R4-2). Its presence
	// on a pump-rebuilt call is also the durable marker that the call's
	// caller ran the session non-interactive with auto-approve
	// (RunSessionAgentCall uses it to re-arm AutoApproveSession after a
	// real process restart, review R4-3). nil for legacy in-process
	// callers, pre-migration durable rows, and non-ExecuteRun callers.
	RunAllowlistSpec *permission.RunAllowlistSpec

	// FolderScopeSpec pins THIS call's folder scope in its serializable,
	// pre-compilation form (the spec CallOptions.FolderScope was compiled
	// from). Unlike CallOptions above, it is NOT json:"-": it is the
	// field ToSessionAgentCallData persists on the durable run queue,
	// and RebuildSessionAgentCall recompiles it into a rebuilt call's
	// CallOptions.FolderScope so a durably-restarted call gets its
	// scoped filesystem toolset back instead of silently restarting
	// with the shared unscoped toolset (T12). nil for unscoped calls,
	// legacy in-process callers, and pre-migration durable rows.
	FolderScopeSpec *permission.FolderScopeSpec

	// DiskRequirement is the process-bound execution requirement THIS call
	// must run under (R12-H2b): the opaque binding ID (with the
	// serializable policy baseline) a continuation restores or refuses.
	// It is stamped at admission and round-trips through
	// SessionAgentCallData so every continuation carries its own identity
	// instead of inheriting "the latest provider for the session". nil for
	// ordinary OS-disk calls. Unlike CallOptions (json:"-") this IS
	// serialized: ToSessionAgentCallData persists it on the durable row.
	DiskRequirement *session.SessionExecutionRequirement

	// FromDurableQueue is true when this call originates from the durable
	// run queue (task #340). When true and the session's mailbox is already
	// owned by another in-process turn, mailbox.submit does NOT append this
	// call to mb.submitted — the durable row itself provides the retry path,
	// so no in-process handoff is needed. This prevents double-execution
	// where both the live owner (draining mb.submitted) and the pump (after
	// its backoff expires) would execute the same logical request independently
	// (P0-1 in docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md).
	//
	// For non-durable calls (web/CLI turns, interrupt-inject via
	// handleInterruptTick → InterruptAndReplace), FromDurableQueue is false and the mailbox queue
	// is the only retry path, so submit's normal mb.submitted behavior applies.
	FromDurableQueue bool

	// LogicalCallID is a stable identifier for this logical request, generated
	// ONCE when the call is first created (e.g. in buildCall). It is used as the
	// idempotency key for durable enqueue operations (task #340), ensuring that
	// retries of the same logical request reuse the same session_run_queue row
	// instead of creating duplicates. P2-1 fix.
	//
	// Previously, the idempotency key was generated from time.Now().UnixNano()
	// at enqueue time, which meant each retry got a different key — breaking the
	// idempotency contract and potentially creating multiple durable rows for the
	// same logical request.
	LogicalCallID string

	RoleContract *session.RoleContract `json:"-"`

	// CallOptions pins THIS call's execution policy (R1-1) - model role,
	// cost/token caps, peak-hours bypass, timeout watchdog policy, the
	// sub-agent ban and the fail-fast busy contract. Stamped by
	// runInternal/buildCall from the call's context (WithCallOptions); nil
	// for legacy callers, whose runs keep reading the coordinator's shared
	// Set*-state. Never durable-queue-persisted: ToSessionAgentCallData
	// does not copy it, so pump-driven rebuilds fall back to the shared
	// path exactly like any other legacy caller.
	CallOptions *CallOptions `json:"-"`

	// OnQueueResolved, if non-nil, is invoked exactly once with the outcome
	// of THIS specific call's own turn -- whether it ran immediately or was
	// queued behind a busy mailbox and executed later by runOwned's own
	// dispatch loop (agent_run.go's `for` loop, one call per iteration).
	// Unlike OnAssistantMessageCreated (fires per assistant row, mid-turn),
	// this fires once, after the turn this exact call became has fully
	// returned. nil for every existing caller -- added for
	// coordinator.runAwaitingAdmission (#1036).
	// json:"-": in-process only, never durable-queue-persisted (same
	// rationale as OnUserMessageCreated/OnAssistantMessageCreated above).
	//
	// CONTRACT: a call carrying this hook has a live in-process waiter with
	// no implicit timeout anywhere in the chain -- every path that removes a
	// call from a mailbox queue WITHOUT running it as a turn (durable
	// orphan/restart enqueue, ClearQueue, a replacement overwritten by a
	// newer interrupt-and-replace) MUST resolve this hook with an error
	// (see queued_call_resolution.go's resolveCallWithError/
	// splitCallsWithoutWaiters) instead of silently dropping the call, or
	// the waiter blocks forever.
	OnQueueResolved func(*fantasy.AgentResult, error) `json:"-"`

	// FailIfSessionBusy rejects this call instead of queueing it when the
	// session's mailbox is already owned (submit returns false): Run fails
	// with an error wrapping ErrSessionBusy instead of the historical
	// silent nil. Sourced from CallOptions.FailIfSessionBusy. The guard is
	// enforced AT the atomic mailbox reservation, so two simultaneous
	// starters can never both observe "idle" and slip into the queue -
	// exactly one wins, the loser fails fast (#818/R1-4). Not durable: a
	// pump-driven retry is by definition not racing a live in-process
	// starter for ownership.
	FailIfSessionBusy bool

	// IsDrain: phase-4 step 3 drain call. Set by newDrainCall
	// (agent_drain.go) or, for the prompt-string entry points, from
	// WithDrainCall's ctx marker (buildCall/runInternal) -- see agent_drain.go
	// for the full contract. Never durable-queue-persisted.
	IsDrain bool
	// DrainTurnCommitted marks a retry of a Drain call whose first attempt
	// already reached the provider (a transient failure or 401 retry): the
	// notice it reacts to was pulled by that first attempt, so the retry
	// must not re-apply the turn-start "pulled a wake=1 notice" gate.
	DrainTurnCommitted bool `json:"-"`
}

type SessionAgent interface {
	Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)
	// ReserveExclusive atomically claims exclusive ownership of sessionID's
	// mailbox without starting a turn (task #614). See the sessionAgent
	// implementation's doc for the full contract: the returned epoch/cancel
	// pair must be handed to exactly one of RunWithReservedOwnership or
	// ReleaseExclusive, exactly once. ok is false (fail closed) when the
	// session is already owned or the mailbox is stopped. Returns holdCtx
	// (the derived context that cancellation signals), the epoch, a cancel
	// func, and ok. The caller must present the epoch and cancel to exactly
	// one of RunWithReservedOwnership or ReleaseExclusive.
	ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool)
	// ReleaseExclusive drops a reservation taken by ReserveExclusive without
	// running a turn, safely handing off anything that raced into the
	// mailbox during the hold. Use on any bail-out path after a successful
	// ReserveExclusive that will not call RunWithReservedOwnership.
	ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc)
	// RunWithReservedOwnership executes call using ownership already claimed
	// by ReserveExclusive, continuing the SAME ownership era instead of
	// releasing and re-claiming — see its own doc for why that gap matters.
	// onHandoff, if non-nil, is invoked immediately before the handoff to
	// runOwned; it is used by the caller to transfer release responsibility.
	RunWithReservedOwnership(ctx context.Context, call SessionAgentCall, epoch uint64, cancel context.CancelFunc, onHandoff func()) (*fantasy.AgentResult, error)
	SetModels(smart Model, fast Model)
	SetTools(tools []fantasy.AgentTool)
	SetSystemPrompt(systemPrompt string)
	SetSystemPromptPrefix(prefix string)
	SystemPrompt() string
	Cancel(sessionID string)
	CancelAll() (stillBusy bool)
	// ShuttingDown reports whether CancelAll has already latched this
	// agent's one-way shutdown latch. It is a plain read of the agent's own
	// latch; an implementation that has no latch reports false.
	ShuttingDown() bool
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// QueueMessage appends a call to the session's pending queue without
	// starting a Run. Used by the "interrupt and send" path in the web
	// server: the caller queues, then Cancel()s the running turn, and the
	// in-flight Run() drains the queue from its cancel-handling branch.
	//
	// P2-1 API footgun warning: there is NO production caller of this method
	// today (grep "QueueMessage(" outside tests returns empty). The method
	// queues a call WITHOUT starting a runner — a caller MUST independently
	// guarantee a Run() will drain it soon. There is no owner-side timeout.
	// For new production code, prefer Run()/InterruptAndReplace, which provide
	// atomic submit-and-run or handoff semantics. This method exists primarily
	// for legacy mailbox migration tests.
	QueueMessage(call SessionAgentCall)
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4). Replaces the QueueMessage+Cancel two-step that
	// deterministically wiped its own queued message (P0-2). Returns true
	// when a turn was actually interrupted; false when the session was idle
	// (the caller should then queue the call for the next Run itself).
	InterruptAndReplace(sessionID string, call SessionAgentCall) bool
	// InjectMessage persists `call` as a regular user message in the DB
	// immediately (so the UI sees it the moment the operator clicks Inject)
	// AND — if the session is currently running — schedules the message to
	// be appended to `prepared.Messages` at the next PrepareStep boundary so
	// it lands in the next provider request without a restart. Returns the
	// persisted message. When the session is NOT busy, the message is just
	// persisted; the caller can decide whether to start a new Run.
	InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up.  Returns ErrSummarizeQueued when queued.
	//
	// The snapshot contains the model, provider options, and prompt prefix
	// resolved from the target session (or shared state for sessions without
	// overrides), ensuring the entire summarize operation uses consistent
	// configuration regardless of concurrent SetModels calls (task #341).
	Summarize(context.Context, string, *SummarizeSnapshot) error
	// SummarizeQueued reports whether a manual summarise is pending for the
	// given session.
	SummarizeQueued(sessionID string) bool
	// TakeSummarizeQueue atomically removes and returns the pending summarise
	// snapshot for the session (if any).
	TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool)
	// CancelQueuedSummarize removes a pending summarise from the queue.
	CancelQueuedSummarize(sessionID string)
	// SetTimeoutOptions configures the stream watchdog's deadline extension
	// behaviour for the next and subsequent Run() calls. Called from
	// RunNonInteractive when --timeout-extends-on-progress is set.
	// Fork patch: batch 8.
	SetTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	Model() Model
}

type Model struct {
	Model      fantasy.LanguageModel
	CatwalkCfg catwalk.Model
	ModelCfg   config.SelectedModel
	FlatRate   bool
}

// DefaultCancelAllGrace is CancelAll's default grace period: how long it
// waits for in-flight Run()/Summarize() goroutines to unwind after
// cancellation before reporting stillBusy. App.ShutdownAfterDrain reuses
// the same duration as the stall bound after which a shutdown stops
// waiting for its caller's admitted calls and cancels them instead, so
// every shutdown wait shares one policy value.
const DefaultCancelAllGrace = 5 * time.Second

// compiler-forced export: the coordinator-side summarize code reads and builds snapshots from package agent.
// SummarizeSnapshot holds an immutable snapshot of all configuration needed
// for a single manual/queued summarize operation. It is computed ONCE from the
// target session's persisted models (or from shared state for sessions without
// overrides) and passed through the entire summarize path, ensuring the provider
// options, model, and prompt prefix never diverge due to concurrent SetModels
// calls (task #341, P1-1).
//
// This mirrors the resolvedOverrides pattern used for normal turns, but
// specialized for summarize which doesn't need fastModel or systemPrompt.
type SummarizeSnapshot struct {
	Model           Model
	ProviderOptions fantasy.ProviderOptions
	PromptPrefix    string
	ProviderCfg     config.ProviderConfig
}

// BatchChildQuestionState freezes ONE batch delegation's question
// capability onto its driver call template: the stamped route, the
// held-answer binding and the question depth. runSubAgent captures it
// from the driver-stamped run context; runTurn re-arms it onto the
// turn context of EVERY turn of the delegation (initial, wake Drain,
// interrupt replacement), because a wake arrives on a background
// callback context that carries none of them.
type BatchChildQuestionState struct {
	Depth     int
	Route     bool
	Binding   BatchAgentQuestionBinding
	BindingOK bool
}

type BatchAgentQuestionBinding struct {
	Ask func(context.Context, int, string) (json.RawMessage, error)
}

// compiler-forced export: all fields are read and written by the session-agent recovery code, which stays in package agent for now (design §2.3 moves consumers later).
type CodexRecoveryState struct {
	Cfg               *config.Config
	Session           session.Session
	Creds             *CredentialSet
	UserID            string
	MaxTokens         int64
	MaxCost           float64
	TargetModel       Model
	TargetPrefix      string
	TargetSystem      string
	TargetPrompt      string
	TargetMaxOutput   int64
	TargetAttachments []message.Attachment
	TargetTools       []fantasy.AgentTool
	Spent             atomic.Int64
	Attempted         atomic.Bool
}

func (s *CodexRecoveryState) MarkUser(id string) {
	if s != nil && s.UserID == "" {
		s.UserID = id
	}
}

// RequestedModelRole selects only a delegation's model, never its tool policy.
type RequestedModelRole string

var (
	ErrAgentRoleInvalid      = errors.New("agent_role_invalid")
	ErrAgentRoleUnconfigured = errors.New("agent_role_unconfigured")
	ErrAgentRoleMismatch     = errors.New("agent_role_mismatch")
)

func (r RequestedModelRole) Valid() bool {
	switch config.SelectedModelType(r) {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return config.ValidateCustomRoleName(string(r)) == nil
	}
}

func (r RequestedModelRole) Builtin() bool {
	switch config.SelectedModelType(r) {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return false
	}
}

// UnmarshalJSON distinguishes an omitted legacy role from explicit empty/null.
func (r *RequestedModelRole) UnmarshalJSON(raw []byte) error {
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return fmt.Errorf("%w: expected a built-in or valid custom role name", ErrAgentRoleInvalid)
	}
	role := RequestedModelRole(name)
	if !role.Valid() {
		return fmt.Errorf("%w: expected a built-in or valid custom role name", ErrAgentRoleInvalid)
	}
	*r = role
	return nil
}
