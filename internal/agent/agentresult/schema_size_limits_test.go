package agentresult

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type sizeLimitCase struct {
	keyword string
	kind    string
	minimum bool
}

var sizeLimitCases = []sizeLimitCase{
	{keyword: "minItems", kind: "array", minimum: true},
	{keyword: "maxItems", kind: "array"},
	{keyword: "minProperties", kind: "object", minimum: true},
	{keyword: "maxProperties", kind: "object"},
	{keyword: "minLength", kind: "string", minimum: true},
	{keyword: "maxLength", kind: "string"},
}

func sizeLimitCandidate(kind string, count int) string {
	switch kind {
	case "array":
		return "[" + strings.TrimSuffix(strings.Repeat("null,", count), ",") + "]"
	case "object":
		var members []string
		for i := 0; i < count; i++ {
			members = append(members, fmt.Sprintf("%q:null", fmt.Sprintf("p%d", i)))
		}
		return "{" + strings.Join(members, ",") + "}"
	case "string":
		b, _ := json.Marshal(strings.Repeat("x", count))
		return string(b)
	default:
		panic("unknown size limit kind")
	}
}

func TestSchemaSizeLimitsLargeBounds(t *testing.T) {
	bounds := []string{"2147483647", "2147483648", "9007199254740991"}
	for _, tc := range sizeLimitCases {
		for _, bound := range bounds {
			for _, localRef := range []bool{false, true} {
				name := tc.keyword + "/" + bound
				if localRef {
					name += "/local-ref"
				}
				t.Run(name, func(t *testing.T) {
					node := fmt.Sprintf(`{"type":%q,"%s":%s}`, tc.kind, tc.keyword, bound)
					schema := node
					if localRef {
						schema = `{"$defs":{"target":` + node + `},"$ref":"#/$defs/target"}`
					}
					compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
					if err != nil {
						t.Fatalf("large limit rejected: %v", err)
					}
					candidate := sizeLimitCandidate(tc.kind, 2)
					if err := compiled.Validate(candidate); tc.minimum && err == nil || !tc.minimum && err != nil {
						t.Fatalf("limit %s candidate %s: %v", bound, candidate, err)
					}
				})
			}
		}
	}
}

func TestSchemaSizeLimitsSmallBoundaries(t *testing.T) {
	for _, tc := range sizeLimitCases {
		for bound := 0; bound <= 2; bound++ {
			for count := 0; count <= 2; count++ {
				schema := fmt.Sprintf(`{"type":%q,"%s":%d}`, tc.kind, tc.keyword, bound)
				compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
				if err != nil {
					t.Fatalf("compile %s: %v", schema, err)
				}
				want := count >= bound
				if !tc.minimum {
					want = count <= bound
				}
				candidate := sizeLimitCandidate(tc.kind, count)
				if err := compiled.Validate(candidate); (err == nil) != want {
					t.Errorf("%s=%d candidate %s: err=%v, want accepted=%v", tc.keyword, bound, candidate, err, want)
				}
			}
		}
	}
}

func TestSchemaSizeLimitsIgnoreOtherInstanceTypes(t *testing.T) {
	irrelevant := map[string][]string{
		"minItems":      {`null`, `"text"`},
		"maxItems":      {`null`, `"text"`},
		"minProperties": {`null`, `[]`},
		"maxProperties": {`null`, `[]`},
		"minLength":     {`null`, `{}`},
		"maxLength":     {`null`, `{}`},
	}
	for keyword, candidates := range irrelevant {
		schema := `{"` + keyword + `":9007199254740991}`
		compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
		if err != nil {
			t.Fatalf("compile irrelevant %s: %v", keyword, err)
		}
		for _, candidate := range candidates {
			if err := compiled.Validate(candidate); err != nil {
				t.Errorf("%s unexpectedly restricts %s: %v", keyword, candidate, err)
			}
		}
	}
}

