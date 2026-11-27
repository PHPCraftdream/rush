package agentcall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// CoordinatorBatchFacts exposes the batch fact-binding identity needed by readers.
type CoordinatorBatchFacts interface {
	BatchFactsBindingKey() any
}

// CoordinatorBatchReactionStore exposes the identity needed for reaction writes.
type CoordinatorBatchReactionStore interface {
	BatchReactionStoreBindingKey() any
}

// CoordinatorBatchScopeRecovery exposes the scope-time recovery operation.
type CoordinatorBatchScopeRecovery interface {
	RecoverBatchScopeDeadCandidates(context.Context, string)
}

// CoordinatorDrainAttempt is the opaque drain-accounting attempt accepted by the
// coordinator port. The concrete implementation is *drainctl.DrainAttempt;
// implementations must return/pass the same pointer so accounting remains tied
// to the exact attempt record. The marker keeps unrelated values from being
// accepted by production callers at compile time.
type CoordinatorDrainAttempt interface {
	CoordinatorDrainAttemptMarker() any
}

// ModelOverride allows callers to specify per-run model overrides (provider + model ID).
type ModelOverride struct {
	Provider        string
	Model           string
	ReasoningEffort string
	ServiceTier     string
}

// ErrAwaitingAnswer classifies AwaitingAnswerError the same way
// errProviderPeakHours classifies PeakHoursError: it lets callers that only
// care about the error class (not the structured question/options/session
// detail) use errors.Is without knowing the concrete type.
var ErrAwaitingAnswer = errors.New("agent asked a question and is awaiting an operator/orchestrator answer")

// ProviderMaxRetries moved here from package agent (lane SA3 of the 2026-11-15 agent split); package agent and its importers read it through agentcall.
// ProviderMaxRetries pins the fantasy retry COUNT at the pre-v0.45 value. The
// v0.45 bump raised the default from 2 retries to 3, which tripled the total
// wait of an exhausted retry flow (5+10+20 s vs 2+4 s) and pushed internal/app
// close to its CI timeout. Every fantasy agent call/stream call below sets
// MaxRetries from this var so attempt counts stay as before the bump. The
// library's initial delay (5 s vs the old 2 s) cannot be pinned through the
// agent API (fantasy v0.45.2 builds DefaultRetryOptions() per call and only
// MaxRetries is overridable) and is an accepted residual; a Retry-After
// header of up to 60 s still wins over the exponential delay, so
// server-directed rate-limit waits are unchanged.
var ProviderMaxRetries = 2

// AwaitingAnswerError is the sentinel error the (forthcoming) question tool
// returns to force-stop the current turn instead of blocking on a synchronous
// answer — this fork's `rush run` has no code path that can wait mid-turn
// for operator input (web sessions auto-approve permissions unconditionally,
// see internal/server/handlers.go:170-175; headless sessions do too), so
// "ask a question" has to mean "stop the turn cleanly and hand the operator
// an unambiguous resume command" rather than "block until answered".
//
// Structurally this mirrors PeakHoursError: a concrete, typed error carrying
// exactly what the orchestrator-facing guidance needs (the question, any
// suggested answer options, and the session id to resume), so callers don't
// have to parse Error()'s prose to act on it.
type AwaitingAnswerError struct {
	Question  string
	Options   []string
	SessionID string
}

func (e *AwaitingAnswerError) Error() string {
	return fmt.Sprintf(
		"agent asked a question and is awaiting an answer (session %s): %s",
		e.SessionID, e.Question,
	)
}

