package wsconfig

// Model-selection handlers: per-session model overrides, recent-model
// tracking, and the scoped (global/workspace) model-defaults API with
// its wire helpers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	hub "github.com/PHPCraftdream/rush/internal/serve/server/hub"
	wirePkg "github.com/PHPCraftdream/rush/internal/serve/server/wire"
	wsapi "github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	"github.com/PHPCraftdream/rush/internal/session"
)

// Field-group names reported in SetSessionModelsResult (task #696): one
// entry per independent DB update handleSetSessionModels performs, named
// after the session.Service method behind it. A group the payload did not
// ask to change appears in neither Applied nor Failed.
const (
	setModelsFieldSmartFast            = "smartFastModels"
	setModelsFieldWorkerReviewer       = "workerReviewerModels"
	setModelsFieldSmartFastEffort      = "smartFastReasoningEffort"
	setModelsFieldWorkerReviewerEffort = "workerReviewerReasoningEffort"
)

func HandleSetSessionModels(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleSetSessionModels(ctx, a, c, msg)
}

func handleSetSessionModels(ctx context.Context, a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetSessionModelsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
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

	// The four updates below are independent column-scoped writes to the
	// same row, so each is attempted and reported per field (task #696,
	// review F8): a failure used to be either an early EventError that hid
	// which earlier fields HAD already landed, or — for the two effort
	// updates — only a slog.Warn folded into an unqualified "ok" reply. The
	// reply now carries the true per-field outcome, following
	// DeleteOtherSessionsResult's deleted/failed shape (task #684). A
	// single transaction was ruled out: it would need a new session.Service
	// method (the server package has no DB handle), and partial application
	// is operationally meaningful here — every group is independently
	// retryable and the session_updated broadcast below already reflects
	// whatever subset landed.
	result := wirePkg.SetSessionModelsResult{
		Applied: []string{},
		Failed:  []string{},
		Errors:  map[string]string{},
	}
	run := func(field string, update func() error) {
		if err := update(); err != nil {
			result.Failed = append(result.Failed, field)
			result.Errors[field] = err.Error()
			slog.Warn("ws: failed to update session models field", "field", field, "err", err)
			return
		}
		result.Applied = append(result.Applied, field)
	}

	if smartUpdate != nil || fastUpdate != nil {
		run(setModelsFieldSmartFast, func() error {
			return a.Sessions.UpdateModels(ctx, p.SessionID, smartUpdate, fastUpdate)
		})
	}
	if workerUpdate != nil || reviewerUpdate != nil {
		run(setModelsFieldWorkerReviewer, func() error {
			return a.Sessions.UpdateWorkerReviewerModels(ctx, p.SessionID, workerUpdate, reviewerUpdate)
		})
	}

	// Effort values are written independently per sent model slot. This avoids
	// stale Get/backfill/pair writes racing another request and preserves
	// omitted slots. The clear sentinel maps to empty, which service methods
	// store as SQL NULL just like the legacy pair methods.
	if (p.WorkerModel != nil && wre != "") || (p.ReviewerModel != nil && rre != "") {
		run(setModelsFieldWorkerReviewerEffort, func() error {
			var firstErr error
			if p.WorkerModel != nil && wre != "" {
				effort := wre
				if effort == wirePkg.ReasoningEffortClear {
					effort = ""
				}
				firstErr = a.Sessions.UpdateWorkerReasoningEffort(ctx, p.SessionID, effort)
			}
			if p.ReviewerModel != nil && rre != "" {
				effort := rre
				if effort == wirePkg.ReasoningEffortClear {
					effort = ""
				}
				if err := a.Sessions.UpdateReviewerReasoningEffort(ctx, p.SessionID, effort); firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		})
	}

	if (p.SmartModel != nil && lre != "") || (p.FastModel != nil && sre != "") {
		run(setModelsFieldSmartFastEffort, func() error {
			var firstErr error
			if p.SmartModel != nil && lre != "" {
				effort := lre
				if effort == wirePkg.ReasoningEffortClear {
					effort = ""
				}
				firstErr = a.Sessions.UpdateSmartReasoningEffort(ctx, p.SessionID, effort)
			}
			if p.FastModel != nil && sre != "" {
				effort := sre
				if effort == wirePkg.ReasoningEffortClear {
					effort = ""
				}
				if err := a.Sessions.UpdateFastReasoningEffort(ctx, p.SessionID, effort); firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		})
	}

	// Record recently used models in the config (persists across restarts).
	// Deliberately still best-effort/warn-only rather than part of the
	// per-field result: this is a UI-convenience list in a config file, not
	// session DB state, and the EventConfig broadcast below re-syncs every
	// client to whatever the list actually contains.
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
		wsapi.AnnotateSessionExternalOwnership(a, &sess)
		c.Hub().Broadcast(wirePkg.EventSessionUpdated, sess)
	}
	// Broadcast updated config so all clients see the new recent-models list immediately.
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, result, "")
}

