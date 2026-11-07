package agentresult

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func regressionCompile(t *testing.T, pattern string) *Compiled {
	t.Helper()
	c, err := Compile(Spec{Type: "regex", Pattern: ptrString(pattern)})
	if err != nil {
		t.Fatalf("Compile(%q): %v", pattern, err)
	}
	return c
}

func regressionSchema(t *testing.T, schema string) *Compiled {
	t.Helper()
	c, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
	if err != nil {
		t.Fatalf("Compile schema: %v", err)
	}
	return c
}

func TestContractRegressionRegexFullMatchAndAdmission(t *testing.T) {
	cases := []struct {
		pattern, candidate string
		want               bool
	}{
		{"", "", true},
		{"", "x", false},
		{"x", "x", true},
		{"x", "prefixx", false},
		{"x", "xsuffix", false},
		{"x", "x\n", false},
		{"x", " x", false},
		{"x", "x ", false},
		{"(?s).*", "a\nb", true},
		{"(?m)^x$", "x", true},
		{"(?m)^x$", "prefixx", false},
		{"(?m)^x$", "x\ny", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q/%q", tc.pattern, tc.candidate), func(t *testing.T) {
			err := regressionCompile(t, tc.pattern).Validate(tc.candidate)
			if (err == nil) != tc.want {
				t.Fatalf("Validate=%v, want accepted %v", err, tc.want)
			}
		})
	}
	if err := regressionCompile(t, "(?s).*").Validate(strings.Repeat("x", MaxCandidateBytes)); err != nil {
		t.Fatalf("exact candidate limit: %v", err)
	}
	var tooLarge *Error
	if err := regressionCompile(t, "(?s).*").Validate(strings.Repeat("x", MaxCandidateBytes+1)); !errors.As(err, &tooLarge) || tooLarge.Code != CodeTooLarge {
		t.Fatalf("over limit: %v", err)
	}
	for n, want := range map[int]bool{MaxPatternBytes: true, MaxPatternBytes + 1: false} {
		_, err := Compile(Spec{Type: "regex", Pattern: ptrString(strings.Repeat("x", n))})
		if (err == nil) != want {
			t.Fatalf("pattern length %d: %v", n, err)
		}
	}
}

