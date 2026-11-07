package agentresult

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

const (
	DefaultMaxRepairs = 3
	MaxCandidateBytes = 1 << 20
	MaxSchemaBytes    = 64 << 10
	MaxPatternBytes   = 4096
)

type Spec struct {
	Type       string          `json:"type"`
	Pattern    *string         `json:"pattern,omitempty"`
	Schema     json.RawMessage `json:"schema,omitempty"`
	MaxRepairs *int            `json:"max_repairs,omitempty"`
}
type Compiled struct {
	typeName            string
	effectiveMaxRepairs int
	re                  *regexp.Regexp
	validator           interface{ Validate(any) error }
	schemaRoot          map[string]any
	schemaRegistry      map[string]map[string]any
}
type Diagnostic struct {
	Code            string `json:"code"`
	InstancePointer string `json:"instance_pointer"`
	SchemaPointer   string `json:"schema_pointer"`
	Reason          string `json:"reason"`
}
type ValidationError struct {
	Diagnostics []Diagnostic
	// trusted — снапшот диагностик, сформированных доверенным структурным
	// обходом; указатели попадают в Feedback только при точном совпадении.
	trusted []Diagnostic
}

func (e *ValidationError) Error() string { return string(CodeMismatch) }
func (s Spec) EffectiveMaxRepairs() int {
	if s.MaxRepairs == nil {
		return DefaultMaxRepairs
	}
	return *s.MaxRepairs
}

func (c *Compiled) EffectiveMaxRepairs() int {
	if c == nil {
		return 0
	}
	return c.effectiveMaxRepairs
}

func DecodeSpec(raw []byte) (Spec, error) {
	var s Spec
	t, e := ktavcodec.ParseJSONLossless(raw)
	if e != nil {
		return s, &Error{Code: CodeInvalid}
	}
	m, ok := t.(map[string]any)
	if !ok {
		return s, &Error{Code: CodeInvalid}
	}
	var original map[string]json.RawMessage
	if e = json.Unmarshal(raw, &original); e != nil {
		return s, &Error{Code: CodeInvalid}
	}
	for k := range m {
		if k != "type" && k != "pattern" && k != "schema" && k != "max_repairs" {
			return s, &Error{Code: CodeInvalid}
		}
	}
	v, ok := m["type"].(string)
	if !ok {
		return s, &Error{Code: CodeInvalid}
	}
	s.Type = v
	if n, ok := m["max_repairs"]; ok {
		num, ok := n.(json.Number)
		if !ok {
			return s, &Error{Code: CodeInvalid}
		}
		switch num.String() {
		case "0", "1", "2", "3", "4", "5":
			i := int(num.String()[0] - '0')
			s.MaxRepairs = &i
		default:
			return s, &Error{Code: CodeInvalid}
		}
	}
	switch v {
	case "regex":
		p, ok := m["pattern"].(string)
		if !ok || len(p) > MaxPatternBytes {
			return s, &Error{Code: CodeInvalid}
		}
		s.Pattern = &p
		if _, ok = m["schema"]; ok {
			return s, &Error{Code: CodeInvalid}
		}
	case "json_schema":
		if _, ok = m["pattern"]; ok {
			return s, &Error{Code: CodeInvalid}
		}
		if _, ok := m["schema"]; !ok {
			return s, &Error{Code: CodeInvalid}
		}
		b := original["schema"]
		if len(b) > MaxSchemaBytes {
			return s, &Error{Code: CodeInvalid}
		}
		s.Schema = append(json.RawMessage(nil), b...)
	default:
		return s, &Error{Code: CodeInvalid}
	}
	if _, e = Compile(s); e != nil {
		return Spec{}, &Error{Code: CodeInvalid}
	}
	return s, nil
}

