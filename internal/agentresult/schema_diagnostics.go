package agentresult

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// fixedDiagnosticReason возвращает фиксированную английскую фразу для кода.
// Никакого текста из ошибки библиотеки или кандидата здесь нет.
func fixedDiagnosticReason(code string) (string, bool) {
	switch code {
	case "type":
		return "value type does not match schema", true
	case "enum":
		return "value is not one of the allowed values", true
	case "const":
		return "value does not match the constant", true
	case "minimum":
		return "value is less than minimum", true
	case "maximum":
		return "value is greater than maximum", true
	case "exclusiveMinimum":
		return "value is not greater than exclusiveMinimum", true
	case "exclusiveMaximum":
		return "value is not less than exclusiveMaximum", true
	case "minLength":
		return "value is shorter than minLength", true
	case "maxLength":
		return "value is longer than maxLength", true
	case "pattern":
		return "value does not match pattern", true
	case "minItems":
		return "array has fewer items than minItems", true
	case "maxItems":
		return "array has more items than maxItems", true
	case "uniqueItems":
		return "array items are not unique", true
	case "minProperties":
		return "object has fewer properties than minProperties", true
	case "maxProperties":
		return "object has more properties than maxProperties", true
	case "required":
		return "required property missing", true
	case "additionalProperties":
		return "additional properties are not allowed", true
	default:
		return "", false
	}
}

const (
	maxDiagnosticPointerBytes = 512
	maxRequiredDiagnostics    = 8
	maxRequiredDepth          = 32
)

// validStrictPointer проверяет указатель: `/`-префикс, valid UTF-8, без
// control-байтов, длина <=512, escape-последовательности только ~0 и ~1.
func validStrictPointer(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > maxDiagnosticPointerBytes || !utf8.ValidString(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if !validStrictSegment(seg) {
			return false
		}
	}
	return true
}

// validStrictSegment проверяет один сегмент на строгие escape ~0/~1.
func validStrictSegment(seg string) bool {
	for i := 0; i < len(seg); i++ {
		if seg[i] != '~' {
			continue
		}
		if i+1 >= len(seg) || (seg[i+1] != '0' && seg[i+1] != '1') {
			return false
		}
		i++
	}
	return true
}

// strictUnescapeSegment разворачивает ~0/~1; при любых других ~X возвращает false.
func strictUnescapeSegment(seg string) (string, bool) {
	if !validStrictSegment(seg) {
		return "", false
	}
	r := strings.NewReplacer("~1", "/", "~0", "~")
	return r.Replace(seg), true
}

func escapePointerSegment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// sanitizeStrictPointer валидирует указатель strict-правилами и повторно
// экранирует сегменты (unescape строго, потом escape). Невалидный -> "".
func sanitizeStrictPointer(p string) string {
	if p == "" {
		return ""
	}
	if !validStrictPointer(p) {
		return ""
	}
	var b strings.Builder
	for _, seg := range strings.Split(p[1:], "/") {
		u, ok := strictUnescapeSegment(seg)
		if !ok {
			return ""
		}
		b.WriteString("/")
		b.WriteString(escapePointerSegment(u))
	}
	return b.String()
}

// safeRequiredName допускает имя в reason только если оно valid UTF-8,
// без control-байтов и не длиннее 64 рун.
func safeRequiredName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	return true
}

// structuralDiagnostics — доверенный обход кандидата по скомпилированной
// и клэмпнутой схеме (tree + registry). Вызывается только когда библиотечный
// Validate вернул ошибку; текст ошибки библиотеки не разбирается никогда.
func structuralDiagnostics(root map[string]any, registry map[string]map[string]any, candidate any) []Diagnostic {
	if root == nil {
		return nil
	}
	w := &diagWalker{registry: registry, active: map[string]bool{}}
	w.walk(root, "", candidate, "", 1)
	if len(w.out) > maxRequiredDiagnostics {
		w.out = w.out[:maxRequiredDiagnostics]
	}
	return w.out
}

type diagWalker struct {
	registry map[string]map[string]any
	out      []Diagnostic
	active   map[string]bool
}

// Потерянный путь остаётся потерянным при дальнейших спусках; иначе
// потомок длинного ключа превратился бы в несуществующее свойство корня.
const unavailableDiagnosticPath = "\x00"

func diagPointer(parent, key string) string {
	if parent == unavailableDiagnosticPath {
		return parent
	}
	out := schemaChildPointer(parent, key)
	if len(out) > maxDiagnosticPointerBytes {
		return unavailableDiagnosticPath
	}
	return out
}