// Unwrap lets errors.Is(err, ErrAwaitingAnswer) keep working for callers
// that only care about the error class, not the structured detail.
func (e *AwaitingAnswerError) Unwrap() error { return ErrAwaitingAnswer }

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunWithOverrides is like Run but allows overriding the smart and/or fast model for this call.
	RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	CancelAll() (stillBusy bool)
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	// ReserveExclusive atomically claims exclusive ownership of sessionID
	// without starting a turn (task #614). See SessionAgent.ReserveExclusive
	// for the full contract — in particular that the returned holdCtx/epoch/cancel
	// tuple must be handed to exactly one of RunWithReservedOwnership or
	// ReleaseExclusive, exactly once. ok is false (fail closed) when the
	// session is already busy or the session's mailbox is hard-stopped
	// (the CancelAll/shutdown latch — see mailbox.hardStop). This is a
	// pure mailbox-state check: it does NOT consult the coordinator's own
	// readiness gate (readyWg), so a coordinator whose parent ctx has
	// already been cancelled still grants reservations.
	ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool)
	// ReleaseExclusive drops a reservation taken by ReserveExclusive without
	// running a turn. Use on any bail-out path that will not call
	// RunWithReservedOwnership.
	ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc)
	// RunWithReservedOwnership runs prompt for sessionID using ownership
	// already claimed by ReserveExclusive, continuing the SAME ownership
	// era instead of releasing and re-claiming it — this is what lets a
	// caller (handleRerunMessage) hold exclusive ownership across deleting
	// history and starting the replacement turn with no gap in between for
	// a concurrent Send/Rerun to slip through. smart/fast follow the same
	// override semantics as RunWithOverrides (nil means "use session/config
	// defaults"). onHandoff, if non-nil, is invoked immediately before the
	// handoff to the agent layer; it is used by the caller to transfer
	// release responsibility.
	RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	// InterruptAndSend queues a new user message and cancels the running
	// turn so the queued message picks up immediately with everything
	// produced so far retained in history.
	InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) error
	// InjectMessage persists a user message and, if the session is currently
	// running, schedules it to be merged into the next provider request
	// without cancelling the in-flight turn. See SessionAgent.InjectMessage.
	InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error)
	// Summarize compresses the session history. If the session is currently
	// busy the request is queued; call TakeSummarizeQueue after the task
	// finishes to pick it up. Returns ErrSummarizeQueued when queued.
	//
	// The snapshot contains the model, provider options, and prompt prefix
	// resolved from the target session (or shared state for sessions without
	// overrides), ensuring the entire summarize operation uses consistent
	// configuration regardless of concurrent SetModels calls (task #341).
	Summarize(context.Context, string, *SummarizeSnapshot) error
	SummarizeQueued(sessionID string) bool
	TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool)
	CancelQueuedSummarize(sessionID string)
	Model() Model
	UpdateModels(ctx context.Context) error
	GetSystemPrompt() string
	BuildSystemPrompt(ctx context.Context) (string, error)
	BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error)
	UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error
	// SetAgentTimeoutOptions configures the stream watchdog's deadline
	// extension on the current agent. Called from RunNonInteractive when
	// --timeout-extends-on-progress is set. Fork patch: batch 8.
	SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration)
	// SetRunLimits sets cost and token caps for the next Run call.
	// Fork patch: batch 30.
	SetRunLimits(maxCost float64, maxTokens int64)
	// SetActiveModelRole records which named model slot (smart, fast,
	// worker, reviewer) is driving the CURRENT top-level run, so sub-agent
	// spawns can decide whether to prefer the cheaper Worker slot instead of
	// blindly inheriting the parent's Smart model. An empty/unset value
	// means "unknown — treat as smart": the interactive TUI/web path never
	// calls this, and for those the default behavior — use Smart for
	// everything, i.e. exactly today's behavior — is correct, since
	// "Smart" = the default slot. Fork patch (reviewer/worker roles).
	SetActiveModelRole(role config.SelectedModelType)
	// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
	// for the next Run call. It exists so `rush run --allow-peak-hours`
	// can override an operator-configured peak_hours window for a single
	// conscious invocation without introducing a persistent "always
	// allow" config setting. Fork patch (peak-hours bypass).
	SetAllowPeakHours(allow bool)
	// SetPersistentMode marks this coordinator as the long-lived web/interactive
	// server (enables Phase 4 autonomous idle-resume eligibility). rush run
	// leaves it false.
	SetPersistentMode(persistent bool)
	// ResetAutoResumeCounter clears the Phase 4 consecutive-auto-resume bound
	// for a session. Called from the human send path so a human re-entering the
	// loop re-arms autonomy.
	ResetAutoResumeCounter(sessionID string)
	// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
	// for run queue pump execution (task #340, ROUND 3 migration).
	// rowID is the leased run-queue entry's durable id (R12-H2b §2.4):
	// the rebuild releases the row-keyed Continuation holder through it.
	// Callers without a row id pass "".
	RebuildSessionAgentCall(ctx context.Context, rowID string, data coresession.SessionAgentCallData) (SessionAgentCall, error)
	// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
	// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
	// call is already fully reconstructed with all necessary data.
	RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	// CancelTurn cancels ONLY sessionID's live generation (its own agent's
	// Cancel): no job stop, no delegation-tree walk, no wake zeroing, no
	// auto-resume suspension -- unlike Cancel, which is the full Stop. Rerun
	// uses it so a kept history's running jobs, unreacted debt and autonomy
	// survive the rerun (docs/reviews/2026-09-29-async-phase4-round1-rerun-
	// design.md).
	CancelTurn(sessionID string)
	// BatchRerunStopVoided stops the executors a committed batch rerun
	// truncation voided (session.BatchStore.BatchRerunReconcile's Voided
	// node set): each still-running local root incarnation is stopped with
	// Stop semantics, and every voided delegation's child tree is stopped
	// like Stop (stopTree). Best effort, called strictly AFTER the
	// truncation transaction committed -- a stop that fails or cannot reach
	// a foreign host's executor is safe: the rows are already void, so the
	// completion commits void (no debt, never pulled). See
	// batch_cutover_rerun.go (the BatchRerunStopper surface).
	BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode)
}

