package cfgmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxCustomRoleNameRunes    = 64
	maxCustomRolePatterns     = 128
	maxCustomRolePatternBytes = 256
)

// CustomRole adds an optional tool ceiling to an explicit model profile.
type CustomRole struct {
	SelectedModel
	// Nil is unrestricted; a non-nil empty list permits no tools.
	Tools *[]string `json:"tools,omitempty"`
}

func ValidateCustomRoleName(name string) error {
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > maxCustomRoleNameRunes || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid custom role name %q", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return fmt.Errorf("invalid custom role name %q", name)
		}
	}
	for _, reserved := range []string{"smart", "fast", "worker", "reviewer"} {
		if strings.EqualFold(name, reserved) {
			return fmt.Errorf("custom role name %q is reserved", name)
		}
	}
	return nil
}

func (r CustomRole) Validate() error {
	if strings.TrimSpace(r.Provider) == "" || strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("custom role requires provider and model")
	}
	if r.Tools == nil {
		return nil
	}
	if len(*r.Tools) > maxCustomRolePatterns {
		return fmt.Errorf("custom role allows at most %d tool patterns", maxCustomRolePatterns)
	}
	for _, pattern := range *r.Tools {
		if !utf8.ValidString(pattern) || pattern == "" || strings.TrimSpace(pattern) != pattern || len(pattern) > maxCustomRolePatternBytes {
			return fmt.Errorf("invalid custom role tool pattern %q", pattern)
		}
		for _, char := range pattern {
			if unicode.IsControl(char) {
				return fmt.Errorf("invalid custom role tool pattern %q", pattern)
			}
		}
	}
	return nil
}

func (r *CustomRole) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("custom role must be an object")
	}
	for field, value := range fields {
		if strings.EqualFold(field, "tools") && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("custom role tools must be an array, not null")
		}
	}
	type roleJSON CustomRole
	var decoded roleJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	candidate := CustomRole(decoded)
	if err := candidate.Validate(); err != nil {
		return err
	}
	*r = candidate
	return nil
}

func (r CustomRole) MarshalJSON() ([]byte, error) {
	type roleJSON CustomRole
	encoded := roleJSON(r)
	if encoded.Tools != nil && *encoded.Tools == nil {
		empty := make([]string, 0)
		encoded.Tools = &empty
	}
	return json.Marshal(encoded)
}

// MatchRoleToolPattern reports whether name matches pattern. The entire name
// must match; '*' matches any sequence of Unicode code points and '?' matches
// one Unicode code point. Every other code point is literal.
func MatchRoleToolPattern(pattern, name string) bool {
	patternIndex, nameIndex := 0, 0
	starPatternIndex, starNameIndex := -1, -1

	for nameIndex < len(name) {
		if patternIndex < len(pattern) {
			switch pattern[patternIndex] {
			case '*':
				starPatternIndex = patternIndex
				patternIndex++
				starNameIndex = nameIndex
				continue
			case '?':
				_, size := utf8.DecodeRuneInString(name[nameIndex:])
				patternIndex++
				nameIndex += size
				continue
			}

			patternRune, patternSize := utf8.DecodeRuneInString(pattern[patternIndex:])
			nameRune, nameSize := utf8.DecodeRuneInString(name[nameIndex:])
			if patternRune == nameRune {
				patternIndex += patternSize
				nameIndex += nameSize
				continue
			}
		}

		if starPatternIndex < 0 {
			return false
		}
		_, size := utf8.DecodeRuneInString(name[starNameIndex:])
		starNameIndex += size
		nameIndex = starNameIndex
		patternIndex = starPatternIndex + 1
	}

	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// RoleToolsAllow reports whether any pattern allows name. An empty pattern
// list allows no tools.
func RoleToolsAllow(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if MatchRoleToolPattern(pattern, name) {
			return true
		}
	}
	return false
}

// ValidateCustomRoleSelection checks a role against one pinned provider snapshot.
// Unknown raw model IDs retain the existing unverified passthrough behavior.
func ValidateCustomRoleSelection(cfg *Config, role CustomRole) error {
	if err := role.Validate(); err != nil {
		return err
	}
	if cfg == nil || cfg.Providers == nil {
		return fmt.Errorf("model providers are not configured")
	}
	provider, exists := cfg.Providers.Get(role.Provider)
	if !exists {
		return fmt.Errorf("provider %q is not configured", role.Provider)
	}
	if role.ReasoningEffort == "" {
		return nil
	}
	var levels []string
	if live, reported := provider.LiveEfforts[role.Model]; reported {
		levels = live.Levels
	} else {
		for _, model := range provider.Models {
			if model.ID == role.Model {
				levels = model.ReasoningLevels
				break
			}
		}
	}
	if len(levels) != 0 && !slices.Contains(levels, role.ReasoningEffort) {
		return fmt.Errorf("effort %q is not supported by %s/%s (supported: %v)", role.ReasoningEffort, role.Provider, role.Model, levels)
	}
	return nil
}

// Constant mixed into the cache revision hash; the name is deliberately generic.
const hashSalt = "7c1f4e9a2b8d6350af02c4e8b19d5736"

