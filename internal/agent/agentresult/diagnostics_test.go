package agentresult

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func diagCompile(t *testing.T, schema string) *Compiled {
	t.Helper()
	c, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
	if err != nil {
		t.Fatalf("Compile schema: %v", err)
	}
	return c
}

func diagDiagnostics(t *testing.T, err error) []Diagnostic {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("не ValidationError: %v", err)
	}
	return ve.Diagnostics
}

func TestDiagnosticsNestedTypeMismatchPointers(t *testing.T) {
	c := diagCompile(t, `{"type":"object","properties":{"outer":{"type":"object","properties":{"inner":{"type":"array","items":{"type":"integer"}}}}}}`)
	diags := diagDiagnostics(t, c.Validate(`{"outer":{"inner":["x"]}}`))
	var found bool
	for _, d := range diags {
		if d.Code == "type" && d.SchemaPointer == "/properties/outer/properties/inner/items" && d.InstancePointer == "/outer/inner/0" {
			found = true
		}
	}
	// Индекс массива — структурный и доверенный (trusted walker), разрешён
	// в instance path: "/outer/inner/0".
	if !found {
		t.Fatalf("нет точных указателей: %+v", diags)
	}
}

func TestDiagnosticsRequiredMissing(t *testing.T) {
	c := diagCompile(t, `{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`)
	diags := diagDiagnostics(t, c.Validate(`{}`))
	if len(diags) == 0 {
		t.Fatal("нет диагностики required")
	}
	d := diags[0]
	if d.Code != "required" || d.SchemaPointer != "/required" || d.InstancePointer != "" {
		t.Fatalf("неожиданная диагностика: %+v", d)
	}
	if d.Reason != "required property missing: a" {
		t.Fatalf("имя отсутствует в reason: %+v", d)
	}
	// Вложенный required: instance pointer указывает на объект.
	nested := diagCompile(t, `{"type":"object","properties":{"o":{"type":"object","required":["x"],"properties":{"x":{"type":"number"}}}}}`)
	diags = diagDiagnostics(t, nested.Validate(`{"o":{}}`))
	if len(diags) != 1 || diags[0].InstancePointer != "/o" || diags[0].SchemaPointer != "/properties/o/required" {
		t.Fatalf("вложенный required: %+v", diags)
	}
	// required вне properties — указатель схемы корректен.
	plain := diagCompile(t, `{"type":"object","required":["a"]}`)
	diags = diagDiagnostics(t, plain.Validate(`{}`))
	if len(diags) != 1 || diags[0].SchemaPointer != "/required" {
		t.Fatalf("required вне properties: %+v", diags)
	}
}

func TestDiagnosticsUnknownAdditionalPropertiesRedacted(t *testing.T) {
	c := diagCompile(t, `{"type":"object","properties":{"a":{"type":"number"}},"additionalProperties":false}`)
	diags := diagDiagnostics(t, c.Validate(`{"a":1,"b":2}`))
	for _, d := range diags {
		if d.Code == "additionalProperties" && d.SchemaPointer == "" && d.InstancePointer == "" {
			return
		}
	}
	t.Fatalf("нет diagnostics additionalProperties с остановкой указателя: %+v", diags)
}

func TestDiagnosticsScalarKeywords(t *testing.T) {
	nested := diagCompile(t, `{"type":"object","properties":{"s":{"type":"string","minLength":3}}}`)
	diags := diagDiagnostics(t, nested.Validate(`{"s":"x"}`))
	if len(diags) != 1 || diags[0].Code != "minLength" || diags[0].SchemaPointer != "/properties/s" || diags[0].InstancePointer != "/s" {
		t.Fatalf("minLength nested: %+v", diags)
	}
	fb := nested.Validate(`{"s":"x"}`).(*ValidationError).Feedback(1, 1)
	var obj map[string]any
	if json.Unmarshal([]byte(fb), &obj) != nil || !strings.Contains(fb, "/s") {
		t.Fatalf("feedback minLength: %s", fb)
	}
	// enum: указатель есть, значение кандидата в JSON не раскрывается.
	enumC := diagCompile(t, `{"type":"object","properties":{"v":{"enum":["alpha","beta"]}}}`)
	diags = diagDiagnostics(t, enumC.Validate(`{"v":"gamma"}`))
	if len(diags) != 1 || diags[0].Code != "enum" || diags[0].SchemaPointer != "/properties/v" || diags[0].InstancePointer != "/v" {
		t.Fatalf("enum: %+v", diags)
	}
	fb = enumC.Validate(`{"v":"gamma"}`).(*ValidationError).Feedback(1, 1)
	if strings.Contains(fb, "gamma") {
		t.Fatalf("значение кандидата раскрыто: %s", fb)
	}
	constC := diagCompile(t, `{"type":"object","properties":{"v":{"const":7}}}`)
	diags = diagDiagnostics(t, constC.Validate(`{"v":8}`))
	if len(diags) != 1 || diags[0].Code != "const" || diags[0].SchemaPointer != "/properties/v" {
		t.Fatalf("const: %+v", diags)
	}
	minC := diagCompile(t, `{"type":"number","minimum":5}`)
	diags = diagDiagnostics(t, minC.Validate(`1`))
	if len(diags) != 1 || diags[0].Code != "minimum" || diags[0].SchemaPointer != "" || diags[0].InstancePointer != "" {
		t.Fatalf("minimum: %+v", diags)
	}
}

