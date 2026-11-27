package agent

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/responsesws"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/wakectl"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// The reaction chain guard (#1113): the implementation moved to
// reaction_component.go (the drainComponent host pattern); this file keeps
// the thin coordinator delegates.

func (c *coordinator) reactionChainCount(sessionID string) int {
	return newDrainComponent(c).reactionChainCount(sessionID)
}

func (c *coordinator) chainGuardMarker(ctx context.Context, sessionID string) {
	newDrainComponent(c).chainGuardMarker(ctx, sessionID)
}

func (c *coordinator) reactionChainHasClaim(sessionID, claim string) bool {
	return newDrainComponent(c).reactionChainHasClaim(sessionID, claim)
}

// ReactionDebtSource implementations moved to reaction_component.go (the
// drainComponent host pattern); this file keeps the thin coordinator
// delegates.

func (c *coordinator) ClaimExternalDriver(ctx context.Context, sessionID string) error {
	return newDrainComponent(c).ClaimExternalDriver(ctx, sessionID)
}

func (c *coordinator) ReleaseExternalDriver(ctx context.Context, sessionID string) {
	newDrainComponent(c).ReleaseExternalDriver(ctx, sessionID)
}

func (c *coordinator) ReactionDebtExists(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).ReactionDebtExists(ctx, sessionID)
}

func (c *coordinator) WaitForHint(ctx context.Context, sessionID string, until time.Time) {
	newDrainComponent(c).WaitForHint(ctx, sessionID, until)
}

func (c *coordinator) RunMaintenanceSweep(ctx context.Context) {
	newDrainComponent(c).RunMaintenanceSweep(ctx)
}

func (c *coordinator) CLIScope(ctx context.Context, sessionID string) (CLIScopeState, error) {
	return newDrainComponent(c).CLIScope(ctx, sessionID)
}

func (c *coordinator) runningWorkOpen(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).runningWorkOpen(ctx, sessionID)
}

// Published generations fence logout/relogin ABA at the next exchange.
// Unrelated reloads rotate reuse, without revoking an active response.
func (c *coordinator) codexWebSocketOptions() []codexprovider.WebSocketOptions {
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).CodexWebSocketOptions()
}

func (c *coordinator) currentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).CurrentCodexWebSocketPolicy()
}

// coordinatorCodexWebSocketPolicyHost is the root-side port adapter: the
// coordinator surface the codex WebSocket policy manager needs, exported
// only because the destination package has to see it.
type coordinatorCodexWebSocketPolicyHost struct {
	coordinator *coordinator
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSManager() *nettransport.ResponsesWSManager {
	return h.coordinator.responsesWSManager()
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSSnapshot() (*config.Config, uint64) {
	return h.coordinator.responsesWSSnapshot()
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSResolveHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	return h.coordinator.responsesWSResolveHTTPClient(cfg, providerCfg)
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return h.coordinator.responsesWSFallbackOpenaiProvider(baseURL, apiKey, headers, client)
}

func (c *coordinator) responsesWSManager() *nettransport.ResponsesWSManager {
	return c.responsesWS
}

func (c *coordinator) responsesWSSnapshot() (*config.Config, uint64) {
	return c.cfg.Snapshot()
}

func (c *coordinator) responsesWSResolveHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	return modelkit.ResolveProviderHTTPClient(cfg, providerCfg)
}

func (c *coordinator) responsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return modelkit.BuildOpenaiProvider(baseURL, apiKey, headers, client)
}

func (c *coordinator) buildPublicResponsesProvider(pc config.ProviderConfig, baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).BuildPublicResponsesProvider(pc, baseURL, apiKey, headers, client)
}