func TestSchemaSizeLimitsNestedItemsAndAdditionalProperties(t *testing.T) {
	for _, tc := range []struct {
		schema, accepted, rejected string
	}{
		{`{"type":"array","items":{"type":"string","maxLength":1}}`, `["x"]`, `["xy"]`},
		{`{"type":"array","items":{"type":"string","maxLength":9007199254740991}}`, `["xy"]`, `[2]`},
		{`{"type":"array","items":{"type":"string","minLength":9007199254740991}}`, `[]`, `["x"]`},
		{`{"type":"object","additionalProperties":{"type":"string","maxLength":1}}`, `{"x":"x"}`, `{"x":"xy"}`},
		{`{"type":"object","additionalProperties":{"type":"string","maxLength":9007199254740991}}`, `{"x":"xy"}`, `{"x":2}`},
		{`{"type":"object","additionalProperties":{"type":"string","minLength":9007199254740991}}`, `{}`, `{"x":"x"}`},
	} {
		compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(tc.schema)})
		if err != nil {
			t.Fatalf("compile nested schema: %v", err)
		}
		if err := compiled.Validate(tc.accepted); err != nil {
			t.Errorf("accepted candidate %s: %v", tc.accepted, err)
		}
		if err := compiled.Validate(tc.rejected); err == nil {
			t.Errorf("accepted candidate that violates nested size limit: %s", tc.rejected)
		}
	}
}

func TestSchemaSizeLimitsRejectInvalidIntegersBeforeClamp(t *testing.T) {
	for _, tc := range sizeLimitCases {
		for _, value := range []string{"-1", "1.5", "9007199254740992"} {
			schema := `{"` + tc.keyword + `":` + value + `}`
			if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)}); err == nil {
				t.Errorf("accepted invalid %s value %s", tc.keyword, value)
			}
		}
	}
}

func TestSchemaSizeLimitsIntegralDecimalFormsAndSpecImmutability(t *testing.T) {
	for _, value := range []string{"2147483648.0", "2.147483648e9", "9007199254740991e0"} {
		originalSchema := []byte(`{"type":"array","minItems":` + value + `}`)
		spec := Spec{Type: "json_schema", Schema: append(json.RawMessage(nil), originalSchema...)}
		before := append([]byte(nil), spec.Schema...)
		if _, err := Compile(spec); err != nil {
			t.Errorf("integral decimal %s rejected: %v", value, err)
		}
		if !bytes.Equal(spec.Schema, before) {
			t.Errorf("Compile changed serialized Spec schema for %s", value)
		}
		decoded, err := DecodeSpec([]byte(`{"type":"json_schema","schema":` + string(originalSchema) + `}`))
		if err != nil {
			t.Errorf("DecodeSpec integral decimal %s: %v", value, err)
			continue
		}
		if !bytes.Equal(decoded.Schema, originalSchema) {
			t.Errorf("DecodeSpec changed RawMessage for %s: got %s", value, decoded.Schema)
		}
	}
}

func TestSchemaSizeLimitsDoNotClampNamesEnumOrConst(t *testing.T) {
	for _, propertyName := range []string{"9007199254740991", strings.Repeat("p", 2048)} {
		schema := `{"type":"object","required":[` + fmt.Sprintf("%q", propertyName) + `],"properties":{` + fmt.Sprintf("%q", propertyName) + `:{"const":9007199254740991}}}`
		compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
		if err != nil {
			t.Fatal(err)
		}
		candidate := `{` + fmt.Sprintf("%q", propertyName) + `:9007199254740991}`
		if err := compiled.Validate(candidate); err != nil {
			t.Errorf("property name/const changed for %q: %v", propertyName, err)
		}
		if err := compiled.Validate(`{` + fmt.Sprintf("%q", propertyName) + `:9007199254740990}`); err == nil {
			t.Errorf("const was clamped for property %q", propertyName)
		}
	}
	for _, schema := range []string{`{"enum":[9007199254740991]}`, `{"const":9007199254740991}`} {
		compiled, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
		if err != nil {
			t.Fatalf("compile enum/const: %v", err)
		}
		if err := compiled.Validate(`9007199254740991`); err != nil {
			t.Errorf("enum/const value was changed in %s: %v", schema, err)
		}
		if err := compiled.Validate(`9007199254740990`); err == nil {
			t.Errorf("enum/const was clamped in %s", schema)
		}
	}
}
