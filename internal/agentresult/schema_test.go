package agentresult

import "testing"

func TestCompileSchemaProfile(t *testing.T) {
	for _, raw := range []string{
		`{"type":[]}`, `{"type":["string","string"]}`,
		`{"required":["x","x"]}`, `{"enum":[]}`, `{"enum":[1,1]}`,
		`{"minItems":-1}`, `{"minimum":"1"}`, `{"items":true}`, `{"pattern":"["}`,
		`{"$defs":{"unused":{"unknown":true}}}`, `{"unknown":true}`,
	} {
		if _, err := compileSchema([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{
		`{"type":"null"}`, `{"type":["object","array"],"properties":{"x":{"type":"string"}},"required":["x"]}`,
		`{"enum":[null,1,"x"]}`, `{"$defs":{"a/b":{"type":"string"}},"$ref":"#/$defs/a~1b"}`,
		`{"$defs":{"a b":{"type":"string"}},"$ref":"#/$defs/a%20b"}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x","minLength":2}`,
		`{"type":"string","minLength":2}`,
		`{"$defs":{"x":{"type":"integer","minimum":1,"maximum":3}},"$ref":"#/$defs/x","minimum":2,"maximum":4}`,
		`{"type":"string","minLength":2}`,
	} {
		if _, err := compileSchema([]byte(raw)); err != nil {
			t.Errorf("rejected %s: %v", raw, err)
		}
	}
}

func TestCompileSchemaSemanticEdges(t *testing.T) {
	// Enum и const разрешены как самостоятельные цели ссылки, но не как значения.
	for _, raw := range []string{
		`{"$defs":{"x":{"enum":["ok"]}},"$ref":"#/$defs/x","const":"ok"}`,
		`{"$defs":{"x":{"const":"ok"}},"$ref":"#/$defs/x","enum":["ok"]}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x","minimum":1,"maximum":2}`,
		`{"type":"string","minLength":2,"maxLength":2}`,
	} {
		if _, err := compileSchema([]byte(raw)); err != nil {
			t.Errorf("отклонена цель enum/const: %s: %v", raw, err)
		}
	}
	// Enum и const разрешены как самостоятельные цели ссылки, но не как значения.
	for _, raw := range []string{
		`{"$defs":{"x":{"enum":[{}]}},"$ref":"#/$defs/x/enum/0"}`,
		`{"$defs":{"x":{"const":{}}},"$ref":"#/$defs/x/const"}`,
		`{"$defs":{"x":{"properties":{"a":{}}}},"$ref":"#/$defs/x/properties"}`,
	} {
		if _, err := compileSchema([]byte(raw)); err == nil {
			t.Errorf("принято некорректное правило: %s", raw)
		}
	}
}

func TestCandidateJSONEdges(t *testing.T) {
	// Проверяем точные границы JSON-разбора, Unicode и безопасную точность чисел.
	compiled, err := Compile(Spec{Type: "json_schema", Schema: []byte(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		`{"a":{"x":1,"x":2}}`, `{"x":1} trailing`, "```json\n{}\n```", `{"n":9007199254740993}`,
	} {
		if err := compiled.Validate(candidate); err == nil {
			t.Errorf("принят кандидат %q", candidate)
		}
	}
	if _, err := Compile(Spec{Type: "json_schema", Schema: make([]byte, MaxSchemaBytes+1)}); err == nil {
		t.Fatal("принята схема больше лимита")
	}
}

func TestCompileSchemaRejectsCyclesAndLimits(t *testing.T) {
	for _, raw := range []string{
		`{"$defs":{"a":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`,
		`{"$ref":"#/$defs/x"}`, `{"$ref":"#/$defs/a~2b"}`,
		`{"$defs":{"a":{"type":"string"}},"$ref":"#/$defs"}`,
	} {
		if _, err := compileSchema([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
