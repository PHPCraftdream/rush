package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/app/appbatch"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	xstrings "github.com/charmbracelet/x/exp/strings"
)

// Small read-only accessors on *App: config/config-store exposure,
// the event-broker subscription helpers, and the agent-notification
// broker. Kept out of app.go so the type and constructor stand alone
// there.

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[any] {
	return app.events.Subscribe(ctx)
}

func (app *App) SendEvent(msg any) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// BatchHostStore returns the App host store
// (sessionhosts.HostStore): the job-independent host identity the batch
// runtime install registers and releases. Nil without a database writer
// or without a data dir; callers treat nil as "no batch host identity",
// never panic.
func (app *App) BatchHostStore() *sessionhosts.HostStore {
	return app.batchHostStore()
}

// BatchNoticeStore returns the job-independent session-notice store
// (session.SessionNoticeStore) wired to the BatchHostStore identity and
// liveness. Nil without a database writer.
func (app *App) BatchNoticeStore() *session.SessionNoticeStore {
	return app.batchNoticeStore()
}

// WakeScheduleStore returns the stage-4b durable wake-schedule store
// (same construction rules: nil under SkipAgentSetup or without a data
// dir; readers treat nil as "no schedule data available").
func (app *App) WakeScheduleStore() *sessionwake.WakeScheduleStore {
	return app.wakeScheduleStore
}

// SetWakeScheduleStoreForTest wires a WakeScheduleStore into an App built
// WITHOUT InitCoderAgent (the &App{...} literal pattern); test-only.
func (app *App) SetWakeScheduleStoreForTest(s *sessionwake.WakeScheduleStore) {
	app.wakeScheduleStore = s
}

// SetDataDirForTest points the session-activity reader's lock inspection at
// a data directory chosen by the test (the &App{...} literal pattern; the
// reader derives the lock path itself, decision 10).
func (app *App) SetDataDirForTest(dir string) {
	app.dataDir = dir
}

// runEnv snapshots the run cluster's capability set at wrapper entry.
// Closure fields resolve their App field lazily on purpose: the underlying
// fields can be reassigned after New (test fakes, InitCoderAgent's
// self-heal, the lazily built batch-fault writer), so each read must
// happen at the same moment the old method-body read did.
func (app *App) runEnv() RunEnv {
	// A bare &App{} literal has no DB func: env.DB stays nil so every
	// `env.DB == nil` check keeps the old `app.DB == nil` meaning.
	var db func() *sql.DB
	if app.DB != nil {
		db = func() *sql.DB { return app.DB() }
	}
	return RunEnv{
		WorkspaceRoot:         app.workspaceRoot,
		Config:                app.config,
		Sessions:              func() coresession.Service { return app.Sessions },
		Messages:              func() message.Service { return app.Messages },
		Coord:                 func() agent.Coordinator { return app.AgentCoordinator },
		Perms:                 func() permission.Service { return app.Permissions },
		Pump:                  func() *runq.RunQueuePump { return app.RunQueuePump },
		WakeStore:             func() *sessionwake.WakeScheduleStore { return app.wakeScheduleStore },
		DB:                    db,
		DataDir:               func() string { return app.dataDir },
		MCPOwner:              func() *mcp.Owner { return app.mcpOwner },
		ProcessHome:           app.ProcessHome,
		SessionActivity:       app.SessionActivity,
		DiskBindings:          app.resolveDiskBindings,
		ResolveModelOverrides: app.resolveModelOverridesForNonInteractive,
		BatchFaultWriter:      func() *appbatch.FaultEnvelopeWriter { return app.batchFaultWriter },
		SetBatchFaultOutput: func(f func(error)) {
			app.batchFaultOutputMu.Lock()
			app.batchFaultOutput = f
			app.batchFaultOutputMu.Unlock()
		},
		GetBatchFaultOutput: func() func(error) {
			app.batchFaultOutputMu.Lock()
			defer app.batchFaultOutputMu.Unlock()
			return app.batchFaultOutput
		},
		BatchCutoverWriter: app.batchCutoverWriter,
		BatchCutoverReader: app.batchCutoverReader,
		BatchFinishedWork:  app.batchFinishedWork,
	}
}