func diagInstancePath(parent, seg string) string {
	if parent == unavailableDiagnosticPath {
		return parent
	}
	out := parent + "/" + escapePointerSegment(seg)
	if len(out) > maxDiagnosticPointerBytes {
		return unavailableDiagnosticPath
	}
	return out
}

// emit добавляет диагностику; невалидные по strict-правилам указатели
// отбрасываются до пустых строк.
func (w *diagWalker) emit(keyword, schemaPointer, instancePointer string) {
	if len(w.out) >= maxRequiredDiagnostics {
		return
	}
	reason, ok := fixedDiagnosticReason(keyword)
	if !ok {
		return
	}
	if schemaPointer != "" && !validStrictPointer(schemaPointer) {
		schemaPointer = ""
	}
	if instancePointer != "" && !validStrictPointer(instancePointer) {
		instancePointer = ""
	}
	w.out = append(w.out, Diagnostic{
		Code:            keyword,
		InstancePointer: instancePointer,
		SchemaPointer:   schemaPointer,
		Reason:          reason,
	})
}

// candidateKind определяет тип значения converted-дерева кандидата.
func candidateKind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if !math.IsInf(x, 0) && x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return ""
	}
}

func matchesSchemaType(t, kind string) bool {
	return t == kind || (t == "number" && kind == "integer")
}

// schemaNumber читает число из узла схемы (json.Number после ParseJSONLossless).
func schemaNumber(schema map[string]any, key string) (float64, bool) {
	n, ok := schema[key].(json.Number)
	if !ok {
		return 0, false
	}
	f, e := numberFloat(n)
	if e != nil {
		return 0, false
	}
	return f, true
}