// The per-session system prompt, reconciled with the mode of the call that is
// about to run it (A24, docs/plans/2026-10-01-in-turn-progress-guard.md §4).
//
// A session's system prompt is persisted once and then reused by every later
// turn, while a call's tool set is rebuilt from the live config every turn. In
// the wguard turn (774 steps) the prompt had been saved before the operator
// configured a worker, so it had no rule 7 "Orchestrator mode" — and the turn
// that inherited it had no edit tool to obey, only a saved instruction to use
// one. The two generations disagreed for a whole turn and the model crawled
// files instead of delegating.
//
// A stored prompt records its own mode by carrying (or not carrying) the rule
// 7 marker, and a call states its own mode through resolvedOverrides
// .orchestrator — the same bool that went into prompt.Build for the tool set
// it pinned. One mismatch costs one rebuild-and-re-save per mode change, and
// after that the per-session prompt is a working cache again. The priority
// order of resolveTurnConfig is untouched: this decides what BECOMES the
// per-session prompt, not what wins over a pinned one.

func (c *coordinator) sessionPromptForCall(ctx context.Context, sessionID string, pinned *resolvedOverrides) string {
	return newSessionStateComponent(c).sessionPromptForCall(ctx, sessionID, pinned)
}

// sessionPromptForCall returns the system prompt this call's turn must run on:
// the session's stored prompt when it was built in the same mode as the call's
// tool set, and the call's own pinned.systemPrompt persisted over it when the
// two disagree (one cache miss per mode change). A session that has no stored
// prompt yet, or a pinned snapshot that could not build one, falls back to
// resolveSessionSystemPrompt — the resolve-and-persist path, unchanged.
func (s *sessionStateComponent) sessionPromptForCall(ctx context.Context, sessionID string, pinned *resolvedOverrides) string {
	if pinned != nil {
		sess, err := s.sessions().Get(ctx, sessionID)
		if err != nil {
			return ""
		}
		if sess.SystemPrompt != "" {
			storedMode := strings.Contains(sess.SystemPrompt, prompt.OrchestratorRuleMarker)
			if storedMode == pinned.orchestrator {
				return sess.SystemPrompt
			}
			if pinned.systemPrompt != "" {
				if err := s.sessions().UpdateSystemPrompt(ctx, sessionID, pinned.systemPrompt); err != nil {
					// The turn still runs on the prompt that matches its tool
					// set; a failed save only costs a rebuild next turn.
					slog.Warn("coordinator: failed to re-save system prompt for a mode change", "sessionID", sessionID, "err", err)
				}
				return pinned.systemPrompt
			}
		}
	}
	return s.host.resolveSessionSystemPrompt(ctx, sessionID)
}

// Freeing per-session in-memory state (R3B-8). A long-lived web process meets
// many sessions; the rehomed per-session runtime state (the hint counter and
// external-driver mirror, batch_rehome_coord_runtime.go) and the
// coordinator's auto-turn maps used to live as long as the process. The 60s
// pass (RecheckPass) sweeps what nothing refers to any more. State that means something until a human message (Stop's
// suspension, the bg-shell cap counters) of a session that still exists is NOT
// freed: dropping it would re-arm automatic turns.

type sessionStateHost interface {
	sweepIdleSessions() int
	sweepDeletedSessionState(ctx context.Context)
	sweepArbiterEntries()
	sweepArbiterEntriesAt(now time.Time)
	resolveSessionSystemPrompt(ctx context.Context, sessionID string) string
}

type sessionStateComponent struct {
	host        sessionStateHost
	sessions    func() coresession.Service
	driverHints func() *batchDriverHints
	arb         func() *arbiter
}

func newSessionStateComponent(c *coordinator) *sessionStateComponent {
	return &sessionStateComponent{
		host:        c,
		sessions:    func() coresession.Service { return c.sessions },
		driverHints: func() *batchDriverHints { return c.driverHints },
		arb:         func() *arbiter { return &c.arb },
	}
}

func (c *coordinator) sweepIdleSessions() int {
	return newSessionStateComponent(c).sweepIdleSessions()
}

func (c *coordinator) sweepArbiterEntries() {
	newSessionStateComponent(c).sweepArbiterEntries()
}

func (c *coordinator) sweepArbiterEntriesAt(now time.Time) {
	newSessionStateComponent(c).sweepArbiterEntriesAt(now)
}