// ProviderType mirrors catwalk.Type (charm.land/catwalk,
// pkg/catwalk/provider.go) one-to-one: a fixed closed enum, not a
// free-form string. Keep in sync with catwalk.Type.
type ProviderType string

const (
	ProviderTypeOpenAI       ProviderType = "openai"
	ProviderTypeOpenAICompat ProviderType = "openai-compat"
	ProviderTypeOpenRouter   ProviderType = "openrouter"
	ProviderTypeAnthropic    ProviderType = "anthropic"
	ProviderTypeGoogle       ProviderType = "google"
	ProviderTypeGoogleVertex ProviderType = "google-vertex"
	ProviderTypeAzure        ProviderType = "azure"
	ProviderTypeBedrock      ProviderType = "bedrock"
	ProviderTypeVercel       ProviderType = "vercel"
)

// Role names the model slot a ModelChoice fills in a CredentialSet. The
// string values MUST stay literally identical to
// config.SelectedModelType* so a Role casts to config.SelectedModelType
// (and back) without conversion.
type Role string

const (
	RoleSmart    Role = "smart"
	RoleFast     Role = "fast"
	RoleWorker   Role = "worker"
	RoleReviewer Role = "reviewer"
)

// validRoles and validProviderTypes list the enum values in display
// order for validation errors; the known* helpers are Validate's
// membership checks. Keep both slices in sync with the const blocks
// above.
var (
	validRoles         = []Role{RoleSmart, RoleFast, RoleWorker, RoleReviewer}
	validProviderTypes = []ProviderType{
		ProviderTypeOpenAI,
		ProviderTypeOpenAICompat,
		ProviderTypeOpenRouter,
		ProviderTypeAnthropic,
		ProviderTypeGoogle,
		ProviderTypeGoogleVertex,
		ProviderTypeAzure,
		ProviderTypeBedrock,
		ProviderTypeVercel,
	}
)

// knownRole reports whether role is a built-in or syntactically valid custom slot.
func knownRole(role Role) bool {
	switch config.SelectedModelType(role) {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return config.ValidateCustomRoleName(string(role)) == nil
	}
}

// knownProviderType reports whether t is one of the nine ProviderType
// constants.
func knownProviderType(t ProviderType) bool {
	for _, known := range validProviderTypes {
		if t == known {
			return true
		}
	}
	return false
}

// roleNames renders validRoles as a comma-separated list for error
// messages.
func roleNames() string {
	names := make([]string, 0, len(validRoles))
	for _, role := range validRoles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}