// ExecuteRun computes a single agent turn and returns the result envelope
// without rendering it (see RunMode for the output shapes that shape the
// streaming behaviour). Streaming output goes to req.Stdout, diagnostics to
// req.Stderr; nil falls back to io.Discard. For RunModeJSON the caller is
// responsible for encoding the returned *RunResult.
func (app *App) ExecuteRun(ctx context.Context, req RunRequest) (result *RunResult, runErr error) {
	return executeRun(app.runEnv(), ctx, req)
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes. It is a thin
// wrapper over ExecuteRun: it supplies the process streams as defaults
// and renders the JSON envelope for RunModeJSON.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	_, err := runNonInteractiveWithAsyncResults(app.runEnv(), ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
	return err
}

// RunNonInteractiveWithResult runs one agent turn, writes its output, and
// returns the structured result when one is available.
func (app *App) RunNonInteractiveWithResult(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (*RunResult, error) {
	return runNonInteractiveWithAsyncResults(app.runEnv(), ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
}

// ProcessHome reports whether this process owns its own data directory
// (WS-1, #1142 step C) -- the flag that decides whether legacy unbound (”)
// rows are drivable here. It is app.New's copy of the single source
// config.WorkspaceHome() (false only for a linked worktree on the shared data directory, SD-D #1143), NOT a derivation from the
// workspace root: a git checkout has a non-empty root and is still a home
// process today. A nil App (test literals) reads as home, the pre-WS-1 world.
func (app *App) ProcessHome() bool {
	return processHome(app)
}

// Permanent batch-CLI accessors: thin same-signature wrappers over the Sh5
// batch free functions (chunk 4 of lane 0b-a).
func (app *App) BatchCLIStore() (*appbatch.BatchCLIStore, error) {
	return appbatch.NewBatchCLIStore(app.batchCLIParams())
}

// BatchCLISpecWithQuestionRoute stamps the route chosen by trusted intake.
func (app *App) BatchCLISpecWithQuestionRoute(ctx context.Context, owner, messageID string, spec jobbatch.Spec, questionRoute bool) (jobbatch.Spec, func(), error) {
	return appbatch.BatchCLISpecWithQuestionRoute(app.batchCLIParams(), ctx, owner, messageID, spec, questionRoute)
}

// BatchSubmitRefWithQuestionRoute is BatchSubmit with a caller-chosen submit
// reference (see agent.BatchInstall.SubmitCLIRef) and the intake's answer-route
// decision.
func (app *App) BatchSubmitRefWithQuestionRoute(ctx context.Context, owner string, spec jobbatch.Spec, ref func(root string) string, questionRoute bool) (string, error) {
	return appbatch.BatchSubmitRefWithQuestionRoute(app.batchCLIParams(), ctx, owner, spec, ref, questionRoute)
}

// BatchSettleDebt settles the owner's notice debt for a terminal root without
// releasing the root, so its live output stays readable.
func (app *App) BatchSettleDebt(ctx context.Context, owner, root string) error {
	return appbatch.BatchSettleDebt(app.batchCLIParams(), ctx, owner, root)
}

func (app *App) BatchWait(ctx context.Context, owner, root string) (db.BatchNode, error) {
	return appbatch.BatchWait(app.batchCLIParams(), ctx, owner, root)
}

// parseModelStr parses a model string into provider filter and model ID.
// Format: "model-name" or "provider/model-name" or "synthetic/moonshot/kimi-k2".
// This function only checks if the first component is a valid provider name; if not,
// it treats the entire string as a model ID (which may contain slashes).
func parseModelStr(providers map[string]config.ProviderConfig, modelStr string) (providerFilter, modelID string) {
	parts := strings.Split(modelStr, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	// Check if the first part is a valid provider name
	if _, ok := providers[parts[0]]; ok {
		return parts[0], strings.Join(parts[1:], "/")
	}

	// First part is not a valid provider, treat entire string as model ID
	return "", modelStr
}

// modelMatch represents a found model. known is false for the unverified
// passthrough case below (an explicit provider/model that matched nothing in
// that provider's catalog) — callers use it to warn the operator without
// blocking the selection.
type modelMatch struct {
	provider string
	modelID  string
	known    bool
}

func findModels(providers map[string]config.ProviderConfig, smartModel, fastModel string) ([]modelMatch, []modelMatch, error) {
	smartProviderFilter, smartModelID := parseModelStr(providers, smartModel)
	fastProviderFilter, fastModelID := parseModelStr(providers, fastModel)

	// Validate provider filters exist.
	for _, pf := range []struct {
		filter, label string
	}{
		{smartProviderFilter, "smart"},
		{fastProviderFilter, "fast"},
	} {
		if pf.filter != "" {
			if _, ok := providers[pf.filter]; !ok {
				return nil, nil, fmt.Errorf("%s model: provider %q not found in configuration. Use 'rush models' to list available models", pf.label, pf.filter)
			}
		}
	}

	// Find matching models in a single pass.
	var smartMatches, fastMatches []modelMatch
	for name, provider := range providers {
		if provider.Disable {
			continue
		}
		for _, m := range provider.Models {
			if filter(smartModelID, smartProviderFilter, m.ID, name) {
				smartMatches = append(smartMatches, modelMatch{provider: name, modelID: m.ID, known: true})
			}
			if filter(fastModelID, fastProviderFilter, m.ID, name) {
				fastMatches = append(fastMatches, modelMatch{provider: name, modelID: m.ID, known: true})
			}
		}
	}

	// Unverified passthrough: an explicit "provider/model" that matched
	// nothing in that provider's cached catalog is still accepted, since the
	// catalog can be stale (the provider added a model after our last sync —
	// e.g. zai/glm-5.3-flash existing live before `rush providers update`
	// pulled it in) or entirely absent (openai-compat providers with no
	// declared models). Only eligible when a provider WAS named explicitly:
	// a bare "model" search across every provider's catalog has no anchor to
	// fall back to, so a typo there still fails loudly instead of guessing
	// which provider was meant.
	if smartModelID != "" && smartProviderFilter != "" && len(smartMatches) == 0 {
		smartMatches = append(smartMatches, modelMatch{provider: smartProviderFilter, modelID: smartModelID, known: false})
	}
	if fastModelID != "" && fastProviderFilter != "" && len(fastMatches) == 0 {
		fastMatches = append(fastMatches, modelMatch{provider: fastProviderFilter, modelID: fastModelID, known: false})
	}

	return smartMatches, fastMatches, nil
}

func filter(modelFilter, providerFilter, model, provider string) bool {
	return modelFilter != "" && strings.EqualFold(model, modelFilter) &&
		(providerFilter == "" || strings.EqualFold(provider, providerFilter))
}

// ResolveModel does the same smart lookup that --model on `rush run` does,
// but exposed as a public method so the `rush models set` CLI can share
// the rules. modelStr is "model" or "provider/model"; the returned values
// are the unique provider id and the canonical model id from its catalog.
// known is false when modelID was accepted as an unverified "provider/model"
// passthrough rather than a real catalog hit — callers should surface that
// to the operator (see findModels' doc comment for why this is allowed at
// all). Returns an error on no-match or ambiguity.
func (app *App) ResolveModel(modelStr string) (provider, modelID string, known bool, err error) {
	return ResolveConfiguredModel(app.config.Config(), modelStr)
}

// ResolveConfiguredModel resolves a model without starting an application runtime.
func ResolveConfiguredModel(cfg *config.Config, modelStr string) (provider, modelID string, known bool, err error) {
	if cfg == nil || cfg.Providers == nil {
		return "", "", false, fmt.Errorf("model providers are not configured")
	}
	providers := cfg.Providers.Copy()
	matches, _, fErr := findModels(providers, modelStr, "")
	if fErr != nil {
		return "", "", false, fErr
	}
	m, vErr := validateMatches(matches, modelStr, "")
	if vErr != nil {
		return "", "", false, vErr
	}
	return m.provider, m.modelID, m.known, nil
}

// Validate and return a single match. label names the slot ("smart",
// "fast") or is empty.
func validateMatches(matches []modelMatch, modelID, label string) (modelMatch, error) {
	if label != "" {
		label += " "
	}
	switch {
	case len(matches) == 0:
		return modelMatch{}, fmt.Errorf("%smodel %q not found", label, modelID)
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.provider
		}
		return modelMatch{}, fmt.Errorf(
			"%smodel %q found in multiple providers: %s. Please specify provider using 'provider/model' format",
			label,
			modelID,
			xstrings.EnglishJoin(names, true),
		)
	}
	return matches[0], nil
}

// ConfigNotificationsPublished counts the model-change pings that
// publishModelsChanged actually accepted into a subscriber's buffer-1
// channel (a coalesced no-op send is NOT counted — nothing is pending).
// Test-observability hook only: counts accepted buffer-1 sends; it does not
// guarantee that a consumer received or processed the notification.
var ConfigNotificationsPublished atomic.Int64

// modelsSyncBeforeJoinSeam is a test-only hook called immediately before
// shutdown joins subscription workers. nil in production.
var modelsSyncBeforeJoinSeam func()

// modelsRefreshTimeout bounds the background shared-agent rebuild that
// follows a poll-detected model change, so a stuck coordinator cannot pin
// the App's shutdown on stopConfigDiskSync's WaitGroup join.
const modelsRefreshTimeout = 30 * time.Second

// startConfigDiskSync starts the config disk-sync poller under the
// App-owned lifecycle (plan §3.4). Started by New right after InitCoderAgent,
// so it covers rush (web), rush run, and SDK application mode; releaseResources
// stops and joins it before the parallel cleanupFuncs batch.
func (app *App) startConfigDiskSync() {
	if app.config == nil {
		return
	}
	app.config.StartConfigDiskSync(config.ConfigDiskSyncOptions{
		Reload:          app.reloadConfigForSync,
		OnModelsChanged: app.onModelsChangedFromPoll,
	})
}

// reloadConfigForSync is the poller's reload hook: the MCP-aware boundary.
// A nil mcpOwner receiver is safe (ReloadAndReconcileMCPConfig falls back to
// the package-level reload) and routes to the plain reload.
func (app *App) reloadConfigForSync(ctx context.Context) error {
	return app.mcpOwner.ReloadAndReconcileMCPConfig(ctx, app.config)
}

// onModelsChangedFromPoll runs on the poller goroutine, so anything heavier
// than a publish must go async (plan §3.6). The shared default agent is
// refreshed ONLY on a poll-detected model change, never on every reload
// (plan §3.7); the rebuild goroutine is joined by stopConfigDiskSync.
func (app *App) onModelsChangedFromPoll(config.ModelsChange) {
	app.publishModelsChanged()
	if app.AgentCoordinator == nil {
		return
	}
	app.modelsSyncSubsMu.Lock()
	if app.modelsSyncClosed {
		app.modelsSyncSubsMu.Unlock()
		return
	}
	app.modelsSyncWG.Add(1)
	app.modelsSyncSubsMu.Unlock()
	go func() {
		defer app.modelsSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), modelsRefreshTimeout)
		defer cancel()
		if err := app.AgentCoordinator.UpdateModels(ctx); err != nil {
			slog.Warn("Failed to refresh shared agent models after a config change", "error", err)
		}
	}()
}

