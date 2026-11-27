package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Per-attempt evidence capture for coordinator_run.go's retry loop —
// split out because coordinator_run.go sits just under the repo's
// 1000-line limit (CLAUDE.md). Pure addition, no behavior moved.

// attemptEvidence is ONE run() attempt's capture target for the
// SessionAgentCall.OnAssistantMessageCreated callback (R3-1, round 6).
//
// coordinator_run.go used to write every attempt's callback into ONE
// closure variable shared by the whole runInternal call. Two defects
// followed. (A) Stale evidence: when Run queues the call (session busy)
// it returns (nil, nil) WITHOUT firing the callback, so the variable
// still held the PREVIOUS attempt's row ID and the retry classifiers
// judged the queued attempt on evidence it never produced -- requeueing
// a duplicate continuation behind the already-queued one. (B) Data
// race: mailbox.submit copies the call struct by value, so the queued
// copy carries the same closure; when a dispatcher goroutine later
// drains it, PrepareStep writes that shared variable with no
// synchronization against the coordinator goroutine's classifier
// reads.
//
// A fresh instance per attempt closes both by construction: a queued
// attempt's instance stays empty, so the classifiers see "" (no owned
// evidence) and refuse; a late callback from an async dispatcher
// writes into an already-abandoned instance whose state is
// mutex-ordered and sealed, so no read can race it and nothing acts
// on it.
type attemptEvidence struct {
	mu     sync.Mutex
	sealed bool
	msgID  string
}

// newAttemptEvidence returns an unsealed instance ready to record.
func newAttemptEvidence() *attemptEvidence { return &attemptEvidence{} }

// record is the OnAssistantMessageCreated sink for this attempt. The
// method value captures the instance itself, so a SessionAgentCall copy
// that outlives the synchronous run (a queued call drained much later by
// another goroutine) can only ever write HERE -- never into a later
// attempt's instance. Writes after resolve are dropped: resolve runs
// synchronously the moment run() returns, so any later write comes from
// an async dispatcher whose outcome is no longer this call's to
// classify.
func (e *attemptEvidence) record(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.msgID = id
}

// resolve seals the attempt and returns the assistant row ID it reported
// while running. Called exactly once per attempt, synchronously after
// run() returns; the returned string is plain data owned by the calling
// goroutine from then on.
func (e *attemptEvidence) resolve() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sealed = true
	return e.msgID
}

// The 60-second host-level pass (doc sec.3.4 rule (b), sec.3.5's "the ONLY
// inter-process fallback"): hints live only inside this process, so a host
// re-checks, once a minute: every session whose launch is waiting (a Drain refused by an
// admission gate, paced by the launch gate, held by a rerun, or whose launch
// decision could not be read -- wakeSession's verdict switch keeps it in the
// set), and the sweep of idle per-session state. The wakes run detached (one
// per session, bounded concurrency) so a slow turn never delays the next tick.
// The web process starts the ticker (SetPersistentMode) and so does a `rush
// run` (ClaimExternalDriver): the CLI root itself is a hint-only no-op for the
// pass, but its delegated children's retries ride it. A `rush run` also runs
// RunMaintenanceSweep once at loop start and re-reads the DB itself
// (WaitForHint, at most every 5s) while its scope is open.

// drainctlPacingVar adapts one pacing knob that now lives as a private
// atomic in runtime/exec/drainctl: a small Load/Swap/Store-only wrapper so
// root tests keep touching the same knob by its historical names, without
// duplicate state or exported atomics.
type drainctlPacingVar struct {
	load  func() int64
	swap  func(int64) int64
	store func(int64)
}

func (v drainctlPacingVar) Load() int64        { return v.load() }
func (v drainctlPacingVar) Swap(x int64) int64 { return v.swap(x) }
func (v drainctlPacingVar) Store(x int64)      { v.store(x) }

var recheckPassIntervalNS = drainctlPacingVar{drainctl.LoadRecheckPassIntervalNS, drainctl.SwapRecheckPassIntervalNS, drainctl.StoreRecheckPassIntervalNS}

// maxConcurrentRecheckWakes bounds how many detached recheck-pass wakes run
// at once. A wake that finds the bound full is put back in the recheck set
// and retried by the next pass.
const maxConcurrentRecheckWakes = 4

// recheckPassSweepSeam is a test-only hook called once per RecheckPass, right
// before the maintenance sweep. Empty in every production path; atomic
// because a never-stopped ticker of an earlier test may tick while a later
// test swaps it.
var recheckPassSweepSeam atomic.Pointer[func()]

