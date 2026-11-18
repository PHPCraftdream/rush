package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

const RoleContractVersion = 1

// RoleContract is the immutable model selection and tool-cap snapshot bound
// to a child session. Each inherited group is an independent ancestor ceiling.
type RoleContract struct {
	Version   int        `json:"version"`
	Name      string     `json:"name"`
	Model     ModelCfg   `json:"model"`
	Tools     *[]string  `json:"tools,omitempty"`
	Inherited [][]string `json:"inherited,omitempty"`
}

// Clone returns a deep copy, including tool groups and mutable model options.
func (c *RoleContract) Clone() *RoleContract {
	if c == nil {
		return nil
	}
	out := *c
	if c.Tools != nil {
		var tools []string
		if *c.Tools != nil {
			tools = append([]string{}, (*c.Tools)...)
		}
		out.Tools = &tools
	}
	if c.Inherited != nil {
		out.Inherited = make([][]string, len(c.Inherited))
		for i, group := range c.Inherited {
			if group != nil {
				out.Inherited[i] = append([]string{}, group...)
			}
		}
	}
	out.Model = cloneRoleModel(c.Model)
	return &out
}

func cloneRoleModel(model ModelCfg) ModelCfg {
	if model.Temperature != nil {
		value := *model.Temperature
		model.Temperature = &value
	}
	if model.TopP != nil {
		value := *model.TopP
		model.TopP = &value
	}
	if model.TopK != nil {
		value := *model.TopK
		model.TopK = &value
	}
	if model.FrequencyPenalty != nil {
		value := *model.FrequencyPenalty
		model.FrequencyPenalty = &value
	}
	if model.PresencePenalty != nil {
		value := *model.PresencePenalty
		model.PresencePenalty = &value
	}
	if model.ProviderOptions != nil {
		model.ProviderOptions = cloneRoleMap(model.ProviderOptions)
	}
	return model
}

func cloneRoleMap(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		reflected := reflect.ValueOf(value)
		if !reflected.IsValid() {
			out[key] = nil
			continue
		}
		out[key] = cloneRoleValue(reflected).Interface()
	}
	return out
}

func cloneRoleValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(cloneRoleValue(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(cloneRoleValue(iter.Key()), cloneRoleValue(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneRoleValue(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneRoleValue(value.Index(i)))
		}
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(cloneRoleValue(value.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if out.Field(i).CanSet() && value.Field(i).CanInterface() {
				out.Field(i).Set(cloneRoleValue(value.Field(i)))
			}
		}
		return out
	default:
		return value
	}
}

// Equal compares the complete serialized contract, including omitted-versus-empty tools.
func (c *RoleContract) Equal(other *RoleContract) bool {
	if c == nil || other == nil {
		return c == other
	}
	left, leftErr := json.Marshal(c)
	right, rightErr := json.Marshal(other)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// Validate rejects unsupported, incomplete, and malformed role snapshots.
func (c *RoleContract) Validate() error {
	if c == nil {
		return errors.New("role contract is required")
	}
	if c.Version != RoleContractVersion {
		return fmt.Errorf("unsupported role contract version %d", c.Version)
	}
	if c.Name != "" {
		if err := validateRoleName(c.Name); err != nil {
			return err
		}
		if strings.TrimSpace(c.Model.Provider) == "" || strings.TrimSpace(c.Model.Model) == "" {
			return errors.New("named role contract requires provider and model")
		}
	} else if (c.Model.Provider == "") != (c.Model.Model == "") {
		return errors.New("inherited-only role contract model must be complete or omitted")
	}
	if c.Tools != nil {
		if *c.Tools == nil {
			return errors.New("role contract tools must be an array, not null")
		}
		if err := validateRoleToolGroup(*c.Tools); err != nil {
			return fmt.Errorf("invalid role contract tools: %w", err)
		}
	}
	for i, group := range c.Inherited {
		if group == nil {
			return fmt.Errorf("inherited tool group %d must be an array", i)
		}
		if err := validateRoleToolGroup(group); err != nil {
			return fmt.Errorf("invalid inherited tool group %d: %w", i, err)
		}
	}
	if _, err := json.Marshal(c.Model); err != nil {
		return fmt.Errorf("invalid role contract model snapshot: %w", err)
	}
	return nil
}

func validateRoleName(name string) error {
	if !utf8.ValidString(name) || name == "" || utf8.RuneCountInString(name) > 64 || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid custom role name %q", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return fmt.Errorf("invalid custom role name %q", name)
		}
	}
	switch name {
	case "smart", "fast", "worker", "reviewer":
		return nil
	}
	if strings.EqualFold(name, "smart") || strings.EqualFold(name, "fast") || strings.EqualFold(name, "worker") || strings.EqualFold(name, "reviewer") {
		return fmt.Errorf("custom role name %q is a reserved builtin variant", name)
	}
	return nil
}

func validateRoleToolGroup(tools []string) error {
	if len(tools) > 128 {
		return errors.New("too many tool patterns")
	}
	for _, tool := range tools {
		if !utf8.ValidString(tool) || tool == "" || strings.TrimSpace(tool) != tool || len(tool) > 256 {
			return fmt.Errorf("invalid tool pattern %q", tool)
		}
		for _, r := range tool {
			if unicode.IsControl(r) {
				return fmt.Errorf("invalid tool pattern %q", tool)
			}
		}
	}
	return nil
}

func decodeRoleContract(raw string) (*RoleContract, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var contract RoleContract
	if err := decoder.Decode(&contract); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("role contract contains trailing JSON")
		}
		return nil, err
	}
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	return contract.Clone(), nil
}