// providerTypeNames renders validProviderTypes as a comma-separated
// list for error messages.
func providerTypeNames() string {
	names := make([]string, 0, len(validProviderTypes))
	for _, t := range validProviderTypes {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

// CredentialModel describes one model a tenant's provider serves. Pure
// metadata: Rush never validates a ModelChoice.Model against the owning
// Credential's list — the first real provider call is the validator,
// exactly like `rush run --model` today. Named CredentialModel rather
// than Model because package agent already declares Model for a built
// fantasy.LanguageModel wrapper; the JSON shape is the contract, not
// the Go name.
type CredentialModel struct {
	ID               string `json:"id"`
	ContextWindow    int64  `json:"context_window,omitempty"`
	DefaultMaxTokens int64  `json:"default_max_tokens,omitempty"`
	CanReason        bool   `json:"can_reason,omitempty"`
}

// Credential is one tenant provider: which API shape it speaks (Type),
// where to reach it, and the literal API key to present. API-key only by
// design — OAuth/token-refresh providers are explicitly out of scope
// for per-call credentials. The Models list is informational metadata
// (context window, max tokens, reasoning), never a validation allowlist.
type Credential struct {
	Provider string            `json:"provider"`
	Type     ProviderType      `json:"type"`
	APIKey   string            `json:"api_key"`
	BaseURL  string            `json:"base_url,omitempty"`
	Models   []CredentialModel `json:"models,omitempty"`
}

// ModelChoice selects which model of which provider serves one role for
// this call. Model is NOT validated against the owning Credential's
// Models list — an unknown id degrades exactly like an unverified model
// in rush.json (zero-value metadata, warning log) and fails on the
// first real provider call if truly wrong.
type ModelChoice struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	MaxTokens       int64  `json:"max_tokens,omitempty"`
}

// CredentialSet is the per-call credential bundle for
// RunWithCredentials. The smart role is required in Models; fast,
// worker, and reviewer are optional. Strict isolation is the default:
// a smart/fast role the set does not cover is a hard error before any
// provider traffic — Rush will NOT silently serve it from the Client's
// configured providers. Set AllowConfiguredRoleFallback to explicitly
// re-enable configured-model fallback for uncovered roles (see that
// field's doc for the trust boundary it crosses). Treat the value as
// immutable after construction; a RunWithCredentials call never
// mutates it.
type CredentialSet struct {
	Credentials []Credential         `json:"credentials"`
	Models      map[Role]ModelChoice `json:"models"`
	// AllowConfiguredRoleFallback re-enables the ordinary session/config
	// model resolution for roles this set does not cover. The default
	// (false) is fail-closed: an uncovered smart/fast role is an error
	// before any provider traffic. True deliberately crosses the
	// tenant-credential boundary: the uncovered role — with whatever
	// tenant data it carries (fast drives title generation) — is served
	// by the Client's configured (operator) provider, not the tenant's.
	AllowConfiguredRoleFallback bool `json:"allow_configured_role_fallback,omitempty"`
}

// Clone returns a deep copy safe to retain for an asynchronous run.
func (cs CredentialSet) Clone() CredentialSet {
	clone := cs
	if cs.Credentials != nil {
		clone.Credentials = make([]Credential, len(cs.Credentials))
		for i, cred := range cs.Credentials {
			clone.Credentials[i] = cred
			if cred.Models != nil {
				clone.Credentials[i].Models = append([]CredentialModel(nil), cred.Models...)
			}
		}
	}
	if cs.Models != nil {
		clone.Models = make(map[Role]ModelChoice, len(cs.Models))
		for role, choice := range cs.Models {
			clone.Models[role] = choice
		}
	}
	return clone
}

// Validate checks the bundle's internal consistency: at least one
// credential, at least one role choice, every choice naming a known
// credential, non-empty provider types drawn from the nine ProviderType
// constants, a base URL wherever the provider type has no built-in
// default endpoint, Models keys restricted to the four Role constants,
// and the required smart role present. Model IDs are deliberately NOT
// checked against the Credentials' Models lists.
func (cs *CredentialSet) Validate() error {
	if cs == nil {
		return fmt.Errorf("credential set is nil")
	}
	if len(cs.Credentials) == 0 {
		return fmt.Errorf("credential set has no credentials")
	}
	if len(cs.Models) == 0 {
		return fmt.Errorf("credential set has no model choices")
	}
	known := make(map[string]struct{}, len(cs.Credentials))
	for _, cred := range cs.Credentials {
		if cred.Provider == "" {
			return fmt.Errorf("credential is missing its provider name")
		}
		if err := ValidateCredentialProviderID(cred.Provider); err != nil {
			return err
		}
		if _, dup := known[cred.Provider]; dup {
			return fmt.Errorf("duplicate credential for provider %q", cred.Provider)
		}
		known[cred.Provider] = struct{}{}
		if cred.Type == "" {
			return fmt.Errorf("credential for provider %q is missing its type", cred.Provider)
		}
		if !knownProviderType(cred.Type) {
			return fmt.Errorf("credential for provider %q has unknown type %q; valid types: %s", cred.Provider, cred.Type, providerTypeNames())
		}
		if cred.Type == ProviderTypeGoogleVertex {
			return fmt.Errorf("provider %q: google-vertex credentials are unsupported because Vertex uses ambient credentials", cred.Provider)
		}
		if cred.APIKey == "" && cred.Type != ProviderTypeOpenAICompat {
			return fmt.Errorf("provider %q: api_key is required for per-call credentials", cred.Provider)
		}
		if cred.Type == ProviderTypeOpenAICompat && cred.BaseURL == "" {
			return fmt.Errorf("provider %q: base_url is required for the %q provider type", cred.Provider, cred.Type)
		}
	}
	for role, choice := range cs.Models {
		if !knownRole(role) {
			return fmt.Errorf("model choice uses unknown role %q; valid roles: %s", role, roleNames())
		}
		if _, ok := known[choice.Provider]; !ok {
			return fmt.Errorf("model choice for role %q references provider %q which is not in the credential set", role, choice.Provider)
		}
		if choice.Model == "" {
			return fmt.Errorf("model choice for role %q has an empty model id", role)
		}
	}
	if _, ok := cs.Models[RoleSmart]; !ok {
		return fmt.Errorf("credential set must define the smart role (Models[RoleSmart]); smart drives every turn")
	}
	return nil
}

// Credential returns the named credential from the set.
func (cs *CredentialSet) Credential(provider string) (Credential, bool) {
	for _, cred := range cs.Credentials {
		if cred.Provider == provider {
			return cred, true
		}
	}
	return Credential{}, false
}

// reservedCredentialProviderIDs lists IDs with provider-specific construction
// that would change the API or authentication used by tenant credentials.
var reservedCredentialProviderIDs = map[string]struct{}{
	"openai-codex": {},
	"hyper":        {},
	"opencode-go":  {},
	"opencode-zen": {},
	"zai":          {},
	"minimax":      {},
	"minimax-cn":   {},
	"copilot":      {},
}

// ValidateCredentialProviderID rejects empty and reserved provider IDs. Single
// source; package agent delegates here.
func ValidateCredentialProviderID(id string) error {
	if id == "" {
		return fmt.Errorf("credential is missing its provider name")
	}
	if _, reserved := reservedCredentialProviderIDs[id]; reserved {
		return fmt.Errorf("provider ID %q is reserved and cannot be used for per-call credentials", id)
	}
	return nil
}

var (
	ErrRequestCancelled = errors.New("request canceled by user")
	ErrSessionBusy      = errors.New("session is currently processing another request")
	ErrEmptyPrompt      = errors.New("prompt is empty")
	ErrSessionMissing   = errors.New("session id is missing")
	// ErrAgentShuttingDown is returned by Run when CancelAll has already
	// hard-stopped the agent (process shutdown). Returning an explicit
	// error — rather than silently queueing the call, or returning
	// (nil, nil) the way a plain "someone else owns this" would — is
	// deliberate: during shutdown no future turn will ever drain a queue,
	// so a silent accept is exactly the "accepted but never executed"
	// shape the P0-A/P0-B fixes exist to remove. The caller learns
	// immediately and can surface it.
	ErrAgentShuttingDown = errors.New("agent is shutting down")
)

// ErrCallAlreadyAttempted wraps an error that occurred AFTER the call
// started executing (i.e., after createUserMessage successfully persisted
// the user message to the database).
//
// This type is used by the retry tails (restartOrphanedWithRetry and
// startDetachedRun) to distinguish between two failure modes:
//
//  1. Error BEFORE user message creation (e.g., OS lock contention,
//     permission denied during lock acquisition): the call never executed,
//     requeuing is safe and necessary to prevent data loss (task #340).
//
//  2. Error AFTER user message creation (e.g., provider 5xx error mid-stream,
//     DB write failure after user message, summary error): the call has
//     already left a persistent trace, requeuing would cause duplicates.
//
// The wrapping point is the earliest moment after which we can confidently
// say the call "started executing": immediately after createUserMessage
// succeeds (in runTurn, agent_turn.go). Errors from that point forward are
// wrapped so the retry tails can check via errors.As and skip requeue.
//
// This is the typed signal designed to fix task #339 (P0, regression from
// commit 37f1820d) while preserving task #340's protection (data loss on
// retry exhaustion).
//
// This type implements the AlreadyAttempted() marker interface, which is
// used by the session run queue pump to detect terminal failures across
// package boundaries (task #340 ROUND 2 fix for type incompatibility bug).
type ErrCallAlreadyAttempted struct {
	Err error
}

func (e *ErrCallAlreadyAttempted) Error() string {
	return fmt.Sprintf("call already attempted: %v", e.Err)
}

func (e *ErrCallAlreadyAttempted) Unwrap() error {
	return e.Err
}

// AlreadyAttempted implements the marker interface for cross-package
// terminal failure detection. This method is called by the session run
// queue pump to distinguish retryable errors from terminal ones.
func (e *ErrCallAlreadyAttempted) AlreadyAttempted() bool {
	return true
}

// ErrTurnStalled is joined into the turn error when the stall abort fires,
// so the CLI can map the exit to exit_reason "stalled".
var ErrTurnStalled = errors.New("turn aborted: no durable progress past the stall timeout")

// CallBindingHandle is the binding-handle port of one trusted call state
// (design 2026-11-15-sprint0-split-agent.md §3.1 S-A1): the concrete
// binding stays with the disk-provider registry owner (callDiskBinding in
// package agent today, batchinst later); CallOptions carries only this
// interface and reads it through BindingID.
type CallBindingHandle interface {
	BindingID() string
}

// BuildResourceScope is the closeable build-resource scope of one trusted
// call state: every construction pin performed for the call registers its
// construction release here, so closing the scope releases exactly those
// construction owners and nothing else. Copies of the owning CallOptions
// borrow the same scope pointer. A scope is closed once on resolution
// failure, call rejection, queue cancellation or final call completion;
// Close is idempotent.
type BuildResourceScope struct {
	mu       sync.Mutex
	releases []func()
	closed   bool
}

func NewBuildResourceScope() *BuildResourceScope { return &BuildResourceScope{} }

// Closed reports whether the scope has already been closed. Test-support oracle.
func (s *BuildResourceScope) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Register attaches one construction release to the scope. Registering on
// an already-closed scope releases immediately (the scope's lifetime has
// ended; the pin must not outlive it).
func (s *BuildResourceScope) Register(release func()) {
	if s == nil || release == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		release()
		return
	}
	s.releases = append(s.releases, release)
	s.mu.Unlock()
}