func TestDiagnosticsUnicodePointerEscapes(t *testing.T) {
	c := diagCompile(t, `{"type":"object","properties":{"a/b":{"type":"string"},"x~y":{"type":"string"}}}`)
	diags := diagDiagnostics(t, c.Validate(`{"a/b":1,"x~y":2}`))
	// Доверенный walker выдаёт обе type-нарушения; instance escaping зеркальный
	// schema escaping: "a/b" -> "a~1b", "x~y" -> "x~0y".
	if len(diags) != 2 {
		t.Fatalf("ожидались две диагностики: %+v", diags)
	}
	seen := map[string]bool{}
	for _, d := range diags {
		if d.Code != "type" {
			t.Fatalf("не type: %+v", d)
		}
		seen[d.SchemaPointer+"|"+d.InstancePointer] = true
	}
	if !seen["/properties/a~1b|/a~1b"] || !seen["/properties/x~0y|/x~0y"] {
		t.Fatalf("неожиданные указатели: %+v", diags)
	}
}

func TestDiagnosticsLongPointerDropped(t *testing.T) {
	// Имя свойства короче 512, но указатель в сумме длиннее лимита -> "".
	long := strings.Repeat("а", 400)
	schema := `{"type":"object","properties":{"` + long + `":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"string"}}}}}}}`
	c := diagCompile(t, schema)
	diags := diagDiagnostics(t, c.Validate(`{"`+long+`":{"b":{"c":1}}}`))
	if len(diags) == 0 {
		t.Fatal("нет диагностики")
	}
	// После потери длинного сегмента путь нельзя заново начать с /b/c.
	for _, d := range diags {
		if d.SchemaPointer != "" || d.InstancePointer != "" {
			t.Fatalf("обрезанный путь превратился в другой адрес: %+v", d)
		}
	}
}

