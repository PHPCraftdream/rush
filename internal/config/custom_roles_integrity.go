package config

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// Existing model-only documents keep their exact v1 signature payload.
// Role-aware payloads also bind tools presence, preventing unsigned widening.
func canonicalModelsAndRolesJSON(data []byte) (string, bool, error) {
	models, modelsPresent, err := canonicalModelsJSON(gjson.GetBytes(data, "models").Raw)
	if err != nil {
		return "", false, err
	}
	rawRoles := gjson.GetBytes(data, "roles")
	if !rawRoles.Exists() {
		return models, modelsPresent, nil
	}
	roles, err := decodeCustomRoles(json.RawMessage(rawRoles.Raw))
	if err != nil {
		return "", false, err
	}
	if len(roles) == 0 {
		return models, modelsPresent, nil
	}
	canonicalRoles, _, err := canonicalModelsJSON(rawRoles.Raw)
	if err != nil {
		return "", false, err
	}
	payload := map[string]json.RawMessage{"roles": json.RawMessage(canonicalRoles)}
	if modelsPresent {
		payload["models"] = json.RawMessage(models)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	return "roles-v1|" + string(encoded), true, nil
}
