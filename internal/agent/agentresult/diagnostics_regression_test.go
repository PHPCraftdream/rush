package agentresult

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnosticsUnionTypeReportsActualFailure(t *testing.T) {
	for _, tc := range []struct {
		name, schema, candidate, code string
	}{
		{"object-or-null", `{"type":["object","null"],"required":["x"]}`, `{}`, "required"},
		{"string-or-null", `{"type":["string","null"],"minLength":2}`, `""`, "minLength"},
		{"integer-or-string", `{"type":["integer","string"],"minimum":10}`, `5`, "minimum"},
		{"no-matching-type", `{"type":["string","null"]}`, `5`, "type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := diagDiagnostics(t, diagCompile(t, tc.schema).Validate(tc.candidate))
			if len(diags) != 1 || diags[0].Code != tc.code {
				t.Fatalf("expected true keyword %s, got %+v", tc.code, diags)
			}
		})
	}
}

func TestDiagnosticsRedactedExtraDoesNotRestartInstancePath(t *testing.T) {
	c := diagCompile(t, `{"additionalProperties":{"properties":{"inner":{"type":"string"}}}}`)
	err := c.Validate(`{"LEAKED_TOKEN":{"inner":1}}`)
	diags := diagDiagnostics(t, err)
	if len(diags) != 1 || diags[0].Code != "type" || diags[0].SchemaPointer != "/additionalProperties/properties/inner" || diags[0].InstancePointer != "" {
		t.Fatalf("redacted path became a false root property: %+v", diags)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || strings.Contains(ve.Feedback(1, 1), "LEAKED_TOKEN") {
		t.Fatal("candidate key leaked into feedback")
	}
}

func TestDiagnosticsTrustedFeedbackActuallyOverflows(t *testing.T) {
	properties := map[string]any{}
	candidate := map[string]any{}
	for i := range 8 {
		name := fmt.Sprintf("k%d%s", i, strings.Repeat("p", 440))
		properties[name] = map[string]any{"properties": map[string]any{"v": map[string]any{"type": "integer"}}}
		candidate[name] = map[string]any{"v": "wrong type"}
	}
	schema, err := json.Marshal(map[string]any{"properties": properties})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	validationErr := diagCompile(t, string(schema)).Validate(string(raw))
	var ve *ValidationError
	if !errors.As(validationErr, &ve) || len(ve.Diagnostics) != 8 || len(ve.trusted) != 8 {
		t.Fatalf("expected eight real trusted failures: %v", validationErr)
	}
	for _, d := range ve.Diagnostics {
		if len(d.SchemaPointer) < 400 || len(d.InstancePointer) < 400 {
			t.Fatalf("fixture does not exercise long trusted paths: %+v", d)
		}
	}
	unbounded, err := json.Marshal(map[string]any{"code": CodeMismatch, "attempt": 1, "remaining": 1, "diagnostics": ve.Diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded) <= 4096 {
		t.Fatalf("fixture did not cross feedback limit: %d", len(unbounded))
	}
	feedback := ve.Feedback(1, 1)
	var envelope struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if len(feedback) > 4096 || json.Unmarshal([]byte(feedback), &envelope) != nil || len(envelope.Diagnostics) != 0 {
		t.Fatalf("trusted overflow was not reduced safely: %s", feedback)
	}
}

func TestDiagnosticConversionsPreserveTrustedPaths(t *testing.T) {
	err := diagCompile(t, `{"properties":{"value":{"type":"integer"}}}`).Validate(`{"value":"wrong type"}`)
	var original *ValidationError
	if !errors.As(err, &original) {
		t.Fatal("missing validation error")
	}
	var contract *Error
	if !errors.As(err, &contract) {
		t.Fatal("missing contract error")
	}
	var roundtrip *ValidationError
	if !errors.As(contract, &roundtrip) {
		t.Fatal("missing validation error after conversion")
	}
	if original.Feedback(1, 2) != roundtrip.Feedback(1, 2) {
		t.Fatal("error conversion lost trusted diagnostics")
	}
	if roundtrip.Diagnostics[0].InstancePointer != "/value" {
		t.Fatalf("wrong original instance path: %+v", roundtrip.Diagnostics)
	}
}