// RegisteredCount returns how many releases the still-open scope currently
// holds. Test-support oracle: a closed scope's count is the number of pins
// its Close (or immediate release on register-after-close) has already run.
func (s *BuildResourceScope) RegisteredCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.releases)
}

// Close runs every registered release exactly once.
func (s *BuildResourceScope) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	releases := s.releases
	s.releases = nil
	s.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

// EnsureCallBuildScope returns the trusted call state's build-resource
// scope, creating it first when the options carry none yet (ordinary,
// non-provider calls). It must be called before the options are shared;
// copies borrow the same scope.
func EnsureCallBuildScope(opts *CallOptions) *BuildResourceScope {
	if opts == nil {
		return nil
	}
	if opts.BuildScope == nil {
		opts.BuildScope = NewBuildResourceScope()
	}
	return opts.BuildScope
}

// borrowCallDiskBinding is the local copy of package agent's
// BorrowCallDiskBinding nil-guard: the moved CallOptions method below needs
// it and agentcall must not import agent.
func borrowCallDiskBinding(opts *CallOptions) CallBindingHandle {
	if opts == nil {
		return nil
	}
	return opts.DiskBinding
}

// BorrowDiskBindingFrom copies the binding HANDLE from src into opts (H2a
// §1.2 borrow semantics): a copy of the owning options borrows the same
// immutable handle without acquiring a new owner — src's Call owner retains
// it. BuildScope is deliberately left untouched: the borrowing turn's tool
// builds own their scope via the normal per-call setup.
func (opts *CallOptions) BorrowDiskBindingFrom(src *CallOptions) {
	if opts == nil {
		return
	}
	opts.DiskBinding = borrowCallDiskBinding(src)
}

