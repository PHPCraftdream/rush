package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

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

func buildCustomRolesWire(a *appPkg.App) (*CustomRolesWire, error) {
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
	return &CustomRolesWire{
		Global: customRolesToWire(global), Workspace: customRolesToWire(workspace),
		Effective: customRolesToWire(effective), HasWorkspace: store.HasWorkspaceConfig(),
	}, nil
}

func customRolesToWire(roles map[string]config.CustomRole) map[string]CustomRoleWire {
	wire := make(map[string]CustomRoleWire, len(roles))
	for name, role := range roles {
		var tools *[]string
		if role.Tools != nil {
			patterns := append([]string{}, (*role.Tools)...)
			tools = &patterns
		}
		wire[name] = CustomRoleWire{Provider: role.Provider, Model: role.Model, ReasoningEffort: role.ReasoningEffort, Think: role.Think, Tools: tools}
	}
	return wire
}

func handleGetCustomRoles(a *appPkg.App, c *Client, msg WSMessage) {
	wire, err := buildCustomRolesWire(a)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventCustomRoles, wire, "")
}

func customRoleUpdateFromPayload(p SetCustomRolePayload) (config.CustomRoleUpdate, error) {
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

func handleSetCustomRole(a *appPkg.App, c *Client, msg WSMessage) {
	var p SetCustomRolePayload
	if err := decodeCustomRolePayload(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid role payload")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	update, err := customRoleUpdateFromPayload(p)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	cfg, _ := store.Snapshot()
	if err := config.ValidateCustomRoleSelection(cfg, config.CustomRole{SelectedModel: update.Model, Tools: update.Tools}); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if err := store.SetCustomRoleWithAuthority(scope, p.Name, update, c.settingsWriteAuthority()); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	publishCustomRoleChange(a, c, msg)
}

func handleRemoveCustomRole(a *appPkg.App, c *Client, msg WSMessage) {
	var p RemoveCustomRolePayload
	if err := decodeCustomRolePayload(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid role payload")
		return
	}
	scope, err := scopedModelsScopeFromWire(p.Scope)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if err := store.RemoveCustomRoleWithAuthority(scope, p.Name, c.settingsWriteAuthority()); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	publishCustomRoleChange(a, c, msg)
}

func publishCustomRoleChange(a *appPkg.App, c *Client, msg WSMessage) {
	if wire, ok := buildConfigWire(a); ok {
		c.hub.Broadcast(EventConfig, wire)
	}
	if wire, err := buildCustomRolesWire(a); err == nil {
		c.hub.Broadcast(EventCustomRoles, wire)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