// SubscribeModelsChanged returns a buffered, coalescing notification channel.
// The channel closes on caller cancellation or App shutdown; shutdown joins
// the subscription workers.
func (app *App) SubscribeModelsChanged(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	app.modelsSyncSubsMu.Lock()
	if app.modelsSyncClosed {
		close(ch)
		app.modelsSyncSubsMu.Unlock()
		return ch
	}
	if app.modelsSyncStop == nil {
		app.modelsSyncStop = make(chan struct{})
	}
	app.modelsSyncSubs = append(app.modelsSyncSubs, ch)
	app.modelsSyncSubsWG.Add(1)
	app.modelsSyncSubsMu.Unlock()
	go func() {
		defer app.modelsSyncSubsWG.Done()
		select {
		case <-ctx.Done():
		case <-app.modelsSyncStop:
		}
		app.modelsSyncSubsMu.Lock()
		for i, sub := range app.modelsSyncSubs {
			if sub == ch {
				app.modelsSyncSubs = append(app.modelsSyncSubs[:i], app.modelsSyncSubs[i+1:]...)
				close(ch)
				break
			}
		}
		app.modelsSyncSubsMu.Unlock()
	}()
	return ch
}

// publishModelsChanged pings every subscriber once; the non-blocking send
// keeps a stalled consumer from blocking the poller goroutine. Sends and
// the unsubscribe close are serialized by the same mutex, so a concurrent
// close can never race a send.
func (app *App) publishModelsChanged() {
	app.modelsSyncSubsMu.Lock()
	defer app.modelsSyncSubsMu.Unlock()
	for _, ch := range app.modelsSyncSubs {
		select {
		case ch <- struct{}{}:
			ConfigNotificationsPublished.Add(1)
		default:
		}
	}
}