func (c *coordinator) sweepDeletedSessionState(ctx context.Context) {
	newSessionStateComponent(c).sweepDeletedSessionState(ctx)
}

func (c *coordinator) sweepSessionState(ctx context.Context) {
	newSessionStateComponent(c).sweepSessionState(ctx)
}

// sweepIdleSessions drops the rehomed per-session runtime state that holds
// nothing: no external-driver mirror entry (batch_rehome_coord_runtime.go).
// (The launch gate moved to the arbiter state in R-ARB-2; its idle half is
// swept by sweepArbiterEntries below.) A dropped entry's hint counter starts
// again at zero -- harmless only while the gate is open: while a dormant gate
// records HintSeen, dropping the counter makes gateShut read the reset
// counter as "a newer fact" and reopens the gate with no fact at all. Entries
// whose gate still holds hint state are kept until it does not. The check is
// best effort: a pace that records its hint between the gate check and the
// delete can still lose the counter once (the cost is one extra gate
// evaluation; the next pace re-records the hint).
func (s *sessionStateComponent) sweepIdleSessions() int {
	candidates := s.driverHints().idleCandidates()
	dropped := 0
	for _, id := range candidates {
		if s.arb().gateHoldsHint(id) {
			continue
		}
		// Re-check under the lock: a driver may have arrived while the
		// arbiter was asked.
		if s.driverHints().dropIfIdle(id) {
			dropped++
		}
	}
	return dropped
}

// sweepArbiterEntries frees arbiter entries whose every field is idle (no
// hold, no suspension, no cap counter, no chain, an expired-or-zero gate):
// such an entry is an open gate, exactly like a zero one. State that means
// something until a human message (a suspension, a spent cap) is kept.
func (s *sessionStateComponent) sweepArbiterEntries() {
	s.host.sweepArbiterEntriesAt(time.Now())
}

func (s *sessionStateComponent) sweepArbiterEntriesAt(now time.Time) {
	for _, id := range s.arb().sessionsWithState() {
		s.arb().dropIfIdle(id, now)
	}
}

// sweepDeletedSessionState drops the arbiter state of sessions that no longer
// exist (deleted): the only moment that state stops meaning anything without
// a human message.
func (s *sessionStateComponent) sweepDeletedSessionState(ctx context.Context) {
	if s.sessions() == nil {
		return
	}
	ids := s.arb().sessionsWithState()
	if len(ids) == 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := s.sessions().Get(readCtx, id); !errors.Is(err, sql.ErrNoRows) {
			continue // alive, or unreadable right now: keep it
		}
		s.arb().dropSession(id)
	}
}

// sweepSessionState is the RecheckPass hook: all halves, best effort.
func (s *sessionStateComponent) sweepSessionState(ctx context.Context) {
	s.host.sweepIdleSessions()
	s.host.sweepArbiterEntries()
	s.host.sweepDeletedSessionState(ctx)
}

// Sub-agent delegation and skill discovery: bodies moved to
// subagents_component.go (the subagentsComponent host pattern); this file
// keeps the thin coordinator delegates and the concrete host adapter.

func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	return newSubagentsComponent(c).runSubAgent(ctx, params)
}

func (c *coordinator) inheritBatchQuestionState(ctx context.Context, childSessionID string, call *SessionAgentCall) error {
	return newSubagentsComponent(c).inheritBatchQuestionState(ctx, childSessionID, call)
}

func (c *coordinator) agentFor(sessionID string) SessionAgent {
	return newSubagentsComponent(c).agentFor(sessionID)
}

func (c *coordinator) releaseDriverIfScopeClosed(childSessionID string) {
	newSubagentsComponent(c).releaseDriverIfScopeClosed(childSessionID)
}

func (c *coordinator) releaseDriverGeneration(childSessionID string, generation uint64) {
	newSubagentsComponent(c).releaseDriverGeneration(childSessionID, generation)
}

type coordinatorSubAgentHost struct{ coordinator *coordinator }

