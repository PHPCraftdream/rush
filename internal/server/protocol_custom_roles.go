package server

import "encoding/json"

const EventCustomRoles = "custom_roles"

// CustomRoleWire exposes only role selection and tool restrictions, not provider options.
type CustomRoleWire struct {
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Think           bool      `json:"think,omitempty"`
	Tools           *[]string `json:"tools,omitempty"`
}

// CustomRolesWire carries scoped definitions and the effective merged registry.
type CustomRolesWire struct {
	Global       map[string]CustomRoleWire `json:"global"`
	Workspace    map[string]CustomRoleWire `json:"workspace"`
	Effective    map[string]CustomRoleWire `json:"effective"`
	HasWorkspace bool                      `json:"hasWorkspace"`
}

// SetCustomRolePayload distinguishes omitted tools from an explicit empty array.
type SetCustomRolePayload struct {
	Scope           string          `json:"scope"`
	Name            string          `json:"name"`
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Think           bool            `json:"think,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	InheritTools    bool            `json:"inherit_tools,omitempty"`
}

// RemoveCustomRolePayload removes only the chosen scope's definition.
type RemoveCustomRolePayload struct {
	Scope string `json:"scope"`
	Name  string `json:"name"`
}