// addToRecheckSet records sessionID so the next RecheckPass retries its
// Drain submission instead of losing the wake (doc sec.3.4 rule (b)).
func (c *coordinator) addToRecheckSet(sessionID string) {
	if sessionID == "" {
		return
	}
	c.recheckMu.Lock()
	if c.recheckSet == nil {
		c.recheckSet = make(map[string]struct{})
	}
	c.recheckSet[sessionID] = struct{}{}
	c.recheckMu.Unlock()
}

// clearFromRecheckSet removes sessionID from the recheck set (R12-H2b
// §2.3): a binding-loss refusal must not re-spin the 60s pass — the
// binding cannot come back on a timer, and a newly admitted explicit
// epoch re-evaluates on its own.
func (c *coordinator) clearFromRecheckSet(sessionID string) {
	if sessionID == "" {
		return
	}
	c.recheckMu.Lock()
	delete(c.recheckSet, sessionID)
	c.recheckMu.Unlock()
}

// drainRecheckSet atomically empties and returns the current recheck set.
func (c *coordinator) drainRecheckSet() []string {
	c.recheckMu.Lock()
	defer c.recheckMu.Unlock()
	if len(c.recheckSet) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.recheckSet))
	for id := range c.recheckSet {
		out = append(out, id)
	}
	c.recheckSet = nil
	return out
}

// RecheckPass runs one iteration of the 60s host-level pass: the retention
// purge, and a wake for every session in the recheck set (a hint that could not be acted on immediately). The wakes are
// DETACHED (launchRecheckWake): a Drain turn can take minutes, and the pass
// must return promptly so the next tick's sweep/purge/recheck run on
// schedule. Exported and independently callable so tests can drive it
// without waiting for the real ticker.
func (c *coordinator) RecheckPass(ctx context.Context) {
	if seam := recheckPassSweepSeam.Load(); seam != nil {
		(*seam)()
	}
	c.RunMaintenanceSweep(ctx)
	// Parked-delegation re-evaluation is batch delivery now: a delegation's
	// outcome is delivered by the batch executor, and a release-shaped retry
	// reaches this pass through the recheck set (noteSubAgentChildRunEnded).
	for _, sessionID := range c.drainRecheckSet() {
		if !c.launchRecheckWake(ctx, sessionID) {
			// Already being woken, or the concurrency bound is full: keep the
			// session for the next pass instead of forgetting the wake.
			c.addToRecheckSet(sessionID)
		}
	}
	// Last: a session the loop above just queued keeps its entry (its gate is
	// not zero, or it has jobs); only idle state is freed.
	c.sweepSessionState(ctx)
}

// launchRecheckWake starts sessionID's wake on its own goroutine and returns
// at once, or returns false without launching when a recheck wake for the
// session is already in flight or maxConcurrentRecheckWakes are running. The
// wake outlives the pass but not ctx (the ticker's context, cancelled by
// StopRecheckTicker/CancelAll). wakeSession re-adds sessionID to the recheck
// set itself while the launch is still not allowed (paced, deferred with a
// re-check, refused, an unreadable decision), so the launcher never duplicates
// that decision.
func (c *coordinator) launchRecheckWake(ctx context.Context, sessionID string) bool {
	c.recheckMu.Lock()
	if _, busy := c.recheckWakeInFlight[sessionID]; busy || len(c.recheckWakeInFlight) >= maxConcurrentRecheckWakes {
		c.recheckMu.Unlock()
		return false
	}
	if c.recheckWakeInFlight == nil {
		c.recheckWakeInFlight = make(map[string]struct{})
	}
	c.recheckWakeInFlight[sessionID] = struct{}{}
	c.recheckWakes.Add(1)
	c.recheckMu.Unlock()
	go func() {
		defer c.recheckWakes.Done()
		defer func() {
			c.recheckMu.Lock()
			delete(c.recheckWakeInFlight, sessionID)
			c.recheckMu.Unlock()
		}()
		if err := c.wakeSession(ctx, sessionID, false); err != nil {
			slog.Debug("coordinator: recheck pass wake attempt did not complete", "session_id", sessionID, "err", err)
		}
	}()
	return true
}

// waitRecheckWakes blocks until every detached recheck-pass wake launched so
// far has returned.
func (c *coordinator) waitRecheckWakes() {
	c.recheckWakes.Wait()
}