func handleRemoveRecentModel(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.RemoveRecentModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
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
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func handleTrackModelUsage(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.TrackModelUsagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
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
	// models.<type> into the global rush.json. That made model selection
	// leak out of the session it was made in: picking a model for one session
	// silently changed the system-wide default for every other session, every
	// other folder, and every subsequent CLI invocation. Worse, the web client
	// fires this command on every assistant message (web/src/useWS.ts), not
	// just on an explicit pick, so the global default drifted to whatever the
	// most recently active session happened to be running.
	//
	// Model defaults are scoped deliberately and cascade system -> folder ->
	// session: the system and folder levels are written only through the
	// explicit scoped commands (and `rush models use`), and the session level
	// only through set_session_models. "Recently used" is a UI convenience
	// list and is the only thing this command may touch.
	if err := store.RecordRecentModel(config.ScopeGlobal, modelType, config.SelectedModel{Provider: p.Provider, Model: p.Model}); err != nil {
		slog.Warn("ws: failed to track model usage", "err", err)
	}
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

// scopedModelSlots is the fixed slot set the scoped-models API exposes, in
// display order. Matches internal/cmd/models_state.go's four slots.
var scopedModelSlots = []config.SelectedModelType{
	config.SelectedModelTypeSmart,
	config.SelectedModelTypeFast,
	config.SelectedModelTypeWorker,
	config.SelectedModelTypeReviewer,
}

func scopedModelSlotFromWire(s *config.SelectedModel) *wirePkg.ModelOverrideWire {
	if s == nil {
		return nil
	}
	return &wirePkg.ModelOverrideWire{Provider: s.Provider, Model: s.Model, ReasoningEffort: s.ReasoningEffort}
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
// same primitives `rush models state` (internal/cmd/models_state.go) reads
// — so the CLI and the web UI can never disagree about what's effective.
func buildScopedModelsWire(a *appPkg.App) (wirePkg.ScopedModelsWire, error) {
	store := a.Store()
	globalAll, err := store.ReadAllModelsAtScope(config.ScopeGlobal)
	if err != nil {
		return wirePkg.ScopedModelsWire{}, fmt.Errorf("read global scope: %w", err)
	}
	workspaceAll, err := store.ReadAllModelsAtScope(config.ScopeWorkspace)
	if err != nil {
		return wirePkg.ScopedModelsWire{}, fmt.Errorf("read workspace scope: %w", err)
	}
	effectiveAll := store.Config().Models

	build := func(slot config.SelectedModelType) wirePkg.ScopedModelSlotWire {
		g := globalAll[slot]
		w := workspaceAll[slot]
		out := wirePkg.ScopedModelSlotWire{
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

	return wirePkg.ScopedModelsWire{
		Smart:        build(config.SelectedModelTypeSmart),
		Fast:         build(config.SelectedModelTypeFast),
		Worker:       build(config.SelectedModelTypeWorker),
		Reviewer:     build(config.SelectedModelTypeReviewer),
		HasWorkspace: store.HasWorkspaceConfig(),
	}, nil
}

func handleGetScopedModels(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	wire, err := buildScopedModelsWire(a)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventScopedModels, wire, "")
}

// handleSetScopedModel writes one slot at one scope (config.ScopeGlobal =
// "system" or config.ScopeWorkspace = "folder"). Unlike set_session_models,
// this never touches the session DB — it's the system/folder half of the
// cascade, edited the same way `rush models use --global/--local` does.
func HandleSetScopedModel(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleSetScopedModel(a, c, msg)
}

func HandleClearScopedModel(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	handleClearScopedModel(a, c, msg)
}

func handleSetScopedModel(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetScopedModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	slot := config.SelectedModelType(p.Slot)
	if !slices.Contains(scopedModelSlots, slot) {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("unknown slot %q", p.Slot))
		return
	}
	if p.Provider == "" || p.Model == "" {
		c.Reply(msg.ID, wirePkg.EventError, nil, "provider and model are required")
		return
	}
	if err := store.UpdatePreferredModelWithAuthority(scope, slot, config.SelectedModel{
		Provider:        p.Provider,
		Model:           p.Model,
		ReasoningEffort: p.ReasoningEffort,
	}, c.SettingsWriteAuthority()); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	if wire, err := buildScopedModelsWire(a); err == nil {
		c.Hub().Broadcast(wirePkg.EventScopedModels, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

// handleClearScopedModel removes one slot's explicit value at one scope
// (mirrors `rush models unset <slot> --global/--local`, internal/cmd/
// models_unset.go), letting the other scope (or "no default at all") take
// over. A missing key is treated as a no-op success, matching the CLI.
func handleClearScopedModel(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.ClearScopedModelPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid payload")
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	slot := config.SelectedModelType(p.Slot)
	if !slices.Contains(scopedModelSlots, slot) {
		c.Reply(msg.ID, wirePkg.EventError, nil, fmt.Sprintf("unknown slot %q", p.Slot))
		return
	}
	if err := store.RemoveConfigFieldWithAuthority(scope, "models."+string(slot), c.SettingsWriteAuthority()); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	// Best-effort cleanup: if that was the last slot in this scope's
	// "models" object, strip the now-empty object so the file stays clean —
	// same behavior as `rush models unset` (internal/cmd/models_unset.go).
	if remaining, rerr := store.ReadAllModelsAtScope(scope); rerr == nil && len(remaining) == 0 {
		_ = store.RemoveConfigFieldWithAuthority(scope, "models", c.SettingsWriteAuthority())
	}
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	if wire, err := buildScopedModelsWire(a); err == nil {
		c.Hub().Broadcast(wirePkg.EventScopedModels, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}

func decodeCustomRolePayload(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON payload")
	}
	return nil
}

func buildCustomRolesWire(a *appPkg.App) (*wirePkg.CustomRolesWire, error) {
	store := a.Store()
	if store == nil {
		return nil, fmt.Errorf("config not available")
	}
	global, err := store.CustomRolesForScope(config.ScopeGlobal)
	if err != nil {
		return nil, err
	}
	workspace := map[string]config.CustomRole{}
	if store.HasWorkspaceConfig() {
		workspace, err = store.CustomRolesForScope(config.ScopeWorkspace)
		if err != nil {
			return nil, err
		}
	}
	cfg, _ := store.Snapshot()
	effective := cfg.Roles
	if effective == nil {
		effective = map[string]config.CustomRole{}
	}
	return &wirePkg.CustomRolesWire{
		Global: customRolesToWire(global), Workspace: customRolesToWire(workspace),
		Effective: customRolesToWire(effective), HasWorkspace: store.HasWorkspaceConfig(),
	}, nil
}

func customRolesToWire(roles map[string]config.CustomRole) map[string]wirePkg.CustomRoleWire {
	wire := make(map[string]wirePkg.CustomRoleWire, len(roles))
	for name, role := range roles {
		var tools *[]string
		if role.Tools != nil {
			patterns := append([]string{}, (*role.Tools)...)
			tools = &patterns
		}
		wire[name] = wirePkg.CustomRoleWire{Provider: role.Provider, Model: role.Model, ReasoningEffort: role.ReasoningEffort, Think: role.Think, Tools: tools}
	}
	return wire
}

func handleGetCustomRoles(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	wire, err := buildCustomRolesWire(a)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, wirePkg.EventCustomRoles, wire, "")
}

func customRoleUpdateFromPayload(p wirePkg.SetCustomRolePayload) (config.CustomRoleUpdate, error) {
	update := config.CustomRoleUpdate{
		Model:        config.SelectedModel{Provider: p.Provider, Model: p.Model, ReasoningEffort: p.ReasoningEffort, Think: p.Think},
		InheritTools: p.InheritTools,
	}
	if len(p.Tools) != 0 {
		var patterns []string
		if err := json.Unmarshal(p.Tools, &patterns); err != nil {
			return config.CustomRoleUpdate{}, fmt.Errorf("tools must be an array: %w", err)
		}
		if patterns == nil {
			return config.CustomRoleUpdate{}, fmt.Errorf("tools must be an array, not null")
		}
		update.Tools = &patterns
	}
	if update.Tools != nil && update.InheritTools {
		return config.CustomRoleUpdate{}, fmt.Errorf("tools and inherit_tools are mutually exclusive")
	}
	if err := (config.CustomRole{SelectedModel: update.Model, Tools: update.Tools}).Validate(); err != nil {
		return config.CustomRoleUpdate{}, err
	}
	return update, nil
}

func handleSetCustomRole(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.SetCustomRolePayload
	if err := decodeCustomRolePayload(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid role payload")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	update, err := customRoleUpdateFromPayload(p)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	cfg, _ := store.Snapshot()
	if err := config.ValidateCustomRoleSelection(cfg, config.CustomRole{SelectedModel: update.Model, Tools: update.Tools}); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	if err := store.SetCustomRoleWithAuthority(scope, p.Name, update, c.SettingsWriteAuthority()); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	publishCustomRoleChange(a, c, msg)
}

func handleRemoveCustomRole(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	var p wirePkg.RemoveCustomRolePayload
	if err := decodeCustomRolePayload(msg.Payload, &p); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "invalid role payload")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	store := a.Store()
	if store == nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, "config not available")
		return
	}
	if err := store.RemoveCustomRoleWithAuthority(scope, p.Name, c.SettingsWriteAuthority()); err != nil {
		c.Reply(msg.ID, wirePkg.EventError, nil, err.Error())
		return
	}
	publishCustomRoleChange(a, c, msg)
}

func publishCustomRoleChange(a *appPkg.App, c *hub.Client, msg wirePkg.WSMessage) {
	if wire, ok := buildConfigWire(a); ok {
		c.Hub().Broadcast(wirePkg.EventConfig, wire)
	}
	if wire, err := buildCustomRolesWire(a); err == nil {
		c.Hub().Broadcast(wirePkg.EventCustomRoles, wire)
	}
	c.Reply(msg.ID, wirePkg.EventResponse, map[string]string{"status": "ok"}, "")
}
