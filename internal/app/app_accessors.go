// Small read-only accessors on *App: config/config-store exposure,
// the event-broker subscription helpers, and the agent-notification
// broker. Kept out of app.go so the type and constructor stand alone
// there.

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
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	xstrings "github.com/charmbracelet/x/exp/strings"
)

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
