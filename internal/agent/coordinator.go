package agent

// Fork patch: this Coordinator drives N concurrent web sessions, not a single
// TUI run. The fork-specific additions visible in the diff against upstream
// include:
//
//   - ModelOverride struct + RunWithOverrides path used by `handleSendMessage`
//     in `internal/server/handlers.go` so the WUI can pick a model per turn.
//   - TakeSummarizeQueue + queued background summarisation that does not
//     block the user's next message (paired with agent.go's sliding window).
//   - Wiring to `internal/agent/cliprovider` for npx-claude-code / Gemini /
//     Codex CLI providers, including MCP bridge initialisation.
//
// Upstream's `copilotResponsesModels` table and per-model Responses-API
// special-casing were removed when the dispatch was refactored. Keep an eye
// on that during merges: if upstream adds a new Responses-only model, the
// adapter selection in this file is where it needs to land.
//
// See CHANGELOG.fork.md sections 4.D (agent extensions) and 4.E (CLI
// providers) before resolving a merge conflict.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/skills"
)

// ModelOverride allows callers to specify per-run model overrides (provider + model ID).
type ModelOverride struct {
	Provider        string
	Model           string
	ReasoningEffort string
}

// Coordinator errors.
var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errSmartModelNotSelected           = errors.New("smart model not selected")
	errFastModelNotSelected            = errors.New("fast model not selected")
	errSmartModelProviderNotConfigured = errors.New("smart model provider not configured")
	errFastModelProviderNotConfigured  = errors.New("fast model provider not configured")
	// errProviderPeakHours is returned when a provider's peak_hours
	// window refuses the request. It is operator-actionable (the user
	// configured the window on purpose) and MUST NOT be retried: the
	// condition only clears when the wall clock leaves the window,
	// which the backoff loop cannot accelerate. classifyProviderError
	// pins it to classTerminal as defense-in-depth.
	errProviderPeakHours = errors.New("provider is inside its configured peak-hours window")
)

// PeakHoursError is the concrete error checkPeakHours returns while a
// provider is inside its configured peak_hours window. It carries the exact
// reopen time as a time.Time (not just formatted into the error string) so
// callers that need to act on it precisely — e.g. an orchestrating agent
// scheduling a resume — don't have to parse Error()'s text.
type PeakHoursError struct {
	ProviderID string
	Start, End string // HH:MM, as configured
	ReopensAt  time.Time
}

func (e *PeakHoursError) Error() string {
	return fmt.Sprintf(
		"provider %s is in peak hours (%s–%s), refusing until %s",
		e.ProviderID, e.Start, e.End, e.ReopensAt.Format("15:04"),
	)
}

// Unwrap lets errors.Is(err, errProviderPeakHours) keep working for callers
// that only care about the error class, not the structured detail.
func (e *PeakHoursError) Unwrap() error { return errProviderPeakHours }

// errAwaitingAnswer classifies AwaitingAnswerError the same way
// errProviderPeakHours classifies PeakHoursError: it lets callers that only
// care about the error class (not the structured question/options/session
// detail) use errors.Is without knowing the concrete type.
var errAwaitingAnswer = errors.New("agent asked a question and is awaiting an operator/orchestrator answer")

// AwaitingAnswerError is the sentinel error the (forthcoming) question tool
// returns to force-stop the current turn instead of blocking on a synchronous
// answer — this fork's `rush run` has no code path that can wait mid-turn
// for operator input (both headless and web sessions auto-approve
// permissions unconditionally, see internal/server/handlers.go:163-171), so
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

// Unwrap lets errors.Is(err, errAwaitingAnswer) keep working for callers
// that only care about the error class, not the structured detail.
func (e *AwaitingAnswerError) Unwrap() error { return errAwaitingAnswer }

