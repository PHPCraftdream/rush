package server

// Model-selection handlers: per-session model overrides, recent-model
// tracking, and the scoped (global/workspace) model-defaults API with
// its wire helpers.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
)

func handleSetSessionModels(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSessionModelsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSetSessionModels", "sessionID", p.SessionID, "smart", p.SmartModel, "fast", p.FastModel)

	// p.SmartModel/p.FastModel being nil means "the caller didn't touch this
	// slot" (task #461) — pass that straight through to UpdateModels as a nil
	// *ModelSlotUpdate so the OTHER, untouched slot's session override is
	// left exactly as it was rather than being silently pinned or cleared.
	var lp, lm, lre, sp, sm, sre string
	var smartUpdate, fastUpdate *session.ModelSlotUpdate
	if p.SmartModel != nil {
		lp, lm = p.SmartModel.Provider, p.SmartModel.Model
		lre = p.SmartModel.ReasoningEffort
		smartUpdate = &session.ModelSlotUpdate{Provider: lp, Model: lm}
	}
	if p.FastModel != nil {
		sp, sm = p.FastModel.Provider, p.FastModel.Model
		sre = p.FastModel.ReasoningEffort
		fastUpdate = &session.ModelSlotUpdate{Provider: sp, Model: sm}
	}

	if err := a.Sessions.UpdateModels(ctx, p.SessionID, smartUpdate, fastUpdate); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}

	// Worker/reviewer (task #466) — same nil-means-untouched partial update,
	// via UpdateWorkerReviewerModels/UpdateWorkerReviewerReasoningEffort
	// (UpdateModels/UpdateReasoningEffort's siblings for these optional slots).
	var wp, wm, wre, rp, rm, rre string
	var workerUpdate, reviewerUpdate *session.ModelSlotUpdate
	if p.WorkerModel != nil {
		wp, wm = p.WorkerModel.Provider, p.WorkerModel.Model
		wre = p.WorkerModel.ReasoningEffort
		workerUpdate = &session.ModelSlotUpdate{Provider: wp, Model: wm}
	}
	if p.ReviewerModel != nil {
		rp, rm = p.ReviewerModel.Provider, p.ReviewerModel.Model
		rre = p.ReviewerModel.ReasoningEffort
		reviewerUpdate = &session.ModelSlotUpdate{Provider: rp, Model: rm}
	}
	if workerUpdate != nil || reviewerUpdate != nil {
		if err := a.Sessions.UpdateWorkerReviewerModels(ctx, p.SessionID, workerUpdate, reviewerUpdate); err != nil {
			c.reply(msg.ID, EventError, nil, err.Error())
			return
		}
	}
	if wre != "" || rre != "" {
		if wre == "" || rre == "" {
			if sess, sessErr := a.Sessions.Get(ctx, p.SessionID); sessErr == nil {
				if wre == "" {
					wre = sess.WorkerModelReasoningEffort
				}
				if rre == "" {
					rre = sess.ReviewerModelReasoningEffort
				}
			}
		}
		if err := a.Sessions.UpdateWorkerReviewerReasoningEffort(ctx, p.SessionID, wre, rre); err != nil {
			slog.Warn("ws: failed to update worker/reviewer reasoning effort", "err", err)
		}
	}

	// Update reasoning effort for models that support it. CRITICAL: a single
	// ModelSelector arrow click only carries ONE effort value but the frontend
	// always serialises BOTH models in set_session_models. Without preserving
	// the unset side from the DB, the previous "default to medium" behaviour
	// would clobber the OTHER model's effort on every click — and on GLM (where
	// medium is not a supported level) the clamp useEffect would immediately
	// fire back, locking the UI into a flash-loop that never let the
	// operator's chosen effort stick.
	if lre != "" || sre != "" {
		if lre == "" || sre == "" {
			if sess, sessErr := a.Sessions.Get(ctx, p.SessionID); sessErr == nil {
				if lre == "" {
					lre = sess.SmartModelReasoningEffort
				}
				if sre == "" {
					sre = sess.FastModelReasoningEffort
				}
			}
		}
		if err := a.Sessions.UpdateReasoningEffort(ctx, p.SessionID, lre, sre); err != nil {
			slog.Warn("ws: failed to update reasoning effort", "err", err)
		}
	}

	// Record recently used models in the config (persists across restarts)
	store := a.Store()
	if store != nil && lp != "" && lm != "" {
		if err := store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeSmart, config.SelectedModel{Provider: lp, Model: lm}); err != nil {
			slog.Warn("ws: failed to record recent smart model", "err", err)
		}
	}
	if store != nil && sp != "" && sm != "" {
		if err := store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeFast, config.SelectedModel{Provider: sp, Model: sm}); err != nil {
			slog.Warn("ws: failed to record recent fast model", "err", err)
		}
	}

	// Broadcast updated session so clients can refresh their model selectors.
	// Unlike handleCreateSession/handleForkSession (harmless per round-23
	// review F-3: a session born in this process cannot be foreign-locked at
	// creation), set_session_models targets an EXISTING session by ID that
	// another live process may legitimately hold, so the broadcast must
	// carry the ownership annotation like every other Session that reaches
	// a client — see AnnotateSessionExternalOwnership.
	if sess, err := a.Sessions.Get(ctx, p.SessionID); err == nil {
		AnnotateSessionExternalOwnership(a, &sess)
		c.hub.Broadcast(EventSessionUpdated, sess)
	}
	// Broadcast updated config so all clients see the new recent-models list immediately.
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleRemoveRecentModel(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveRecentModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	if err := store.RemoveRecentModel(config.ScopeGlobal, modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
		slog.Warn("ws: failed to remove recent model", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTrackModelUsage(a *appPkg.App, c *Client, msg WSMessage) {
	var p TrackModelUsagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		return
	}
	modelType := config.SelectedModelType(p.ModelType)
	// Recency ONLY — never the preferred/default model.
	//
	// This used to call UpdatePreferredModel(ScopeGlobal, ...), which writes
	// models.<type> into the global crush.json. That made model selection
	// leak out of the session it was made in: picking a model for one session
	// silently changed the system-wide default for every other session, every
	// other folder, and every subsequent CLI invocation. Worse, the web client
	// fires this command on every assistant message (web/src/useWS.ts), not
	// just on an explicit pick, so the global default drifted to whatever the
	// most recently active session happened to be running.
	//
	// Model defaults are scoped deliberately and cascade system -> folder ->
	// session: the system and folder levels are written only through the
	// explicit scoped commands (and `crush models use`), and the session level
	// only through set_session_models. "Recently used" is a UI convenience
	// list and is the only thing this command may touch.
	if err := store.RecordRecentModel(config.ScopeGlobal, modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
		slog.Warn("ws: failed to track model usage", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// scopedModelSlots is the fixed slot set the scoped-models API exposes, in
// display order. Matches internal/cmd/models_state.go's four slots.
var scopedModelSlots = []config.SelectedModelType{
	config.SelectedModelTypeSmart,
	config.SelectedModelTypeFast,
	config.SelectedModelTypeWorker,
	config.SelectedModelTypeReviewer,
}

func scopedModelSlotFromWire(s *config.SelectedModel) *ModelOverrideWire {
	if s == nil {
		return nil
	}
	return &ModelOverrideWire{Provider: s.Provider, Model: s.Model, ReasoningEffort: s.ReasoningEffort}
}

// scopedModelsScopeFromWire parses the "global"/"workspace" strings the web
// client sends for the scoped-models API into a config.Scope. Deliberately
// stricter than the sibling scopeFromWire (used by provider-config commands,
// which silently defaults unrecognised values to ScopeGlobal): an unknown
// scope here is surfaced as an error rather than silently landing in global,
// since writing to the wrong scope is exactly the class of bug this whole
// feature exists to prevent.
func scopedModelsScopeFromWire(s string) (config.Scope, error) {
	switch s {
	case "global":
		return config.ScopeGlobal, nil
	case "workspace":
		return config.ScopeWorkspace, nil
	default:
		return 0, fmt.Errorf("unknown scope %q", s)
	}
}

func scopedModelsScopeToWire(scope config.Scope) string {
	if scope == config.ScopeWorkspace {
		return "workspace"
	}
	return "global"
}

// buildScopedModelsWire assembles the get_scoped_models response: for each
// of the four slots, what global and workspace explicitly set (nil = unset
// there) plus the resolved effective value and which scope it came from.
// Reuses store.ReadAllModelsAtScope and store.Config().Models — the exact
// same primitives `crush models state` (internal/cmd/models_state.go) reads
// — so the CLI and the web UI can never disagree about what's effective.
func buildScopedModelsWire(a *appPkg.App) (ScopedModelsWire, error) {
	store := a.Store()
	globalAll, err := store.ReadAllModelsAtScope(config.ScopeGlobal)
	if err != nil {
		return ScopedModelsWire{}, fmt.Errorf("read global scope: %w", err)
	}
	workspaceAll, err := store.ReadAllModelsAtScope(config.ScopeWorkspace)
	if err != nil {
		return ScopedModelsWire{}, fmt.Errorf("read workspace scope: %w", err)
	}
	effectiveAll := store.Config().Models

	build := func(slot config.SelectedModelType) ScopedModelSlotWire {
		g := globalAll[slot]
		w := workspaceAll[slot]
		out := ScopedModelSlotWire{
			Global:    scopedModelSlotFromWire(g),
			Workspace: scopedModelSlotFromWire(w),
		}
		if eff, ok := effectiveAll[slot]; ok {
			effCopy := eff
			out.Effective = scopedModelSlotFromWire(&effCopy)
			switch {
			case w != nil:
				out.EffectiveScope = scopedModelsScopeToWire(config.ScopeWorkspace)
			case g != nil:
				out.EffectiveScope = scopedModelsScopeToWire(config.ScopeGlobal)
			}
		}
		return out
	}

	return ScopedModelsWire{
		Smart:        build(config.SelectedModelTypeSmart),
		Fast:         build(config.SelectedModelTypeFast),
		Worker:       build(config.SelectedModelTypeWorker),
		Reviewer:     build(config.SelectedModelTypeReviewer),
		HasWorkspace: store.HasWorkspaceConfig(),
	}, nil
}

func handleGetScopedModels(a *appPkg.App, c *Client, msg WSMessage) {
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	wire, err := buildScopedModelsWire(a)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventScopedModels, wire, "")
}

// handleSetScopedModel writes one slot at one scope (config.ScopeGlobal =
// "system" or config.ScopeWorkspace = "folder"). Unlike set_session_models,
// this never touches the session DB — it's the system/folder half of the
// cascade, edited the same way `crush models use --global/--local` does.
func handleSetScopedModel(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetScopedModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	slot := config.SelectedModelType(p.Slot)
	if !slices.Contains(scopedModelSlots, slot) {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("unknown slot %q", p.Slot))
		return
	}
	if p.Provider == "" || p.Model == "" {
		c.reply(msg.ID, EventError, nil, "provider and model are required")
		return
	}
	if err := store.UpdatePreferredModel(scope, slot, config.SelectedModel{
		Provider:        p.Provider,
		Model:           p.Model,
		ReasoningEffort: p.ReasoningEffort,
	}); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	if wire, err := buildScopedModelsWire(a); err == nil {
		c.hub.Broadcast(EventScopedModels, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleClearScopedModel removes one slot's explicit value at one scope
// (mirrors `crush models unset <slot> --global/--local`, internal/cmd/
// models_unset.go), letting the other scope (or "no default at all") take
// over. A missing key is treated as a no-op success, matching the CLI.
func handleClearScopedModel(a *appPkg.App, c *Client, msg WSMessage) {
	var p ClearScopedModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	slot := config.SelectedModelType(p.Slot)
	if !slices.Contains(scopedModelSlots, slot) {
		c.reply(msg.ID, EventError, nil, fmt.Sprintf("unknown slot %q", p.Slot))
		return
	}
	if err := store.RemoveConfigField(scope, "models."+string(slot)); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Best-effort cleanup: if that was the last slot in this scope's
	// "models" object, strip the now-empty object so the file stays clean —
	// same behavior as `crush models unset` (internal/cmd/models_unset.go).
	if remaining, rerr := store.ReadAllModelsAtScope(scope); rerr == nil && len(remaining) == 0 {
		_ = store.RemoveConfigField(scope, "models")
	}
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	if wire, err := buildScopedModelsWire(a); err == nil {
		c.hub.Broadcast(EventScopedModels, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