// stopConfigDiskSync stops and joins the poller, subscribers, and model
// refresh workers. Idempotent; safe on bare App literals.
func (app *App) stopConfigDiskSync() {
	if app.config != nil {
		app.config.StopConfigDiskSync()
	}
	app.modelsSyncSubsMu.Lock()
	if !app.modelsSyncClosed {
		app.modelsSyncClosed = true
		if app.modelsSyncStop != nil {
			close(app.modelsSyncStop)
		}
		for _, ch := range app.modelsSyncSubs {
			close(ch)
		}
		app.modelsSyncSubs = nil
	}
	app.modelsSyncSubsMu.Unlock()
	if f := modelsSyncBeforeJoinSeam; f != nil {
		f()
	}
	app.modelsSyncSubsWG.Wait()
	app.modelsSyncWG.Wait()
}

// The session activity reader (plan R-ACT): collects the ActivityFacts the
// pure classifier (internal/session's ClassifySessionActivity) reduces to a
// verdict, for one session and -- for list views -- for many in one batched
// pass: one query per fact kind (driver markers, reaction debt, live work,
// wake schedules, ended reasons), never N, every predicate living in ONE
// sqlc query (architect decision 13: no second SQL formulation). This is
// the single place readers (internal/cmd, internal/server) will go through
// in step 2 (internal/cmd now does); the per-command layers it replaced are
// deleted.
//
// The single-session reader is the batch reader with one id, so the two can
// never disagree. A failed read of a LIVE-WORK fact is recorded in
// Facts.Unreadable and keeps the session open (the fail-open convention);
// a failed read of an END fact is recorded in Facts.EndUnreadable and never
// confers liveness (decision 7).
//
// Step 3 note (decision 12): the web flags are derived from the FACTS, not
// from the verdict -- Facts.Driver != nil || Facts.OwnRunningJobs > 0 -->
// HasLiveOwnWork, Facts.LiveDelegations > 0 --> HasLiveDescendantWork (the
// server annotator, internal/server's annotateSessionActivity). SessionActivity
// carries the Facts for exactly that; do not switch the flags over to Kind.

