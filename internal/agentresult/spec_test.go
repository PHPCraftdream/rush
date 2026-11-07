package agentresult

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUncompiledContractFailsClosed(t *testing.T) {
	for _, compiled := range []*Compiled{nil, new(Compiled)} {
		err := compiled.Validate("{}")
		var contract *Error
		if !errors.As(err, &contract) || contract.Code != CodeInvalid {
			t.Errorf("Validate on uncompiled contract: %v", err)
		}
	}
	for _, spec := range []Spec{
		{Type: "regex", Pattern: ptrString("x"), MaxRepairs: ptrInt(-1)},
		{Type: "regex", Pattern: ptrString("x"), MaxRepairs: ptrInt(6)},
		{Type: "regex", Pattern: ptrString("x"), Schema: []byte(`{}`)},
		{Type: "unknown"},
	} {
		if _, err := Compile(spec); err == nil {
			t.Errorf("accepted invalid programmatic spec: %+v", spec)
		}
	}
}

func ptrInt(i int) *int { return &i }

func TestCompileSpecBoundaries(t *testing.T) {
	// Проверяем типы, ограничения ремонта и строгое полное сопоставление.
	for _, raw := range []string{`null`, `[]`, `{"type":null}`, `{"type":"regex","pattern":null}`, `{"type":"regex","pattern":"["}`, `{"type":"regex","pattern":"x","max_repairs":6}`, `{"type":"regex","pattern":"x","extra":true}`} {
		if _, err := DecodeSpec([]byte(raw)); err == nil {
			t.Errorf("принята спецификация %s", raw)
		}
	}
	for value := 0; value <= 5; value++ {
		compiled, err := Compile(Spec{Type: "regex", Pattern: ptrString("a.*b"), MaxRepairs: &value})
		if err != nil || compiled.EffectiveMaxRepairs() != value {
			t.Fatalf("max repairs %d: %v", value, err)
		}
	}
	for _, pattern := range []string{"a.*b", "(?m)^x$", `a\s+b`, "", "(?=x)", `(x)\1`} {
		_, err := Compile(Spec{Type: "regex", Pattern: ptrString(pattern)})
		if pattern == "(?=x)" || pattern == `(x)\1` {
			if err == nil {
				t.Errorf("принят неподдерживаемый шаблон %q", pattern)
			}
		} else if err != nil {
			t.Errorf("отклонён шаблон %q: %v", pattern, err)
		}
	}
	compiled, err := Compile(Spec{Type: "regex", Pattern: ptrString("a.*b")})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"a\nb", "xa-b", "a-bx"} {
		if compiled.Validate(candidate) == nil {
			t.Errorf("принят кандидат %q", candidate)
		}
	}
	if _, err := Compile(Spec{Type: "regex", Pattern: ptrString(string([]byte{0xff}))}); err == nil {
		t.Fatal("принят невалидный UTF-8")
	}
	if _, err := Compile(Spec{Type: "regex", Pattern: ptrString(strings.Repeat("a", MaxPatternBytes+1))}); err == nil {
		t.Fatal("принят слишком длинный шаблон")
	}
}

func ptrString(s string) *string { return &s }

func TestFeedbackIsBoundedAndSanitized(t *testing.T) {
	ve := &ValidationError{Diagnostics: []Diagnostic{{Code: "schema_mismatch", Reason: strings.Repeat("secret", 10000), SchemaPointer: "/secret"}}}
	var contract *Error
	if !errors.As(ve, &contract) || contract.Code != CodeMismatch {
		t.Fatal("errors.As не преобразовал mismatch")
	}
	feedback := ve.Feedback(1, 2)
	// Секретная reason-строка стирается; caller-constructed указатель "/secret"
	// опускается: provenance-контракт — указатели только из доверенного снапшота
	// структурного валидатора, а эта ValidationError сконструирована снаружи.
	if len(feedback) > 4096 || strings.Contains(feedback, strings.Repeat("secret", 2)) || strings.Contains(feedback, "/secret") {
		t.Fatal("feedback не ограничен или содержит секретную причину/указатель")
	}
	var obj map[string]any
	if json.Unmarshal([]byte(feedback), &obj) != nil {
		t.Fatalf("feedback не является валидным JSON: %s", feedback)
	}
	diags, ok := obj["diagnostics"].([]any)
	if !ok || len(diags) != 1 {
		t.Fatalf("ожидалась одна диагностика: %s", feedback)
	}
	d := diags[0].(map[string]any)
	// Указатель опущен (""), причина сохранена из allowlist — усиление no-leak.
	if d["schema_pointer"] != "" || d["instance_pointer"] != "" || d["reason"] != "candidate does not satisfy schema" {
		t.Fatalf("неожиданная диагностика: %s", feedback)
	}
	// Большой Error с неизвестным кодом сохраняет fallback-поведение.
	big := (&Error{Code: Code("private"), Diagnostics: make([]Diagnostic, 100000)}).Feedback(0, 0)
	if len(big) > 4096 || strings.Contains(big, "private") {
		t.Fatal("небезопасный feedback")
	}
}

func TestEffectiveMaxRepairsDoesNotMutateSpec(t *testing.T) {
	value := 2
	s := Spec{Type: "regex", Pattern: ptrString("x"), MaxRepairs: &value}
	if s.EffectiveMaxRepairs() != 2 || value != 2 {
		t.Fatal("spec был изменён")
	}
	c, err := Compile(s)
	if err != nil {
		t.Fatal(err)
	}
	value = 5
	if c.EffectiveMaxRepairs() != 2 {
		t.Fatal("compiled зависит от исходного указателя")
	}
}