// BorrowCallDiskBinding is the exported read side of borrowCallDiskBinding: staying
// package-agent callers delegate here (design §3.1 contract reads).
func BorrowCallDiskBinding(opts *CallOptions) CallBindingHandle { return borrowCallDiskBinding(opts) }

// CoordinatorVerdict exposes only the decision fields read by turn callers.
type CoordinatorVerdict interface {
	Kind() uint8
	Reason() string
}

// CoordinatorTurnFacts exposes the accessed parts of the launch snapshot.
type CoordinatorTurnFacts interface {
	Debt() CoordinatorDebtFacts
	Session() CoordinatorSessionFacts
}

// CoordinatorDebtFacts allows the policy path to mark pending debt inclusive.
type CoordinatorDebtFacts interface {
	SetPendingInclusive(bool)
}

// CoordinatorSessionFacts exposes reaction-chain state used by policy handling.
type CoordinatorSessionFacts interface {
	ChainNoticed() bool
	ExternallyDrivenSelf() bool
}

// CoordinatorArbiter evaluates a launch site and records chain launches.
type CoordinatorArbiter interface {
	ArbiterVerdict(context.Context, string, uint8) (CoordinatorVerdict, CoordinatorTurnFacts, error)
	ChainLastLaunch(string, string) bool
}