// maxConsecutiveAutoResumes bounds Phase 4 autonomous idle-resumes per session
// without human involvement (reset by any human message). Anti-runaway: an
// agent that keeps backgrounding self-completing jobs cannot loop forever.
const maxConsecutiveAutoResumes = 5

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
	RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (SessionAgentCall, error)
	// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
	// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
	// call is already fully reconstructed with all necessary data.
	RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	history     history.Service
	filetracker filetracker.Service
	prompt      *prompt.Prompt
	notify      pubsub.Publisher[notify.Notification]

	currentAgent SessionAgent
	agents       map[string]SessionAgent

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	// readyWg gates every run entry point on the asynchronous half of agent
	// construction (buildAgent's prompt/tool builds). Deliberately NOT an
	// errgroup.Group: buildAgent re-registers on this same gate from every
	// UpdateModels, while other concurrent runs are already parked in Wait —
	// which errgroup and the sync.WaitGroup under it explicitly forbid ("The
	// first call to Go must happen before a Wait") and which -race reports as
	// a real data race. See readyGate's doc comment in ready_gate.go.
	readyWg readyGate

	// Per-run limits. Set via SetRunLimits before Run(). Reset after use.
	// Fork patch: batch 30. Mutex added in review-fix (data race: SetRunLimits
	// called from HTTP handler, read in runInternal on agent goroutine).
	runLimitsMu sync.Mutex
	maxCost     float64
	maxTokens   int64

	// allowPeakHours is a one-shot bypass for the peak-hours refusal,
	// armed by SetAllowPeakHours from `rush run --allow-peak-hours`.
	// Reset to false after the next Run. Fork patch (peak-hours bypass).
	allowPeakHours bool

	// activeModelRole records which named model slot is driving the current
	// top-level run, set via SetActiveModelRole. Static per-process (unlike
	// maxCost/maxTokens above, there is no reset-after-use — `rush run` is
	// single-shot). Mutex guards the same race shape as runLimitsMu:
	// SetActiveModelRole is called from RunNonInteractive before the agent
	// goroutine starts, and buildAgentModels reads it from the agent
	// goroutine. Fork patch (reviewer/worker roles).
	activeModelRoleMu sync.Mutex
	activeModelRole   config.SelectedModelType

	// Phase 4 autonomous idle-resume guardrails.
	// persistentMode: true only for the long-lived web server; false for
	// rush run. Currently written exactly once at process start (no real
	// race today), but every sibling field here (allowPeakHours,
	// activeModelRole, maxCost) is already lock/atomic-guarded, so a plain
	// bool would be a silent trap for the next caller who adds a second
	// SetPersistentMode call path — atomic.Bool costs nothing and keeps
	// this field consistent with its neighbors under `go test -race`.
	persistentMode         atomic.Bool
	autoResumeMu           sync.Mutex     // guards consecutiveAutoResumes.
	consecutiveAutoResumes map[string]int // sessionID -> consecutive auto-resumes since last human message.

	// modelCache caches resolved (smart, fast) Model pairs keyed by their
	// combined provider+model+reasoning_effort tuple. Used by
	// resolveSessionModels to avoid rebuilding the same pair repeatedly.
	// Cached as a pair (not two independent per-slot entries) so a single
	// buildModelsFromCfg call always fills both roles together — see
	// resolveSessionModels's own comment for why a per-slot cache
	// previously mismatched smart/fast roles.
	modelCache *csync.Map[string, cachedModelPair]
}

// cachedModelPair holds a resolved (smart, fast) Model pair as built
// together by a single buildModelsFromCfg call.
type cachedModelPair struct {
	smart Model
	fast  Model
}

func NewCoordinator(
	ctx context.Context,
	cfg *config.ConfigStore,
	sessions session.Service,
	messages message.Service,
	permissions permission.Service,
	history history.Service,
	filetracker filetracker.Service,
	notify pubsub.Publisher[notify.Notification],
) (Coordinator, error) {
	p, err := coderPrompt(prompt.WithWorkingDir(cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	// Discover skills once at session start.
	allSkills, activeSkills := discoverSkills(cfg)
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                    cfg,
		sessions:               sessions,
		messages:               messages,
		permissions:            permissions,
		history:                history,
		filetracker:            filetracker,
		prompt:                 p,
		notify:                 notify,
		agents:                 make(map[string]SessionAgent),
		allSkills:              allSkills,
		activeSkills:           activeSkills,
		skillTracker:           skillTracker,
		consecutiveAutoResumes: make(map[string]int),
		modelCache:             csync.NewMap[string, cachedModelPair](),
	}

	agentCfg, ok := cfg.Config().Agents[config.AgentCoder]
	if !ok || agentCfg.ID == "" {
		// Self-heal: config.Load/reload always call SetupAgents once
		// IsConfigured() becomes true, but a caller that mutates
		// Providers/SelectedModel directly on an already-published config
		// (bypassing Load/reload entirely — a test-only pattern; found via
		// a CI-only failure this exact class of gap caused,
		// errCoderAgentNotConfigured, that never reproduced on a dev
		// machine because cliprovider.Available() synthesizes a local-cli
		// provider whenever claude/gemini/codex/qwen is on PATH, making
		// IsConfigured() true at initial Init regardless of any
		// RUSH_GLOBAL_*/XDG_* isolation — confirmed by the sixth @oh
		// review pass) never triggers that population. Also guards against
		// Agents[AgentCoder] being present but zero-value (ok==true,
		// ID=="") — the bare `!ok` check alone would skip self-heal for
		// that case and build a coder agent with an empty ID/AllowedTools.
		// SetupAgents is idempotent (derives Agents purely from Options/
		// DisabledTools, no I/O), so re-deriving it here on a genuine miss
		// is safe. See
		// p350_coder_agent_selfheal_test.go for a deterministic
		// regression test that does not depend on any environment leak.
		cfg.SetupAgents()
		agentCfg, ok = cfg.Config().Agents[config.AgentCoder]
	}
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	agent, err := c.buildAgent(ctx, p, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent
	return c, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil), ensuring that every turn
	// runs with a complete, self-contained model configuration.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve session models: %w", err)
	}

	return c.runInternal(ctx, sessionID, prompt, pinned, attachments...)
}