// StartRecheckTicker starts the 60s background pass exactly once per
// coordinator (idempotent), stopped by StopRecheckTicker (wired into
// CancelAll). Started by the web process (SetPersistentMode) and by a `rush
// run` loop's ClaimExternalDriver (see the file header).
func (c *coordinator) StartRecheckTicker() {
	c.recheckOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.recheckStop = cancel
		done := make(chan struct{})
		c.recheckDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Duration(recheckPassIntervalNS.Load()))
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.RecheckPass(ctx)
				}
			}
		}()
	})
}

// StopRecheckTicker stops the 60s pass started by StartRecheckTicker, if
// any. Safe to call even if it was never started.
func (c *coordinator) StopRecheckTicker() {
	if c.recheckStop != nil {
		c.recheckStop()
	}
}

// System prompt access, summarize snapshot and queue handling, and prompt
// queue introspection. Extracted from coordinator.go — pure code move,
// bodies unchanged.

func (c *coordinator) GetSystemPrompt() string {
	return c.currentAgent.SystemPrompt()
}

func (c *coordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Build the default smart model from config for prompt building.
	cfg, _ := c.cfg.Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]

	smartModel, _, err := c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		return "", fmt.Errorf("failed to build default smart model: %w", err)
	}

	// Use the same pinned cfg captured above (task #341, P1-1) instead of
	// re-reading c.cfg.Config() live inside workerSubAgentActive.
	return c.prompt.Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, c.cfg, cfg, c.workerSubAgentActiveForCall(ctx, cfg))
}

// BuildSystemPromptForSession builds a system prompt for a specific session,
// using the session's model configuration (from DB overrides or config defaults).
// This ensures the system prompt matches the model that will actually run for this session.
func (c *coordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Resolve the session's model configuration (DB overrides → config defaults).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models: %w", err)
	}

	// Reuse the system prompt resolveSessionModels already built from its own
	// single pinned config snapshot (task #341, P1-1), rather than rebuilding
	// it here from a second, separately-timed live cfg read
	// (c.workerSubAgentActive() with no argument). A reload landing between
	// the two builds could otherwise make this second build's
	// WorkerAvailable flag disagree with resolved.smart, which was pinned
	// from an earlier generation.
	return resolved.systemPrompt, nil
}

func (c *coordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return c.sessions.UpdateSystemPrompt(ctx, sessionID, prompt)
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

// Summarize implements Coordinator.
func (c *coordinator) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	// If the caller didn't provide a snapshot (e.g., tests), resolve one now.
	// This maintains backward compatibility while encouraging the correct pattern.
	if snapshot == nil {
		var err error
		snapshot, err = c.buildSummarizeSnapshot(ctx, sessionID)
		if err != nil {
			return err
		}
	}

	providerCfg := snapshot.ProviderCfg
	if providerCfg.ID == "" {
		var err error
		providerCfg, err = c.currentProviderConfig(snapshot.Model.ModelCfg.Provider)
		if err != nil {
			return err
		}
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return err
	}

	currentSnapshot := snapshot
	rebuildSnapshot := func() error {
		cfg, freshProviderCfg, err := c.rebuildInputs(currentSnapshot.Model.ModelCfg.Provider)
		if err != nil {
			return err
		}
		freshModel, err := c.rebuildPinnedModel(ctx, cfg, currentSnapshot.Model, freshProviderCfg, false)
		if err != nil {
			return fmt.Errorf("failed to rebuild summarize model: %w", err)
		}
		currentSnapshot = &SummarizeSnapshot{
			Model:           freshModel,
			ProviderOptions: modelkit.GetProviderOptions(sessionID, freshModel, freshProviderCfg),
			PromptPrefix:    currentSnapshot.PromptPrefix,
			ProviderCfg:     freshProviderCfg,
		}
		providerCfg = freshProviderCfg
		return nil
	}

	if providerCfg.OAuthToken != nil && providerCfg.OAuthToken.IsExpired() {
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
		} else if err := rebuildSnapshot(); err != nil {
			return err
		}
	}

	summarize := func() error {
		return c.currentAgent.Summarize(ctx, sessionID, currentSnapshot)
	}

	return c.runWithUnauthorizedRetry(ctx, providerCfg, summarize, rebuildSnapshot)
}