func (w *diagWalker) walk(schema map[string]any, nodePointer string, v any, instancePath string, depth int) {
	if schema == nil || depth > maxRequiredDepth || len(w.out) >= maxRequiredDiagnostics {
		return
	}
	// $ref: указатель схемы для дальнейших диагностик — указатель TARGET
	// (trusted), защита от циклов через active-набор.
	if r, ok := schema["$ref"].(string); ok {
		if target, targetPointer, ok := resolveSchemaRef(w.registry, r); ok && !w.active[targetPointer] {
			w.active[targetPointer] = true
			w.walk(target, targetPointer, v, instancePath, depth+1)
			delete(w.active, targetPointer)
		}
	}
	// type: строка или массив строк; integer засчитывается для "number".
	if t, ok := schema["type"]; ok {
		kind := candidateKind(v)
		matched := false
		switch tv := t.(type) {
		case string:
			matched = matchesSchemaType(tv, kind)
		case []any:
			for _, x := range tv {
				s, ok := x.(string)
				if ok && matchesSchemaType(s, kind) {
					matched = true
					break
				}
			}
		}
		if !matched {
			w.emit("type", nodePointer, instancePath)
			return
		}
	}
	// enum/const: элементы схемы уже validated; сравнение с converted-деревом.
	if a, ok := schema["enum"].([]any); ok {
		found := false
		for _, el := range a {
			conv, e := numbersForValidator(el)
			if e != nil {
				continue
			}
			if reflect.DeepEqual(conv, v) {
				found = true
				break
			}
		}
		if !found {
			w.emit("enum", nodePointer, instancePath)
			return
		}
	}
	if c, ok := schema["const"]; ok {
		conv, e := numbersForValidator(c)
		if e == nil && !reflect.DeepEqual(conv, v) {
			w.emit("const", nodePointer, instancePath)
			return
		}
	}
	// Числовые границы — только для float64 кандидата.
	if f, isNum := v.(float64); isNum {
		if min, ok := schemaNumber(schema, "minimum"); ok && f < min {
			w.emit("minimum", nodePointer, instancePath)
			return
		}
		if max, ok := schemaNumber(schema, "maximum"); ok && f > max {
			w.emit("maximum", nodePointer, instancePath)
			return
		}
		if min, ok := schemaNumber(schema, "exclusiveMinimum"); ok && f <= min {
			w.emit("exclusiveMinimum", nodePointer, instancePath)
			return
		}
		if max, ok := schemaNumber(schema, "exclusiveMaximum"); ok && f >= max {
			w.emit("exclusiveMaximum", nodePointer, instancePath)
			return
		}
	}
	// Строковые ключевые слова.
	if s, isStr := v.(string); isStr {
		if n, ok := schemaNumber(schema, "minLength"); ok && utf8.RuneCountInString(s) < int(n) {
			w.emit("minLength", nodePointer, instancePath)
			return
		}
		if n, ok := schemaNumber(schema, "maxLength"); ok && utf8.RuneCountInString(s) > int(n) {
			w.emit("maxLength", nodePointer, instancePath)
			return
		}
		if p, ok := schema["pattern"].(string); ok {
			if re, e := regexp.Compile(p); e == nil && !re.MatchString(s) {
				w.emit("pattern", nodePointer, instancePath)
				return
			}
		}
	}
	// Массивы: minItems/maxItems/uniqueItems, затем рекурсия в items.
	if arr, isArr := v.([]any); isArr {
		if n, ok := schemaNumber(schema, "minItems"); ok && len(arr) < int(n) {
			w.emit("minItems", nodePointer, instancePath)
			return
		}
		if n, ok := schemaNumber(schema, "maxItems"); ok && len(arr) > int(n) {
			w.emit("maxItems", nodePointer, instancePath)
			return
		}
		if u, ok := schema["uniqueItems"].(bool); ok && u && !arrayItemsUnique(arr) {
			w.emit("uniqueItems", nodePointer, instancePath)
			return
		}
		if items, ok := schema["items"].(map[string]any); ok {
			// Индекс массива — структурный, разрешён в instance path.
			for i, item := range arr {
				idx := diagInstancePath(instancePath, strconv.Itoa(i))
				if idx != instancePath {
					w.walk(items, diagPointer(nodePointer, "items"), item, idx, depth+1)
				} else {
					w.walk(items, diagPointer(nodePointer, "items"), item, instancePath, depth+1)
				}
				if len(w.out) >= maxRequiredDiagnostics {
					return
				}
			}
		}
	}
	// Объекты: min/maxProperties, required, properties, additionalProperties.
	obj, isObj := v.(map[string]any)
	if !isObj {
		return
	}
	if n, ok := schemaNumber(schema, "minProperties"); ok && len(obj) < int(n) {
		w.emit("minProperties", nodePointer, instancePath)
		return
	}
	if n, ok := schemaNumber(schema, "maxProperties"); ok && len(obj) > int(n) {
		w.emit("maxProperties", nodePointer, instancePath)
		return
	}
	if req, ok := schema["required"].([]any); ok {
		reqPointer := diagPointer(nodePointer, "required")
		for _, r := range req {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := obj[name]; present {
				continue
			}
			if len(w.out) >= maxRequiredDiagnostics {
				return
			}
			reason := "required property missing"
			if safeRequiredName(name) {
				reason = "required property missing: " + name
			}
			w.emitReason("required", reqPointer, instancePath, reason)
		}
	}
	props, _ := schema["properties"].(map[string]any)
	if props != nil {
		for _, name := range sortedKeys(props) {
			child, present := obj[name]
			if !present {
				continue
			}
			sub, ok := props[name].(map[string]any)
			if !ok {
				continue
			}
			// Instance path расширяется только schema-known именем.
			w.walk(sub, diagPointer(diagPointer(nodePointer, "properties"), name), child, diagInstancePath(instancePath, name), depth+1)
			if len(w.out) >= maxRequiredDiagnostics {
				return
			}
		}
	}
	switch ap := schema["additionalProperties"].(type) {
	case bool:
		if ap {
			return
		}
		// Только реальные extras дают диагностику.
		for _, k := range sortedKeys(obj) {
			if _, known := props[k]; !known {
				w.emit("additionalProperties", nodePointer, instancePath)
				return
			}
		}
	case map[string]any:
		for _, k := range sortedKeys(obj) {
			if _, known := props[k]; known {
				continue
			}
			// Путь через скрытый ключ нельзя продолжать именами потомков.
			w.walk(ap, diagPointer(nodePointer, "additionalProperties"), obj[k], unavailableDiagnosticPath, depth+1)
			if len(w.out) >= maxRequiredDiagnostics {
				return
			}
		}
	}
}

// emitReason добавляет диагностику с уже безопасным reason.
func (w *diagWalker) emitReason(code, schemaPointer, instancePointer, reason string) {
	if len(w.out) >= maxRequiredDiagnostics {
		return
	}
	if schemaPointer != "" && !validStrictPointer(schemaPointer) {
		schemaPointer = ""
	}
	if instancePointer != "" && !validStrictPointer(instancePointer) {
		instancePointer = ""
	}
	w.out = append(w.out, Diagnostic{
		Code:            code,
		InstancePointer: instancePointer,
		SchemaPointer:   schemaPointer,
		Reason:          reason,
	})
}

// arrayItemsUnique проверяет уникальность элементов через marshal-нормализацию.
func arrayItemsUnique(arr []any) bool {
	seen := map[string]bool{}
	for _, el := range arr {
		b, e := json.Marshal(el)
		if e != nil {
			return false
		}
		if seen[string(b)] {
			return false
		}
		seen[string(b)] = true
	}
	return true
}