func newSubagentsComponent(c *coordinator) *subagentsComponent {
	return &subagentsComponent{host: coordinatorSubAgentHost{coordinator: c}}
}

func (h coordinatorSubAgentHost) readyWg() *readyGate { return &h.coordinator.readyWg }
func (h coordinatorSubAgentHost) sessions() core.Service {
	return h.coordinator.sessions
}
func (h coordinatorSubAgentHost) cfg() *config.ConfigStore { return h.coordinator.cfg }
func (h coordinatorSubAgentHost) permissions() permission.Service {
	return h.coordinator.permissions
}

func (h coordinatorSubAgentHost) notify() pubsub.Publisher[notify.Notification] {
	return h.coordinator.notify
}

func (h coordinatorSubAgentHost) subAgentDrivers() *toolwrap.Registry {
	return h.coordinator.subAgentDrivers
}

func (h coordinatorSubAgentHost) currentAgent() SessionAgent {
	return h.coordinator.currentAgent
}

func (h coordinatorSubAgentHost) resolveRequestedDelegationModel(ctx context.Context, cfg *config.Config, parentID string, role RequestedModelRole) (delegationRoleModel, error) {
	return h.coordinator.resolveRequestedDelegationModel(ctx, cfg, parentID, role)
}

func (h coordinatorSubAgentHost) batchHeldQuestionResumeHint(ctx context.Context, owner string) string {
	return h.coordinator.batchHeldQuestionResumeHint(ctx, owner)
}

func (h coordinatorSubAgentHost) batchHeldQuestionResumeRefusal(ctx context.Context, owner, child string) string {
	return h.coordinator.batchHeldQuestionResumeRefusal(ctx, owner, child)
}

func (h coordinatorSubAgentHost) resetConsecutiveResume(sessionID string) {
	h.coordinator.resetConsecutiveResume(sessionID)
}

func (h coordinatorSubAgentHost) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	return h.coordinator.buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

func (h coordinatorSubAgentHost) buildCredentialModel(ctx context.Context, cfg *config.Config, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error) {
	return h.coordinator.buildCredentialModel(ctx, cfg, creds, choice)
}

func (h coordinatorSubAgentHost) resolveSubAgentModelOverride(ctx context.Context, sessionID string) (*Model, error) {
	return h.coordinator.resolveSubAgentModelOverride(ctx, sessionID)
}

func (h coordinatorSubAgentHost) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	return h.coordinator.workerSubAgentActiveForCall(ctx, cfg)
}

func (h coordinatorSubAgentHost) refreshSubAgentSlotModel(ctx context.Context, cfg *config.Config, role config.SelectedModelType, model Model) (Model, bool) {
	return h.coordinator.refreshSubAgentSlotModel(ctx, cfg, role, model)
}

func (h coordinatorSubAgentHost) rebuildInputs(providerID string) (*config.Config, config.ProviderConfig, error) {
	return h.coordinator.rebuildInputs(providerID)
}

func (h coordinatorSubAgentHost) rebuildPinnedModel(ctx context.Context, cfg *config.Config, model Model, providerCfg config.ProviderConfig, isSubAgent bool) (Model, error) {
	return h.coordinator.rebuildPinnedModel(ctx, cfg, model, providerCfg, isSubAgent)
}

func (h coordinatorSubAgentHost) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	return h.coordinator.refreshOAuth2Token(ctx, providerCfg)
}

func (h coordinatorSubAgentHost) runAwaitingAdmission(ctx context.Context, agent SessionAgent, call SessionAgentCall) (result *fantasy.AgentResult, err error, queued bool) {
	return h.coordinator.runAwaitingAdmission(ctx, agent, call)
}

func (h coordinatorSubAgentHost) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error, rebuildCall func() error) error {
	return h.coordinator.runWithUnauthorizedRetry(ctx, providerCfg, fn, rebuildCall)
}

func (h coordinatorSubAgentHost) isUnauthorized(err error) bool {
	return modelkit.IsUnauthorized(err)
}

func (h coordinatorSubAgentHost) childQuestionPreamble(childSessionID string) string {
	return h.coordinator.childQuestionPreamble(childSessionID)
}