// CoordinatorDriverHints exposes the two driver-hint reads needed by turns.
type CoordinatorDriverHints interface {
	HintSeqOf(string) uint64
	IsExternalDriver(string) bool
}

// CoordinatorBackground exposes the owned-work counters needed by the guard.
type CoordinatorBackground interface {
	ActiveOwned(string) int
	PendingCompletionsOwned(string) int
}

// CoordinatorSubAgentDrivers exposes finalization identity binding to a driver.
type CoordinatorSubAgentDrivers interface {
	BindFinalizationIdentity(SessionAgent, SessionAgentCall) error
}

// CoordinatorFinalization exposes whether flag-gated finalization wiring exists.
type CoordinatorFinalization interface {
	Enabled() bool
}

// CoordinatorPort is the narrow coordinator surface used by agent session turns.
type CoordinatorPort interface {
	CoordinatorBatchFacts
	CoordinatorBatchReactionStore
	CoordinatorBatchScopeRecovery
	// Cfg returns the coordinator's config store.
	Cfg() *config.ConfigStore
	// BuildCredentialModel constructs a model from the pinned config and credentials.
	BuildCredentialModel(context.Context, *config.Config, *CredentialSet, ModelChoice) (Model, config.ProviderConfig, error)
	// BuildModelsFromCfg constructs models from explicit selected-model settings.
	BuildModelsFromCfg(context.Context, *config.Config, config.SelectedModel, config.SelectedModel, bool) (Model, Model, error)
	// DrainCredentialBoundary reports whether a credentialed root blocks configured credentials.
	DrainCredentialBoundary(context.Context, string) bool
	// CheckLivePeakHours checks a provider against current peak-hours configuration.
	CheckLivePeakHours(string) error
	// BatchFactsInstalled reports whether batch facts were installed.
	BatchFactsInstalled() bool
	// BackgroundOwnsWork reports owned background work for a session.
	BackgroundOwnsWork(string) bool
	// AccountDrainAttempt reports the completed drain accounting attempt.
	AccountDrainAttempt(context.Context, CoordinatorDrainAttempt, error)
	// NoteDrainRefused records a refused drain admission.
	NoteDrainRefused(string, error)
	// SuspendAutoResume suspends automatic resumes for a session.
	SuspendAutoResume(string)
	// NoteTurnFailed updates pacing after a failed provider turn.
	NoteTurnFailed(string)
	// PushDeadlineOnTurnEnd advances supervision after a turn ends.
	PushDeadlineOnTurnEnd(string)
	// ArbiterVerdict evaluates a launch site through a package-neutral adapter.
	ArbiterVerdict(context.Context, string, uint8) (CoordinatorVerdict, CoordinatorTurnFacts, error)
	// Arb exposes the narrow launch-policy adapter used by turn decisions.
	Arb() CoordinatorArbiter
	// ClearSleepAll clears all await-task sleep for the session.
	ClearSleepAll(string)
	// ReactionChainHasClaim reports whether a claim belongs to the reaction chain.
	ReactionChainHasClaim(string, string) bool
	// RecordProgress updates progress after a notice is consumed.
	RecordProgress(string)
	// DriverHints exposes the per-session external-driver and hint data.
	DriverHints() CoordinatorDriverHints
	// Background exposes owned background-work counters.
	Background() CoordinatorBackground
	// SubAgentDrivers exposes finalization identity binding.
	SubAgentDrivers() CoordinatorSubAgentDrivers
	// Finalization exposes the optional finalization wiring state.
	Finalization() CoordinatorFinalization
	BatchCutoverFacts() BatchCutoverFacts
	UnboundBatchFacts(BatchCutoverFacts) bool
	CLIActiveWorkMessages() message.Service
	ChildSessionBusy(string) bool
	SubAgentQuestionBlockFromFinish(*message.Finish) (string, bool)
	SubAgentQuestionFromFinish(string, *message.Finish) (string, bool)
	BatchNodeStateLive(string) bool
}