// SessionActivity pairs one session's facts with their verdict.
type SessionActivity struct {
	Facts   sessionfacts.ActivityFacts
	Verdict sessionfacts.ActivityVerdict
}

// SessionActivityBatchResult is the batch reader's answer: the per-id
// verdicts plus the lock-file-stem reverse map (decision 11): several real
// ids sanitise into one lock-file name (R7C-2/R8C-8), and locks/reap/prune
// need stem -> real ids to never rebuild their own mapping.
type SessionActivityBatchResult struct {
	ByID map[string]SessionActivity
	// LockStems maps sessionfacts.SessionLockStem(id) to the requested ids that
	// sanitise to it.
	LockStems map[string][]string
}

// SessionActivity classifies one session.
func (app *App) SessionActivity(ctx context.Context, sessionID string) (SessionActivity, error) {
	out, err := app.SessionActivityBatch(ctx, []string{sessionID})
	if err != nil {
		return SessionActivity{}, err
	}
	return out.ByID[sessionID], nil
}

// SessionActivityBatch classifies every requested id in one batched pass.
// Every requested id has an entry; ids unknown to the session store still
// classify (from lock, drivers and live work alone).
func (app *App) SessionActivityBatch(ctx context.Context, ids []string) (SessionActivityBatchResult, error) {
	out := SessionActivityBatchResult{
		ByID:      make(map[string]SessionActivity, len(ids)),
		LockStems: make(map[string][]string, len(ids)),
	}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		stem := sessionfacts.SessionLockStem(id)
		out.LockStems[stem] = append(out.LockStems[stem], id)
	}

	// An App without a database handle (a configless/read-only test App) has
	// no durable facts at all: that is "no facts", not "unreadable facts" --
	// the same answer the removed per-command live-work reader gave a nil
	// store. A real query error stays fail-open (decision 8).
	//
	// The debt, live-work facts come ONLY from batch node state
	// (app_batch_cutover_facts.go); host liveness is the shared lock probe.
	hostLiveness := app.batchCutoverHostLiveness()
	liveDrivers := map[string]sessionhosts.SessionDriver{}
	deadDrivers := map[string]bool{}
	driversFailed := false
	if app.activityConn() != nil {
		var driversErr error
		liveDrivers, deadDrivers, driversErr = app.sessionDriverFacts(ctx, ids, hostLiveness)
		driversFailed = driversErr != nil
	}

	// Fact kind 2: reaction debt, one statement (same predicates as the
	// single-owner query, decision 13).
	debts, debtErr := app.batchCutoverDebtOwners(ctx, ids)

	// Fact kind 3: live own jobs and descendant delegations, one batched
	// walk (one query per BFS level, one probe per distinct host).
	work := app.batchCutoverLiveWork(ctx, ids)

	// Fact kind 4: RUNNING rows on provably DEAD hosts: the batch authority
	// has no such fact -- host death never settles batch nodes -- so the
	// count stays zero.
	deadRows := map[string]int{}

	// Fact kind 5: open once wake schedules, one statement.
	schedules, wakeErr := map[string][]sessionwake.OpenWakeSchedule{}, error(nil)
	if app.WakeScheduleStore() != nil {
		schedules, wakeErr = sessionwake.OpenOnceWakeSchedulesForOwners(ctx, app.WakeScheduleStore(), ids)
	}

	// Fact kind 6: ended reasons, one statement over the requested ids --
	// top-level AND child sessions, which the session list does not carry
	// (decision 7). A missing row is not an error and not an end fact. No
	// database handle at all: no end facts, no unreadable flag (the
	// configless-App rule above).
	var ends map[string]string
	var endErr error
	if app.activityConn() != nil {
		ends, endErr = app.sessionEndReasons(ctx, ids)
	}

	for _, id := range ids {
		f := sessionfacts.ActivityFacts{SessionID: id}
		if d, ok := liveDrivers[id]; ok {
			f.Driver = &d
		}
		f.DeadHostDriver = deadDrivers[id]
		switch {
		case driversFailed:
			f.Unreadable = append(f.Unreadable, "the session's driver markers")
		case debtErr != nil:
			f.Unreadable = append(f.Unreadable, "the session's reaction debt")
		default:
			f.DriverOwes = debts[id]
		}
		if wf, ok := work[id]; ok {
			f.OwnRunningJobs = wf.Own
			f.LiveDelegations = wf.Descendants
			f.LiveDescendantSessionIDs = wf.DescendantSessionIDs
			if wf.OwnIncomplete {
				f.Unreadable = append(f.Unreadable, "the session's own jobs")
			}
			if wf.DescendantsIncomplete {
				f.Unreadable = append(f.Unreadable, "the session's delegations")
			}
		}
		f.DeadHostRunningJobs = deadRows[id]
		if wakeErr != nil {
			f.Unreadable = append(f.Unreadable, "the session's wake schedules")
		} else {
			f.OpenSchedules = schedules[id]
		}
		switch {
		case endErr != nil:
			f.EndUnreadable = true
		default:
			f.EndedReason = ends[id]
		}
		f.Lock = sessionfacts.InspectSessionLockFact(app.dataDir, id)

		v := sessionfacts.ClassifySessionActivity(f)
		out.ByID[id] = SessionActivity{Facts: f, Verdict: v}
	}
	return out, nil
}

