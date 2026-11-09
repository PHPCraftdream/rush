package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// configFieldRemoval is internal to typed atomic set-and-remove operations.
type configFieldRemoval struct{}

// CustomRoleUpdate changes a model without implicitly resetting its tool ceiling.
type CustomRoleUpdate struct {
	Model        SelectedModel
	Tools        *[]string
	InheritTools bool
}

func customRoleConfigKey(name string) string {
	// The colon forces an object key even for a numeric role name.
	return "roles.:" + strings.ReplaceAll(name, ".", `\.`)
}

func (s *ConfigStore) SetCustomRole(scope Scope, name string, update CustomRoleUpdate) error {
	return s.SetCustomRoleWithAuthority(scope, name, update, SettingsAuthorityGuarded)
}

func (s *ConfigStore) SetCustomRoleWithAuthority(scope Scope, name string, update CustomRoleUpdate, authority SettingsWriteAuthority) error {
	if err := ValidateCustomRoleName(name); err != nil {
		return err
	}
	if update.Tools != nil && update.InheritTools {
		return fmt.Errorf("tools and inherit-tools are mutually exclusive")
	}
	if err := (CustomRole{SelectedModel: update.Model, Tools: update.Tools}).Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(update.Model)
	if err != nil {
		return err
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &profile); err != nil {
		return err
	}
	prefix := customRoleConfigKey(name)
	fields := make(map[string]any, len(profile)+3)
	for field, value := range profile {
		fields[prefix+"."+field] = value
	}
	fields[prefix+".reasoning_effort"] = update.Model.ReasoningEffort
	fields[prefix+".think"] = update.Model.Think
	if update.Tools != nil {
		patterns := append([]string{}, (*update.Tools)...)
		fields[prefix+".tools"] = patterns
	} else if update.InheritTools {
		fields[prefix+".tools"] = configFieldRemoval{}
	}
	return s.SetConfigFieldsWithAuthority(scope, fields, authority)
}

func (s *ConfigStore) RemoveCustomRole(scope Scope, name string) error {
	return s.RemoveCustomRoleWithAuthority(scope, name, SettingsAuthorityGuarded)
}

func (s *ConfigStore) RemoveCustomRoleWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) error {
	if err := ValidateCustomRoleName(name); err != nil {
		return err
	}
	return s.RemoveConfigFieldWithAuthority(scope, customRoleConfigKey(name), authority)
}

// CustomRolesForScope reads only the selected document, not the effective merge.
func (s *ConfigStore) CustomRolesForScope(scope Scope) (map[string]CustomRole, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	doc, err := readSettingsDocument(path)
	if os.IsNotExist(err) {
		return map[string]CustomRole{}, nil
	}
	if err != nil {
		return nil, err
	}
	_, roles, err := stripCustomRoleOverlays([][]byte{doc.jsonBytes})
	return roles, err
}