// CacheRevKey is the settings field containing the model-integrity signature.
const CacheRevKey = "cache_rev"

// ApplyModelsIntegrity signs built-in models and custom role definitions.
func ApplyModelsIntegrity(doc string) (string, error) {
	canonical, present, err := CanonicalModelsAndRolesJSON([]byte(doc))
	if err != nil {
		return "", fmt.Errorf("failed to canonicalize model settings: %w", err)
	}
	if present {
		return sjson.Set(doc, CacheRevKey, modelsIntegritySignature(canonical))
	}
	return sjson.Delete(doc, CacheRevKey)
}

// TouchesModelsKey includes custom roles in the model-integrity boundary.
func TouchesModelsKey(keys []string) bool {
	for _, key := range keys {
		if component := settingsKeyFirstComponent(key); component == "models" || strings.EqualFold(component, "roles") {
			return true
		}
	}
	return false
}

// ModelsIntegrityOf checks one config document's JSON-shaped bytes against the
// models-integrity rule: a models block must carry a valid cache_rev
// signature. A models block with no cache_rev at all is a legacy file,
// accepted only when no settings lock exists. data unreadable (nil or
// garbage) fails closed. Reload reuses this on the candidate's stable
// bytes, so a hand-edited models block never reaches a running process.
//
// data is the decoded JSON for .ktav documents (decodeConfigDocument ->
// ktavcodec.DecodeKtav + ToJSON), so no extra number normalization is
// needed before canonicalModelsJSON: the codec re-tokenizes every float
// through strconv.FormatFloat('g', -1, 64) and integers through
// strconv.FormatInt, and ToJSON emits those normalized lexemes, so a
// cache_rev computed over Ktav-normalized values at migration time
// verifies here unchanged.
func ModelsIntegrityOf(data []byte, lockExists bool) error {
	canonical, present, err := CanonicalModelsAndRolesJSON(data)
	if err != nil {
		return ErrModelsEditedDirectly
	}
	if !present {
		return nil
	}
	rev := gjson.GetBytes(data, CacheRevKey)
	if !rev.Exists() {
		// Genuinely absent: legacy file. Accepted only when unlocked.
		if lockExists {
			return ErrModelsEditedDirectly
		}
		return nil
	}
	// Present but not a valid signature string: a mismatch, locked or not.
	if rev.Type != gjson.String || rev.String() == "" {
		return ErrModelsEditedDirectly
	}
	if rev.String() != modelsIntegritySignature(canonical) {
		return ErrModelsEditedDirectly
	}
	return nil
}

var ErrModelsEditedDirectly = errors.New("models cannot be changed by editing the settings file directly; they can only be changed with the rush models commands (ask the user if that is refused)")

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

// canonicalModelsJSON canonicalizes a models value for hashing. Absent, null,
// or empty values are treated as not present.
func canonicalModelsJSON(raw string) (canonical string, present bool, err error) {
	value := gjson.Parse(raw)
	if !value.Exists() || value.Type == gjson.Null {
		return "", false, nil
	}
	if value.IsObject() && len(value.Map()) == 0 {
		return "", false, nil
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

// CanonicalModelsAndRolesJSON canonicalizes the models and custom-roles
// payload bound into cache_rev signatures. Existing model-only documents
// keep their exact v1 signature payload; role-aware payloads also bind
// tools presence, preventing unsigned widening.
func CanonicalModelsAndRolesJSON(data []byte) (string, bool, error) {
	models, modelsPresent, err := canonicalModelsJSON(gjson.GetBytes(data, "models").Raw)
	if err != nil {
		return "", false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return "", false, err
	}
	rawRoles, rolesPresent, err := CustomRolesDocumentField(document)
	if err != nil {
		return "", false, err
	}
	if !rolesPresent {
		return models, modelsPresent, nil
	}
	roles, err := DecodeCustomRoles(rawRoles)
	if err != nil {
		return "", false, err
	}
	if len(roles) == 0 {
		return models, modelsPresent, nil
	}
	canonicalRoles, _, err := canonicalModelsJSON(string(rawRoles))
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

// ModelsIntegritySignature hashes the canonicalized models value.
func ModelsIntegritySignature(canonical string) string {
	return modelsIntegritySignature(canonical)
}

// modelsIntegritySignature hashes the canonicalized models value.
func modelsIntegritySignature(canonical string) string {
	digest := sha256.Sum256([]byte("v1|" + hashSalt + "|" + canonical))
	return hex.EncodeToString(digest[:])
}

func DecodeCustomRoles(raw json.RawMessage) (map[string]CustomRole, error) {
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

// Reject aliases that encoding/json would accept outside our merge/signing seam.
func CustomRolesDocumentField(root map[string]json.RawMessage) (json.RawMessage, bool, error) {
	for field := range root {
		if strings.EqualFold(field, "roles") && field != "roles" {
			return nil, false, fmt.Errorf("custom roles must use the canonical roles key, not %q", field)
		}
	}
	value, present := root["roles"]
	return value, present, nil
}