// buildSummarizeSnapshot creates an immutable snapshot for a summarize operation,
// resolving the model from the target session's persisted models (or config defaults
// for sessions without overrides). This ensures the entire summarize operation
// uses consistent configuration regardless of concurrent session model changes.
func (c *coordinator) buildSummarizeSnapshot(ctx context.Context, sessionID string) (*SummarizeSnapshot, error) {
	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve session models for summarize: %w", err)
	}

	// Get the provider config for this model.
	providerCfg := resolved.providerCfg
	if providerCfg.ID == "" {
		return nil, errModelProviderNotConfigured
	}

	// Build provider options from the resolved model.
	opts := modelkit.GetProviderOptions(sessionID, resolved.smart, providerCfg)

	// Use the prompt prefix from the resolved snapshot (provider config's
	// prefix, already set by resolveSessionModels).
	promptPrefix := resolved.promptPrefix
	if promptPrefix == "" {
		promptPrefix = providerCfg.SystemPromptPrefix
	}

	return &SummarizeSnapshot{
		Model:           resolved.smart,
		ProviderOptions: opts,
		PromptPrefix:    promptPrefix,
		ProviderCfg:     providerCfg,
	}, nil
}

func (c *coordinator) SummarizeQueued(sessionID string) bool {
	return c.currentAgent.SummarizeQueued(sessionID)
}

func (c *coordinator) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return c.currentAgent.TakeSummarizeQueue(sessionID)
}

func (c *coordinator) CancelQueuedSummarize(sessionID string) {
	c.currentAgent.CancelQueuedSummarize(sessionID)
}

type StandaloneSummarizer interface {
	SummarizeWithRole(context.Context, string, config.SelectedModelType) error
}

type standaloneSummarizer struct {
	cfg      *config.ConfigStore
	sessions session.Service
	messages message.Service
}

func NewStandaloneSummarizer(cfg *config.ConfigStore, sessions session.Service, messages message.Service) StandaloneSummarizer {
	return &standaloneSummarizer{cfg: cfg, sessions: sessions, messages: messages}
}

func SummaryRoleAllowed(role config.SelectedModelType) bool {
	switch role {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return false
	}
}

func (s *standaloneSummarizer) SummarizeWithRole(ctx context.Context, sessionID string, role config.SelectedModelType) error {
	if !SummaryRoleAllowed(role) {
		return fmt.Errorf("unsupported standalone summary role %q", role)
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	cfg, _ := s.cfg.Snapshot()
	selected, _, err := requestedRoleSlot(cfg, sess, RequestedModelRole(role))
	if err != nil {
		return fmt.Errorf("%s model is unavailable: %w", role, err)
	}
	providerCfg, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return fmt.Errorf("%s provider %q is not configured", role, selected.Provider)
	}
	if providerCfg.Type == cliprovider.ProviderType {
		return fmt.Errorf("standalone compaction does not support CLI provider %q: CLI tool authority cannot be safely restricted on this path", selected.Provider)
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return err
	}
	dataDir := ""
	if cfg.Options != nil {
		dataDir = strings.TrimSpace(cfg.Options.DataDirectory)
	}
	if dataDir == "" {
		return fmt.Errorf("standalone compaction requires a configured data directory for session locking")
	}
	coord := &coordinator{cfg: s.cfg, sessions: s.sessions, messages: s.messages}
	model, _, err := coord.buildModelsFromCfg(ctx, cfg, selected, selected, false)
	if err != nil {
		return fmt.Errorf("%s model is unavailable: %w", role, err)
	}
	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		DisableAutoSummarize: true,
		Sessions:             s.sessions,
		Messages:             s.messages,
		Config:               s.cfg,
		DataDirectory:        dataDir,
		Tools:                []fantasy.AgentTool{},
	})
	snapshot := &SummarizeSnapshot{
		Model:           model,
		ProviderOptions: modelkit.GetProviderOptions(sessionID, model, providerCfg),
		PromptPrefix:    providerCfg.SystemPromptPrefix,
		ProviderCfg:     providerCfg,
	}
	// Preserve the selected worker's session identity and configured effort for
	// direct Codex requests (cache affinity and reasoning dispatch).
	summaryCtx := ctx
	if providerCfg.ID == "openai-codex" {
		summaryCtx = context.WithValue(summaryCtx, cliprovider.SessionIDContextKey, sessionID)
		summaryCtx = context.WithValue(summaryCtx, cliprovider.ReasoningEffortContextKey, provauth.TurnSmartReasoningEffort(model))
		summaryCtx = context.WithValue(summaryCtx, cliprovider.ServiceTierContextKey, turnstep.TurnSmartServiceTier(model))
	}
	return agent.(*sessionAgent).SummarizeNow(summaryCtx, sessionID, snapshot)
}