// sessionDriverFacts splits the marker rows into live drivers (keyed by
// session id) and dead-host markers, deciding each distinct
// host's liveness once. The liveness predicate is injected: the shared
// host probe.
func (app *App) sessionDriverFacts(ctx context.Context, ids []string, hostLiveness func(string) sessionhosts.HostLockStatus) (live map[string]sessionhosts.SessionDriver, dead map[string]bool, err error) {
	live = map[string]sessionhosts.SessionDriver{}
	dead = map[string]bool{}
	conn := app.activityConn()
	if conn == nil {
		return live, dead, fmt.Errorf("session activity: no database handle")
	}
	rows, err := conn.ListSessionDrivers(ctx)
	if err != nil {
		return live, dead, fmt.Errorf("session activity: driver markers: %w", err)
	}
	hostStatus := make(map[string]sessionhosts.HostLockStatus)
	statusOf := func(hostID string) sessionhosts.HostLockStatus {
		if st, ok := hostStatus[hostID]; ok {
			return st
		}
		st := session.HostStatusUnknown
		if hostLiveness != nil {
			st = hostLiveness(hostID)
		}
		hostStatus[hostID] = st
		return st
	}
	for _, row := range rows {
		if statusOf(row.HostID) == session.HostStatusDead {
			dead[row.SessionID] = true
			continue
		}
		live[row.SessionID] = sessionhosts.SessionDriver{
			SessionID: row.SessionID, HostID: row.HostID,
			PID: int64(row.Pid), Status: statusOf(row.HostID),
		}
	}
	return live, dead, nil
}