// MarshalJSON omits a zero model snapshot for inherited-only contracts.
func (c RoleContract) MarshalJSON() ([]byte, error) {
	type contractJSON struct {
		Version   int        `json:"version"`
		Name      string     `json:"name"`
		Model     *ModelCfg  `json:"model,omitempty"`
		Tools     *[]string  `json:"tools,omitempty"`
		Inherited [][]string `json:"inherited,omitempty"`
	}
	encoded := contractJSON{
		Version:   c.Version,
		Name:      c.Name,
		Tools:     c.Tools,
		Inherited: c.Inherited,
	}
	if c.Model.Provider != "" || c.Model.Model != "" || c.Model.ReasoningEffort != "" || c.Model.Think || c.Model.MaxTokens != 0 || c.Model.Temperature != nil || c.Model.TopP != nil || c.Model.TopK != nil || c.Model.FrequencyPenalty != nil || c.Model.PresencePenalty != nil || c.Model.ProviderOptions != nil {
		model := c.Model
		encoded.Model = &model
	}
	return json.Marshal(encoded)
}

func (c *RoleContract) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("role contract must be an object")
	}
	for key, value := range fields {
		switch key {
		case "version", "name", "model", "tools", "inherited":
		default:
			return fmt.Errorf("unknown role contract field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("role contract field %q must not be null", key)
		}
	}
	for _, required := range []string{"version", "name"} {
		if _, ok := fields[required]; !ok {
			return fmt.Errorf("role contract field %q is required", required)
		}
	}
	type contractJSON RoleContract
	var decoded contractJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	modelJSON := fields["model"]
	if len(modelJSON) != 0 {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(modelJSON, &nested); err != nil || nested == nil {
			return errors.New("role contract model must be an object")
		}
		for key, value := range nested {
			switch key {
			case "model", "provider", "reasoning_effort", "think", "max_tokens", "temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty", "provider_options":
			default:
				return fmt.Errorf("unknown role model field %q", key)
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("role model field %q must not be null", key)
			}
		}
		var model ModelCfg
		modelDecoder := json.NewDecoder(bytes.NewReader(modelJSON))
		modelDecoder.DisallowUnknownFields()
		if err := modelDecoder.Decode(&model); err != nil {
			return fmt.Errorf("invalid role contract model snapshot: %w", err)
		}
		if strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" {
			return errors.New("role contract model snapshot must include provider and model")
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("role contract contains trailing JSON")
		}
		return err
	}
	candidate := RoleContract(decoded)
	if err := candidate.Validate(); err != nil {
		return err
	}
	*c = candidate
	return nil
}
