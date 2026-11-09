package config

import (
	"encoding/json"
	"fmt"
)

func decodeCustomRoles(raw json.RawMessage) (map[string]CustomRole, error) {
	var roles map[string]CustomRole
	if err := json.Unmarshal(raw, &roles); err != nil {
		return nil, fmt.Errorf("invalid roles: %w", err)
	}
	if roles == nil {
		return nil, fmt.Errorf("roles must be an object")
	}
	for name := range roles {
		if err := ValidateCustomRoleName(name); err != nil {
			return nil, err
		}
	}
	return roles, nil
}

// stripCustomRoleOverlays validates each source before the generic deep merge.
// The returned role map contains whole definitions, never merged tool arrays.
func stripCustomRoleOverlays(configs [][]byte) ([][]byte, map[string]CustomRole, error) {
	stripped := make([][]byte, 0, len(configs))
	resolved := make(map[string]CustomRole)
	for _, raw := range configs {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, nil, err
		}
		roleData, present := root["roles"]
		if !present {
			stripped = append(stripped, raw)
			continue
		}
		roles, err := decodeCustomRoles(roleData)
		if err != nil {
			return nil, nil, err
		}
		for name, role := range roles {
			resolved[name] = role
		}
		delete(root, "roles")
		encoded, err := json.Marshal(root)
		if err != nil {
			return nil, nil, err
		}
		stripped = append(stripped, encoded)
	}
	return stripped, resolved, nil
}

func validateCustomRoleWrite(raw []byte, keys []string) error {
	for _, key := range keys {
		if settingsKeyFirstComponent(key) == "roles" {
			return validateCustomRolesDocument(raw)
		}
	}
	return nil
}

func validateCustomRolesDocument(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if roles, present := root["roles"]; present {
		_, err := decodeCustomRoles(roles)
		return err
	}
	return nil
}