func TestContractRegressionStrictSpecAndRoundTrip(t *testing.T) {
	bad := []string{`{"type":"regex","type":"regex","pattern":"x"}`, `{"type":"json_schema","schema":{"type":"object","type":"string"}}`, `{"type":"regex","pattern":"x"} {}`, `{"type":"regex","pattern":"x"} trailing`, "```json\n{\"type\":\"regex\",\"pattern\":\"x\"}\n```", `null`, `{"type":"regex","pattern":"x","extra":1}`, `{"type":"regex","pattern":"x","schema":{}}`, `{"type":"json_schema","schema":{},"pattern":"x"}`, `{"type":"regex","pattern":"x","max_repairs":null}`, `{"type":"regex","pattern":"x","max_repairs":-1}`, `{"type":"regex","pattern":"x","max_repairs":6}`, `{"type":"regex","pattern":"x","max_repairs":true}`, `{"type":"regex","pattern":"x","max_repairs":"3"}`, `{"type":"regex","pattern":"x","max_repairs":1.5}`}
	for _, raw := range bad {
		if _, err := DecodeSpec([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for i := 0; i <= 5; i++ {
		raw := fmt.Sprintf(`{"type":"regex","pattern":"x","max_repairs":%d}`, i)
		s, err := DecodeSpec([]byte(raw))
		if err != nil || s.EffectiveMaxRepairs() != i {
			t.Fatalf("max_repairs %d: %+v %v", i, s, err)
		}
	}
	s, err := DecodeSpec([]byte(`{"type":"regex","pattern":"","max_repairs":0}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var round Spec
	if err = json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	if round.Pattern == nil || *round.Pattern != "" || round.MaxRepairs == nil || *round.MaxRepairs != 0 {
		t.Fatalf("round trip lost zero/empty: %#v", round)
	}
	if got, err := DecodeSpec([]byte(`{"type":"regex","pattern":"x"}`)); err != nil || got.EffectiveMaxRepairs() != DefaultMaxRepairs {
		t.Fatalf("default: %+v %v", got, err)
	}
}

func TestContractRegressionJSONCandidatesAndUnicode(t *testing.T) {
	c := regressionSchema(t, `{}`)
	for _, v := range []string{`{}`, `"тест"`, `null`, `[]`, `0.1`, `  { "x" : 1 }  `, `"\uD83D\uDE00"`} {
		if err := c.Validate(v); err != nil {
			t.Errorf("valid JSON %s: %v", v, err)
		}
	}
	for _, v := range []string{string([]byte{0xff}), `"` + `\uD800` + `"`, `{"x":{"a":1,"a":2}}`, "```", `{} trailing`, `{} {}`, "{}\n---\n{}"} {
		if err := c.Validate(v); err == nil {
			t.Errorf("accepted malformed candidate %q", v)
		}
	}
	emoji := regressionSchema(t, `{"type":"string","minLength":1,"maxLength":1}`)
	for _, v := range []string{`"😀"`, `"\uD83D\uDE00"`} {
		if err := emoji.Validate(v); err != nil {
			t.Errorf("emoji: %v", err)
		}
	}
	for _, v := range []string{`""`, `"ab"`} {
		if err := emoji.Validate(v); err == nil {
			t.Errorf("accepted %s", v)
		}
	}
	for _, schema := range []string{`{"format":"x"}`, `{"default":1}`} {
		if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)}); err == nil {
			t.Errorf("accepted unsupported keyword: %s", schema)
		}
	}
	properties := regressionSchema(t, `{"properties":{"format":{"type":"string"},"default":{"type":"number"}}}`)
	if err := properties.Validate(`{"format":"x","default":1}`); err != nil {
		t.Fatal(err)
	}
	if err := properties.Validate(`{"format":1}`); err == nil {
		t.Fatal("property type not enforced")
	}
}

func TestContractRegressionSchemaRefs(t *testing.T) {
	c := regressionSchema(t, `{"$defs":{"n":{"type":"number","minimum":1,"maximum":3}},"$ref":"#/$defs/n","minimum":2}`)
	for _, v := range []string{"2", "2.5"} {
		if err := c.Validate(v); err != nil {
			t.Errorf("sibling ref %s: %v", v, err)
		}
	}
	for _, v := range []string{"1", "4"} {
		if err := c.Validate(v); err == nil {
			t.Errorf("accepted sibling-ref bound %s", v)
		}
	}
	for _, key := range []string{"a/b", "a~b", "a b"} {
		schema := fmt.Sprintf(`{"$defs":{%q:{"type":"string"}},"$ref":"#/$defs/%s"}`, key, strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"), " ", "%20"))
		c := regressionSchema(t, schema)
		if err := c.Validate(`"ok"`); err != nil {
			t.Errorf("escaped ref %q: %v", key, err)
		}
		if err := c.Validate(`1`); err == nil {
			t.Errorf("ref %q accepted wrong type", key)
		}
	}
	for _, schema := range []string{`{"$ref":"https://example.com/schema"}`, `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`, `{"properties":{"x":{"type":"string"}},"$ref":"#/properties"}`, `{"$defs":{"x":{"const":{"password":"secret"}}},"$ref":"#/$defs/x/const"}`} {
		if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)}); err == nil {
			t.Errorf("accepted unsafe ref schema %s", schema)
		}
	}
}

func TestContractRegressionSchemaLimits(t *testing.T) {
	depth := `{}`
	for i := 0; i < 31; i++ {
		depth = `{"items":` + depth + `}`
	}
	if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(depth)}); err != nil {
		t.Fatalf("depth 32: %v", err)
	}
	depth = `{"items":` + depth + `}`
	if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(depth)}); err == nil {
		t.Fatal("accepted depth 33")
	}
	props := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"properties":{`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"p%d":{}`, i)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	for n, want := range map[int]bool{2047: true, 2048: false} {
		raw := props(n)
		if len(raw) >= MaxSchemaBytes {
			t.Fatalf("node fixture too large: %d", len(raw))
		}
		_, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(raw)})
		if (err == nil) != want {
			t.Errorf("nodes root+%d: %v", n, err)
		}
	}
	for n, want := range map[int]bool{256: true, 257: false} {
		vals := make([]string, n)
		for i := range vals {
			vals[i] = fmt.Sprintf("%q", fmt.Sprintf("v%d", i))
		}
		raw := `{"enum":[` + strings.Join(vals, ",") + `]}`
		_, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(raw)})
		if (err == nil) != want {
			t.Errorf("enum %d: %v", n, err)
		}
	}
	refTree := `{"type":"string"}`
	for i := 0; i < 20; i++ {
		refTree = `{"items":` + refTree + `}`
	}
	var defs strings.Builder
	defs.WriteString(`{"$defs":{"tree":` + refTree + `},"properties":{`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			defs.WriteByte(',')
		}
		fmt.Fprintf(&defs, `"p%d":{"$ref":"#/$defs/tree"}`, i)
	}
	defs.WriteString(`}}`)
	if strings.Count(defs.String(), `"$ref"`) > 2048 {
		t.Fatal("raw-node fixture invalid")
	}
	if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(defs.String())}); err == nil {
		t.Fatal("accepted expanded-node overflow")
	}
	for _, n := range []int{MaxSchemaBytes, MaxSchemaBytes + 1} {
		schema := `{"description":"` + strings.Repeat("x", n-len(`{"description":""}`)) + `"}`
		_, compileErr := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)})
		raw := []byte(`{"type":"json_schema","schema":` + schema + `}`)
		_, decodeErr := DecodeSpec(raw)
		want := n == MaxSchemaBytes
		if (compileErr == nil) != want || (decodeErr == nil) != want {
			t.Errorf("schema bytes %d compile=%v decode=%v", n, compileErr, decodeErr)
		}
	}
}

