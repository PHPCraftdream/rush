package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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