func (s Spec) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": s.Type}
	if s.Pattern != nil {
		m["pattern"] = *s.Pattern
	}
	if len(s.Schema) > 0 {
		m["schema"] = json.RawMessage(s.Schema)
	}
	if s.MaxRepairs != nil {
		m["max_repairs"] = *s.MaxRepairs
	}
	return json.Marshal(m)
}

func (s *Spec) UnmarshalJSON(b []byte) error {
	v, e := DecodeSpec(b)
	if e == nil {
		*s = v
	}
	return e
}

func Compile(s Spec) (*Compiled, error) {
	max := s.EffectiveMaxRepairs()
	if max < 0 || max > 5 {
		return nil, &Error{Code: CodeInvalid}
	}
	c := &Compiled{typeName: s.Type, effectiveMaxRepairs: max}
	switch s.Type {
	case "regex":
		if s.Pattern == nil || !utf8.ValidString(*s.Pattern) || len(*s.Pattern) > MaxPatternBytes || len(s.Schema) > 0 {
			return nil, &Error{Code: CodeInvalid}
		}
		r, e := regexp.Compile("\\A(?:" + *s.Pattern + ")\\z")
		if e != nil {
			return nil, &Error{Code: CodeInvalid}
		}
		c.re = r
	case "json_schema":
		if s.Pattern != nil || len(s.Schema) == 0 || len(s.Schema) > MaxSchemaBytes || string(s.Schema) == "null" {
			return nil, &Error{Code: CodeInvalid}
		}
		v, root, registry, e := compileSchema(append([]byte(nil), s.Schema...))
		if e != nil {
			return nil, &Error{Code: CodeInvalid}
		}
		c.validator = v
		c.schemaRoot = root
		c.schemaRegistry = registry
	default:
		return nil, &Error{Code: CodeInvalid}
	}
	return c, nil
}

func (c *Compiled) Validate(candidate string) error {
	if len(candidate) > MaxCandidateBytes {
		return &Error{Code: CodeTooLarge}
	}
	if c == nil || c.typeName == "" {
		return &Error{Code: CodeInvalid}
	}
	if c.typeName == "regex" {
		if c.re == nil {
			return &Error{Code: CodeInvalid}
		}
		if !utf8.ValidString(candidate) {
			return &ValidationError{Diagnostics: []Diagnostic{{Code: "regex_mismatch", SchemaPointer: "", Reason: "неверный UTF-8 или формат"}}}
		}
		if !c.re.MatchString(candidate) {
			return &ValidationError{Diagnostics: []Diagnostic{{Code: "regex_mismatch", SchemaPointer: "", Reason: "не соответствует шаблону"}}}
		}
		return nil
	}
	if c.validator == nil {
		return &Error{Code: CodeInvalid}
	}
	v, e := ktavcodec.ParseJSONLossless([]byte(candidate))
	if e != nil {
		return &ValidationError{Diagnostics: []Diagnostic{{Code: "invalid_json", SchemaPointer: "", Reason: "некорректный JSON"}}}
	}
	return validateCompiled(c, v)
}

func validateCompiled(c *Compiled, v any) error {
	converted, e := numbersForValidator(v)
	if e != nil {
		return &ValidationError{Diagnostics: []Diagnostic{{Code: "number_not_representable", Reason: "number is not exactly representable"}}}
	}
	err := c.validator.Validate(converted)
	if err == nil {
		return nil
	}
	// Доверенный структурный обход выдаёт diagnostics только потому, что
	// библиотечный Validate вернул ошибку; текст ошибки не разбирается.
	diags := structuralDiagnostics(c.schemaRoot, c.schemaRegistry, converted)
	if len(diags) > maxRequiredDiagnostics {
		diags = diags[:maxRequiredDiagnostics]
	}
	if len(diags) == 0 {
		diags = []Diagnostic{{Code: "schema_mismatch", Reason: "candidate does not satisfy schema"}}
	}
	trusted := make([]Diagnostic, len(diags))
	copy(trusted, diags)
	return &ValidationError{Diagnostics: diags, trusted: trusted}
}