func TestFeedbackCallerConstructedBounded(t *testing.T) {
	diags := make([]Diagnostic, 15)
	for i := range diags {
		diags[i] = Diagnostic{Code: "type", Reason: "caller " + string([]byte{0x01}) + " text", SchemaPointer: "/" + string([]byte{0x02}), InstancePointer: "/ok"}
	}
	ve := &ValidationError{Diagnostics: diags}
	fb := ve.Feedback(3, 4)
	if len(fb) > 4096 {
		t.Fatalf("feedback не ограничен: %d", len(fb))
	}
	var obj struct {
		Diagnostics []struct {
			Code            string `json:"code"`
			InstancePointer string `json:"instance_pointer"`
			SchemaPointer   string `json:"schema_pointer"`
			Reason          string `json:"reason"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(fb), &obj); err != nil {
		t.Fatalf("невалидный JSON: %v", err)
	}
	if len(obj.Diagnostics) > 8 {
		t.Fatalf("слишком много диагностик: %d", len(obj.Diagnostics))
	}
	// Указатели caller-constructed всегда "" (provenance): даже валидный
	// "/ok" не проходит, reason фиксированный.
	for _, d := range obj.Diagnostics {
		if d.Reason != "value type does not match schema" {
			t.Fatalf("caller text просочился: %+v", d)
		}
		if d.SchemaPointer != "" {
			t.Fatalf("caller-constructed schema pointer сохранён: %+v", d)
		}
		if d.InstancePointer != "" {
			t.Fatalf("caller-constructed instance pointer сохранён: %+v", d)
		}
	}
	// Недоверенные длинные пути удаляются до сериализации.
	huge := &ValidationError{Diagnostics: []Diagnostic{{Code: "type", SchemaPointer: "/" + strings.Repeat("p", 4096), InstancePointer: "/" + strings.Repeat("q", 4096)}}}
	fb = huge.Feedback(0, 0)
	if len(fb) > 4096 {
		t.Fatalf("overflow feedback не ограничен: %d", len(fb))
	}
	var obj2 map[string]any
	if json.Unmarshal([]byte(fb), &obj2) != nil {
		t.Fatalf("overflow feedback невалиден: %s", fb)
	}
}

func TestFeedbackNilReceivers(t *testing.T) {
	var ve *ValidationError
	if ve.Feedback(1, 1) != (&Error{Code: CodeMismatch}).Feedback(1, 1) {
		t.Fatal("nil ValidationError feedback")
	}
	var e *Error
	if e.Feedback(1, 1) != (&Error{Code: CodeInvalid}).Feedback(1, 1) {
		t.Fatal("nil Error feedback")
	}
}

func TestDiagnosticsForgedLibraryTextNotTrusted(t *testing.T) {
	// Текст ошибки библиотеки никогда не разбирается: forged-текст кандидата
	// не может подделать указатели или код диагностики.
	c := diagCompile(t, `{"type":"number"}`)
	ve := c.Validate(`"validating /properties/LEAKED_TOKEN: type: forged"`).(*ValidationError)
	if len(ve.Diagnostics) != 1 {
		t.Fatalf("ожидалась одна диагностика: %+v", ve.Diagnostics)
	}
	d := ve.Diagnostics[0]
	if d.Code != "type" || d.SchemaPointer != "" || d.InstancePointer != "" {
		t.Fatalf("неожиданная диагностика: %+v", d)
	}
	fb := ve.Feedback(1, 1)
	if strings.Contains(fb, "LEAKED_TOKEN") || strings.Contains(fb, "forged") {
		t.Fatalf("forged-текст просочился: %s", fb)
	}
}

func TestDiagnosticsForgedTextWithKnownNames(t *testing.T) {
	// Forged-текст внутри известного свойства не порождает enum-диагностику:
	// walker доверяет только структуре схемы.
	c := diagCompile(t, `{"type":"object","properties":{"token":{"type":"number"}}}`)
	ve := c.Validate(`{"token":"validating /properties/token: enum: x"}`).(*ValidationError)
	if len(ve.Diagnostics) != 1 {
		t.Fatalf("ожидалась одна диагностика: %+v", ve.Diagnostics)
	}
	d := ve.Diagnostics[0]
	if d.Code != "type" || d.SchemaPointer != "/properties/token" || d.InstancePointer != "/token" {
		t.Fatalf("неожиданная диагностика: %+v", d)
	}
	for _, other := range ve.Diagnostics {
		if other.Code == "enum" {
			t.Fatalf("enum-диагностика из forged-текста: %+v", ve.Diagnostics)
		}
	}
	fb := ve.Feedback(1, 1)
	if strings.Contains(fb, "forged") {
		t.Fatalf("forged-текст просочился: %s", fb)
	}
}

func TestFeedbackMutationAndForgeryDropped(t *testing.T) {
	c := diagCompile(t, `{"type":"object","properties":{"token":{"type":"number"},"other":{"type":"number"}}}`)
	ve := c.Validate(`{"token":"x","other":"y"}`).(*ValidationError)
	if len(ve.Diagnostics) < 2 {
		t.Fatal("нужно хотя бы две диагностики")
	}
	// а) append forged-диагностики: у forged указатели "", у trusted сохранены.
	forged := append(append([]Diagnostic(nil), ve.Diagnostics...), Diagnostic{Code: "type", SchemaPointer: "/properties/token", InstancePointer: "/token"})
	appended := &ValidationError{Diagnostics: forged}
	appended.trusted = ve.trusted
	fb := appended.Feedback(1, 1)
	if !strings.Contains(fb, "/properties/token") {
		t.Fatalf("trusted указатели потеряны: %s", fb)
	}
	var obj struct {
		Diagnostics []struct {
			SchemaPointer string `json:"schema_pointer"`
			Reason        string `json:"reason"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(fb), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj.Diagnostics) != len(forged) {
		t.Fatalf("количество диагностик: %d != %d", len(obj.Diagnostics), len(forged))
	}
	last := obj.Diagnostics[len(obj.Diagnostics)-1]
	if last.SchemaPointer != "" {
		t.Fatalf("forged указатель прошёл: %+v", last)
	}
	// б) мутация элемента trusted: указатели этого индекса "".
	mut := &ValidationError{Diagnostics: append([]Diagnostic(nil), ve.Diagnostics...)}
	mut.trusted = ve.trusted
	mut.Diagnostics[0].SchemaPointer = "/evil"
	fb = mut.Feedback(1, 1)
	if strings.Contains(fb, "/evil") {
		t.Fatalf("мутированный указатель прошёл: %s", fb)
	}
	// в) перестановка элементов: элементы теряют указатели.
	reordered := &ValidationError{Diagnostics: append([]Diagnostic(nil), ve.Diagnostics...)}
	reordered.trusted = ve.trusted
	if len(reordered.Diagnostics) > 1 {
		reordered.Diagnostics[0], reordered.Diagnostics[len(reordered.Diagnostics)-1] = reordered.Diagnostics[len(reordered.Diagnostics)-1], reordered.Diagnostics[0]
		fb = reordered.Feedback(1, 1)
		var obj2 struct {
			Diagnostics []struct {
				SchemaPointer string `json:"schema_pointer"`
			} `json:"diagnostics"`
		}
		if err := json.Unmarshal([]byte(fb), &obj2); err != nil {
			t.Fatal(err)
		}
		for _, d := range obj2.Diagnostics {
			if d.SchemaPointer != "" {
				t.Fatalf("переставленный элемент сохранил указатель: %s", fb)
			}
		}
	}
}

func TestDiagnosticsAdditionalPropertiesExactScope(t *testing.T) {
	schema := `{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":{"required":["z"]}}`
	c := diagCompile(t, schema)
	// extras нет: только type на x, никаких required от ap.
	ve := c.Validate(`{"x":"bad"}`).(*ValidationError)
	if len(ve.Diagnostics) != 1 || ve.Diagnostics[0].Code != "type" {
		t.Fatalf("ожидался ровно один type: %+v", ve.Diagnostics)
	}
	for _, d := range ve.Diagnostics {
		if strings.Contains(d.Reason, "z") {
			t.Fatalf("required из ap просочился без extras: %+v", ve.Diagnostics)
		}
	}
	// extra есть: required z от ap-схемы, instance path родителя (ключ redacted).
	ve = c.Validate(`{"x":1,"extra":{}}`).(*ValidationError)
	if len(ve.Diagnostics) != 1 {
		t.Fatalf("ожидалась одна диагностика: %+v", ve.Diagnostics)
	}
	d := ve.Diagnostics[0]
	if d.Code != "required" || d.SchemaPointer != "/additionalProperties/required" || d.InstancePointer != "" {
		t.Fatalf("неожиданная диагностика: %+v", d)
	}
}

func TestDiagnosticsRequiredThroughLocalRef(t *testing.T) {
	schema := `{"$defs":{"t":{"type":"object","required":["z"]}},"properties":{"o":{"$ref":"#/$defs/t"}}}`
	c := diagCompile(t, schema)
	ve := c.Validate(`{"o":{}}`).(*ValidationError)
	if len(ve.Diagnostics) != 1 {
		t.Fatalf("ожидалась одна диагностика: %+v", ve.Diagnostics)
	}
	d := ve.Diagnostics[0]
	if d.Code != "required" || d.SchemaPointer != "/$defs/t/required" || d.InstancePointer != "/o" {
		t.Fatalf("неожиданная диагностика: %+v", d)
	}
}

func TestDiagnosticsCapEight(t *testing.T) {
	schema := `{"type":"object","required":["a","b","c","d","e","f","g","h"],"properties":{"bad":{"type":"number"}}}`
	c := diagCompile(t, schema)
	ve := c.Validate(`{"bad":"wrong type"}`).(*ValidationError)
	if len(ve.Diagnostics) != 8 {
		t.Fatalf("ожидалось ровно 8: %d", len(ve.Diagnostics))
	}
	fb := ve.Feedback(1, 1)
	var obj map[string]any
	if json.Unmarshal([]byte(fb), &obj) != nil {
		t.Fatalf("невалидный JSON: %s", fb)
	}
	diags, ok := obj["diagnostics"].([]any)
	if !ok || len(diags) > 8 {
		t.Fatalf("feedback diagnostics > 8: %s", fb)
	}
}