// sessionEndReasons reads ended_reason for every requested id in one
// statement. A deleted session has no row: no fact, no error (decision 7).
func (app *App) sessionEndReasons(ctx context.Context, ids []string) (map[string]string, error) {
	conn := app.activityConn()
	if conn == nil {
		return nil, fmt.Errorf("session activity: no database handle")
	}
	rows, err := conn.ListSessionEndReasonsForIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("session activity: end reasons: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.EndedReason
	}
	return out, nil
}

// activityConn is the read pool the batched fact queries run on, wrapped in
// the generated Queries (no hand-written SQL: decision 13).
func (app *App) activityConn() *db.Queries {
	conn := app.readDB
	if conn == nil && app.DB != nil {
		conn = app.DB()
	}
	if conn == nil {
		return nil
	}
	return db.New(conn)
}

// LiveSessionDrivers is the cross-process reader of the durable driver marker
// (session_drivers, ASYNC-02): which sessions a live `rush run` loop drives
// right now, keyed by session id. A loop between turns (a paced Drain retry,
// debt pending) holds no session lock and has no running row, so this marker
// is the only durable fact that its scope is open; the status surfaces
// (`sessions why`, `sessions list`, the web session list) read it so such a
// session is not reported at rest.
//
// Liveness is the host's (this process's own host and sibling Apps' hosts are
// alive; anything else goes through the shared probe; unknown counts as alive,
// only a provably dead host's marker is dropped) and is decided once per
// distinct host. The whole table is read in ONE statement on the read pool
// (never per session, never on the writer connection): it holds one row per
// running or crashed-uncleaned loop, and the maintenance sweep purges dead
// hosts' rows. An App without a database
// handle answers an empty map. Liveness is the shared lock probe.
//
// The statement is a plain read here rather than a store method because the
// session store exposes only the per-session ForeignLiveDriver, which also
// hides this process's own host's marker.
func (app *App) LiveSessionDrivers(ctx context.Context) (map[string]sessionhosts.SessionDriver, error) {
	liveness := app.batchCutoverHostLiveness()
	conn := app.readDB
	if conn == nil && app.DB != nil {
		conn = app.DB()
	}
	if conn == nil {
		return nil, nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT session_id, host_id, pid FROM session_drivers`)
	if err != nil {
		return nil, fmt.Errorf("session drivers: read: %w", err)
	}
	defer rows.Close()
	status := make(map[string]sessionhosts.HostLockStatus)
	live := make(map[string]sessionhosts.SessionDriver)
	for rows.Next() {
		var d sessionhosts.SessionDriver
		if err := rows.Scan(&d.SessionID, &d.HostID, &d.PID); err != nil {
			return nil, fmt.Errorf("session drivers: scan: %w", err)
		}
		st, ok := status[d.HostID]
		if !ok {
			st = liveness(d.HostID)
			status[d.HostID] = st
		}
		if st == session.HostStatusDead {
			continue
		}
		d.Status = st
		live[d.SessionID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session drivers: read: %w", err)
	}
	return live, nil
}
