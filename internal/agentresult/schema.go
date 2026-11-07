package agentresult

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/google/jsonschema-go/jsonschema"
)

func compileSchema(raw []byte) (interface{ Validate(any) error }, error) {
	tree, e := ktavcodec.ParseJSONLossless(raw)
	if e != nil {
		return nil, e
	}
	root, ok := tree.(map[string]any)
	if !ok {
		return nil, errors.New("schema must be object")
	}
	allowed := map[string]bool{}
	for _, k := range []string{"$schema", "$defs", "$ref", "title", "description", "type", "properties", "required", "additionalProperties", "items", "minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties", "minLength", "maxLength", "pattern", "enum", "const", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		allowed[k] = true
	}
	registry, e := collectSchemaRefs(root)
	if e != nil {
		return nil, e
	}
	active := map[string]bool{}
	nodes := 0
	var walk func(any, string, int) error
	walk = func(v any, pointer string, depth int) error {
		if depth > 32 {
			return errors.New("schema depth")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("schema object")
		}
		if active[pointer] {
			return errors.New("schema cycle")
		}
		active[pointer] = true
		defer delete(active, pointer)
		nodes++
		if nodes > 2048 {
			return errors.New("schema nodes")
		}
		for _, k := range sortedKeys(m) {
			x := m[k]
			if !allowed[k] {
				return errors.New("unsupported keyword")
			}
			switch k {
			case "$schema":
				if x != "https://json-schema.org/draft/2020-12/schema" {
					return errors.New("dialect")
				}
			case "title", "description":
				if _, ok := x.(string); !ok {
					return errors.New("metadata")
				}
			case "type":
				valid := func(s string) bool {
					switch s {
					case "object", "array", "string", "number", "integer", "boolean", "null":
						return true
					}
					return false
				}
				if s, ok := x.(string); ok {
					if !valid(s) {
						return errors.New("type")
					}
				} else {
					a, ok := x.([]any)
					if !ok || len(a) == 0 {
						return errors.New("type")
					}
					seen := map[string]bool{}
					for _, v := range a {
						s, ok := v.(string)
						if !ok || !valid(s) || seen[s] {
							return errors.New("type")
						}
						seen[s] = true
					}
				}
			case "required":
				a, ok := x.([]any)
				if !ok {
					return errors.New("required")
				}
				seen := map[string]bool{}
				for _, v := range a {
					s, ok := v.(string)
					if !ok || seen[s] {
						return errors.New("required")
					}
					seen[s] = true
				}
			case "enum":
				a, ok := x.([]any)
				if !ok || len(a) == 0 || len(a) > 256 {
					return errors.New("enum")
				}
				seen := map[string]bool{}
				for _, v := range a {
					converted, err := numbersForValidator(v)
					if err != nil {
						return err
					}
					b, e := json.Marshal(converted)
					if e != nil {
						return e
					}
					if seen[string(b)] {
						return errors.New("enum")
					}
					seen[string(b)] = true
				}
			case "const":
				if _, e := numbersForValidator(x); e != nil {
					return e
				}
			case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum":
				n, ok := x.(json.Number)
				if !ok {
					return errors.New("number")
				}
				if _, e := numberFloat(n); e != nil {
					return e
				}
			case "minItems", "maxItems", "minProperties", "maxProperties", "minLength", "maxLength":
				n, ok := x.(json.Number)
				if !ok {
					return errors.New("integer")
				}
				f, e := numberFloat(n)
				if e != nil || f < 0 || math.Trunc(f) != f || f > float64(int(^uint(0)>>1)) {
					return errors.New("integer")
				}
			case "pattern":
				s, ok := x.(string)
				if !ok || len(s) > 4096 {
					return errors.New("pattern")
				}
				if _, e := regexp.Compile(s); e != nil {
					return e
				}
			case "uniqueItems":
				if _, ok := x.(bool); !ok {
					return errors.New("boolean")
				}
			case "items", "additionalProperties":
				if b, ok := x.(bool); ok && k == "additionalProperties" {
					_ = b
				} else if e := walk(x, schemaChildPointer(pointer, k), depth+1); e != nil {
					return e
				}
			case "properties", "$defs":
				children, ok := x.(map[string]any)
				if !ok {
					return errors.New("map")
				}
				for _, childKey := range sortedKeys(children) {
					if e := walk(children[childKey], schemaChildPointer(schemaChildPointer(pointer, k), childKey), depth+1); e != nil {
						return e
					}
				}
			case "$ref":
				r, ok := x.(string)
				if !ok {
					return errors.New("ref")
				}
				target, targetPointer, ok := resolveSchemaRef(registry, r)
				if !ok {
					return errors.New("ref")
				}
				if e := walk(target, targetPointer, depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if e = walk(root, "", 1); e != nil {
		return nil, e
	}
	converted, e := numbersForValidator(tree)
	if e != nil {
		return nil, e
	}
	normalized, e := json.Marshal(converted)
	if e != nil {
		return nil, e
	}
	var s jsonschema.Schema
	if e = json.Unmarshal(normalized, &s); e != nil {
		return nil, e
	}
	return s.Resolve(&jsonschema.ResolveOptions{})
}

func validateCompiled(c *Compiled, v any) error {
	converted, e := numbersForValidator(v)
	if e != nil {
		return &ValidationError{[]Diagnostic{{Code: "number_not_representable", Reason: "number is not exactly representable"}}}
	}
	if c.validator.Validate(converted) != nil {
		return &ValidationError{[]Diagnostic{{Code: "schema_mismatch", Reason: "candidate does not satisfy schema"}}}
	}
	return nil
}