// SetRunLimits stores cost and token caps for the next Run call.
// Fork patch: batch 30.
func (c *coordinator) SetRunLimits(maxCost float64, maxTokens int64) {
	c.runLimitsMu.Lock()
	c.maxCost = maxCost
	c.maxTokens = maxTokens
	c.runLimitsMu.Unlock()
}

// SetActiveModelRole records which named model slot is driving the current
// top-level run. Fork patch (reviewer/worker roles).
func (c *coordinator) SetActiveModelRole(role config.SelectedModelType) {
	c.activeModelRoleMu.Lock()
	c.activeModelRole = role
	c.activeModelRoleMu.Unlock()
}

// SetAllowPeakHours arms a one-shot bypass of the peak-hours refusal
// for the next Run call. Fork patch (peak-hours bypass).
func (c *coordinator) SetAllowPeakHours(allow bool) {
	c.runLimitsMu.Lock()
	c.allowPeakHours = allow
	c.runLimitsMu.Unlock()
}

// SetPersistentMode marks this coordinator as the long-lived web/interactive
// server (Phase 4 autonomous idle-resume eligibility). rush run leaves it
// false.
func (c *coordinator) SetPersistentMode(persistent bool) {
	c.persistentMode.Store(persistent)
}

// autonomyEnabled reports whether Phase 4 auto-resume is opted in via config.
func (c *coordinator) autonomyEnabled() bool {
	opts := c.cfg.Config().Options
	return opts != nil && opts.AutoResumeOnJobDone != nil && *opts.AutoResumeOnJobDone
}

// consecutiveResume returns the number of auto-resumes for sessionID since the
// last human message.
func (c *coordinator) consecutiveResume(sessionID string) int {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return c.consecutiveAutoResumes[sessionID]
}

// bumpConsecutiveResume increments the auto-resume counter for sessionID.
func (c *coordinator) bumpConsecutiveResume(sessionID string) {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	c.consecutiveAutoResumes[sessionID]++
}

// resetConsecutiveResume clears the auto-resume counter for sessionID. Called
// from the human send path so a human re-entering the loop re-arms autonomy.
func (c *coordinator) resetConsecutiveResume(sessionID string) {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	delete(c.consecutiveAutoResumes, sessionID)
}

// ResetAutoResumeCounter is the exported wrapper around resetConsecutiveResume
// for the server package's human send path.
func (c *coordinator) ResetAutoResumeCounter(sessionID string) {
	c.resetConsecutiveResume(sessionID)
}

// autoResumeEligible reports whether a finished background job should
// autonomously resume the (idle-or-busy; Run handles that) owning session.
// Pure autonomy policy: opt-in config, persistent (web) coordinator only, and
// under the consecutive-resume runaway bound. Per-turn cost/token caps are
// still enforced by the normal Run path; a Cancel aborts the auto-turn like any
// other. NEVER eligible for rush run (persistentMode stays false there).
func (c *coordinator) autoResumeEligible(sessionID string) bool {
	return c.autonomyEnabled() &&
		c.persistentMode.Load() &&
		c.consecutiveResume(sessionID) < maxConsecutiveAutoResumes
}

// SetAgentTimeoutOptions delegates to the current agent's SetTimeoutOptions.
// Fork patch: batch 8.
func (c *coordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	c.currentAgent.SetTimeoutOptions(extendsOnProgress, hardCap)
}