func TestContractRegressionDiagnosticsAndValidation(t *testing.T) {
	c := regressionSchema(t, `{"type":"object","required":["token"],"properties":{"token":{"type":"string"}},"additionalProperties":false}`)
	for _, candidate := range []string{`{`, `{"token":1}`, `{"token":"ok","secret-key":"secret"}`} {
		err := c.Validate(candidate)
		var ce *Error
		if !errors.As(err, &ce) || ce.Code != CodeMismatch {
			t.Errorf("error class for %q: %v", candidate, err)
			continue
		}
		fb := ce.Feedback(1, 2)
		var obj map[string]any
		if len(fb) > 4096 || json.Unmarshal([]byte(fb), &obj) != nil || strings.Contains(fb, "secret") || strings.Contains(fb, "token") {
			t.Errorf("unsafe feedback: %s", fb)
		}
	}
	for _, tc := range []struct {
		schema, candidate string
		ok                bool
	}{
		{`{"type":"array","uniqueItems":true}`, `[1,1]`, false}, {`{"type":"array","items":{"type":"integer"}}`, `[1,"x"]`, false}, {`{"enum":[0.1,2]}`, `0.1`, true}, {`{"enum":[0.1,2]}`, `0.2`, false}, {`{"const":0.1}`, `0.1`, true}, {`{"const":0.1}`, `0.10000000000000001`, false}, {`{"type":"number","minimum":-2,"maximum":0}`, `-1`, true}, {`{"type":"number","minimum":-2}`, `-3`, false}, {`{}`, `9007199254740991`, true}, {`{}`, `9007199254740992`, false}, {`{}`, `0.10000000000000001`, false}, {`{}`, `1e400`, false}, {`{}`, `1e-4000`, false}, {`{"type":"integer"}`, `1e2`, true}, {`{"minItems":1.0}`, `[1]`, true}, {`{"minItems":1e0}`, `[1]`, true}, {`{"enum":[0.1]}`, `0.1`, true}, {`{"const":0.1}`, `0.1`, true},
	} {
		cc := regressionSchema(t, tc.schema)
		err := cc.Validate(tc.candidate)
		if (err == nil) != tc.ok {
			t.Errorf("schema %s candidate %s: %v", tc.schema, tc.candidate, err)
		}
	}
	for _, schema := range []string{`{"minimum":9007199254740992}`, `{"enum":[9007199254740992]}`, `{"const":9007199254740992}`} {
		if _, err := Compile(Spec{Type: "json_schema", Schema: json.RawMessage(schema)}); err == nil {
			t.Errorf("accepted invalid number schema: %s", schema)
		}
	}
}

func TestContractRegressionSchemaDepthAndBounds(t *testing.T) {
	for _, tc := range []struct {
		schema, candidate string
		ok                bool
	}{
		{`{"type":"object","required":["a"]}`, `{}`, false}, {`{"type":"object","additionalProperties":false}`, `{"a":1}`, false}, {`{"type":"array","uniqueItems":true}`, `[1,2]`, true}, {`{"type":"array","items":{"type":"integer"}}`, `[1,2]`, true}, {`{"type":"string","minLength":2,"maxLength":3}`, `"ab"`, true}, {`{"type":"string","minLength":2}`, `"a"`, false}, {`{"type":"array","minItems":2,"maxItems":2}`, `[1]`, false},
	} {
		c := regressionSchema(t, tc.schema)
		if (c.Validate(tc.candidate) == nil) != tc.ok {
			t.Errorf("schema assertion failed: %s vs %s", tc.schema, tc.candidate)
		}
	}
}