func (h coordinatorSubAgentHost) rebuiltChildQuestionState(ctx context.Context, sessionID string) (*batchChildQuestionState, error) {
	return h.coordinator.rebuiltChildQuestionState(ctx, sessionID)
}

// AutoTurnHolder is the OPTIONAL Coordinator surface a rerun uses to keep
// automatic (Drain) turns off a session while it cancels the live turn,
// truncates history and hands the reservation to the replacement turn: a
// cancelled turn's release would otherwise start a paid Drain that races the
// rerun (it fails "still stopping", or its reaction is truncated and re-pended
// and paid twice). Type-asserted by the caller like ParkedSubAgentWorkReporter
// so the many Coordinator fakes stay untouched.
type AutoTurnHolder interface {
	// HoldAutomaticTurns refuses automatic turns for sessionID until the
	// returned func is called (idempotent). Holds nest: automatic turns resume
	// when the last hold is released. While held the launch predicate answers
	// drainPaced with no clock (a temporary hold, never "deferred": a delegated
	// child's parent keeps the delegation open), so a skipped wake is retried by
	// the re-check tick and the release retry after the hold ends.
	HoldAutomaticTurns(sessionID string) (release func())
}

// HoldAutomaticTurns implements AutoTurnHolder.
func (c *coordinator) HoldAutomaticTurns(sessionID string) (release func()) {
	return c.arb.hold(sessionID)
}

// automaticTurnsHeld reports whether a rerun currently holds sessionID's
// automatic turns.
func (c *coordinator) automaticTurnsHeld(sessionID string) bool {
	return c.arb.held(sessionID)
}

// wakeSession: the single wake primitive (docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.3.4, attempts design 1.6). It persists nothing
// itself: by the time a caller reaches it the fact is durably committed (a
// batch completion row or a session_notices row). Its job
// is to decide -- through the ONE launch predicate drainDecision -- whether a
// Drain call is submitted to the session's owning driver: idle becomes owner
// like any call, busy queues (mailbox_queue.go's mergeQueuedCall). The
// Drain's own turn-start pull moves the notice into history. Nothing runs
// after Run returns: the attempt is accounted by the turn loop that ran it
// (drain_attempt.go).

func (c *coordinator) wakeSession(ctx context.Context, sessionID string, fact bool) error {
	return wakectl.WakeSession(c.wakeLaunchPorts(), ctx, sessionID, fact)
}

func (c *coordinator) handleDrainBuildError(sessionID string, buildErr error) error {
	return wakectl.HandleDrainBuildError(c.wakeBuildErrorPorts(), sessionID, buildErr)
}

// Stage 5a: the wake-schedule side of the CLI lifetime policy (operator
// decisions, docs/plans/2026-09-24-agent-wakes-and-async-job-control.md
// sec.5): an ACTIVE once schedule is open work that holds `rush run` open,
// an ACTIVE loop schedule is not -- the CLI loop cancels the session's
// loop schedules when its scope closes with nothing else open. Storage
// reads/writes stay in session.WakeScheduleStore (SCHED-05); this file only
// filters and cancels through it.

// LoopWakeCanceler is the optional interface the CLI loop asserts its
// ReactionDebtSource to (not part of ReactionDebtSource itself: every test
// fake would have to grow the method for nothing).
type LoopWakeCanceler interface {
	// CancelLoopWakeSchedules cancels every ACTIVE loop schedule owned by
	// sessionID and returns how many rows it moved to cancelled.
	CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error)
}

func (c *coordinator) openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	return c.wakeWorker().OpenOnceWakeSchedules(ctx, sessionID)
}

func (c *coordinator) HasOpenOnceWakeSchedule(ctx context.Context, sessionID string) (bool, error) {
	once, err := c.openOnceWakeSchedules(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return len(once) > 0, nil
}

func (c *coordinator) CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error) {
	return c.wakeWorker().CancelLoopWakeSchedules(ctx, sessionID)
}
