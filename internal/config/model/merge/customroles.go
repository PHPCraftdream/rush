// Custom-roles overlay stripping and role-write validation, moved
// verbatim from the root custom_roles_load.go in D2-1 minus
// decodeCustomRoles/customRolesDocumentField, which were exported away
// into cfgmodel as DecodeCustomRoles/CustomRolesDocumentField (the
// lane's declared crossing names).
package cfgmerge

import (
	"encoding/json"
	"strings"

	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// StripCustomRoleOverlays validates each source before the generic deep merge.
// The returned role map contains whole definitions, never merged tool arrays.
func StripCustomRoleOverlays(configs [][]byte) ([][]byte, map[string]cfgmodel.CustomRole, error) {
	stripped := make([][]byte, 0, len(configs))
	resolved := make(map[string]cfgmodel.CustomRole)
	for _, raw := range configs {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, nil, err
		}
		roleData, present, err := cfgmodel.CustomRolesDocumentField(root)
		if err != nil {
			return nil, nil, err
		}
		if !present {
			stripped = append(stripped, raw)
			continue
		}
		roles, err := cfgmodel.DecodeCustomRoles(roleData)
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

func ValidateCustomRoleWrite(raw []byte, keys []string) error {
	for _, key := range keys {
		if strings.EqualFold(settingsKeyFirstComponent(key), "roles") {
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
	roles, present, err := cfgmodel.CustomRolesDocumentField(root)
	if err != nil {
		return err
	}
	if present {
		_, err := cfgmodel.DecodeCustomRoles(roles)
		return err
	}
	return nil
}

// settingsKeyFirstComponent is a byte-identical local copy of the
// cfgmodel unexported helper (D2-1 boundary closure): cfgmerge needs
// it for role-key validation, and the function is a small pure string
// helper.
// settingsKeyFirstComponent extracts the first sjson path component.
func settingsKeyFirstComponent(key string) string {
	first := strings.TrimPrefix(key, ":")
	var component strings.Builder
	for i := 0; i < len(first); i++ {
		switch first[i] {
		case '.':
			return component.String()
		case '\\':
			i++
			if i < len(first) {
				component.WriteByte(first[i])
			}
		default:
			component.WriteByte(first[i])
		}
	}
	return component.String()
}
