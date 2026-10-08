package cahgen

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fixture loads the frozen cah 0.16.0 manifest; refresh never writes it.
func fixture(t *testing.T) Manifest {
	t.Helper()
	b, e := os.ReadFile("testdata/fixture/manifest-0.16.0.json")
	if e != nil {
		t.Fatal(e)
	}
	m, e := Decode(b)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

// Revert-check: CheckCommitted drift comparison in Run's three-file publication (SnapshotPath).
func TestSnapshot(t *testing.T) {
	if e := CheckCommitted(os.ReadFile, "../.."); e != nil {
		t.Fatal(e)
	}
}

// Revert-check: Transform/renderCmd/renderSpecs must retain measured catalog rules.
func TestFixtureCatalog(t *testing.T) {
	m := fixture(t)
	out, e := Transform(m, Overrides())
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Models) != 20 {
		t.Fatalf("fixture model count: got %d, want 20", len(out.Models))
	}
	if _, ok := out.Names["ul1"]; ok {
		t.Fatal("ul1 exposed")
	}
	if out.Names["hl"].Model != "claude-haiku-5-5" {
		t.Fatal("Claude must win")
	}
	for _, s := range out.Models {
		if strings.HasPrefix(s.Model, "claude-") && s.Context != 200000 && s.Context != 1000000 {
			t.Fatal(s)
		}
		if s.Model == "claude-opus-5" && (s.Arg != "claude-opus-5[1m]" || s.Slug != "cli-claude-opus-5-1m") {
			t.Fatal(s)
		}
		if s.Model == "claude-sonnet-4-6" && !reflect.DeepEqual(s.Efforts, []string{"low", "medium", "high", "max"}) {
			t.Fatal(s)
		}
		if s.Model == "gpt-6-sol" || s.Model == "gpt-6-astra" {
			if len(s.Efforts) != 6 || s.Efforts[5] != "ultra" {
				t.Fatal(s)
			}
		}
		if s.Model == "gpt-5.6-luna" && len(s.Efforts) != 5 {
			t.Fatal(s)
		}
		if strings.HasPrefix(s.Model, "gpt-5.6-") && s.Slug != "cli-codex-"+strings.TrimPrefix(s.Model, "gpt-5.6-") {
			t.Fatal(s)
		}
	}
	for _, w := range out.Warnings {
		if strings.Contains(w, "unmeasured") || strings.Contains(w, "stale") {
			t.Fatal(w)
		}
	}
	if len(out.Warnings) != 2 {
		t.Fatal(out.Warnings)
	}
	for _, n := range out.Notes {
		if strings.Contains(n, "unmeasured") || strings.Contains(n, "stale") {
			t.Fatal("dev-facing text in notes:", n)
		}
	}
	if !bytes.Contains(out.Cmd, []byte("var cahNotes = []string{")) {
		t.Fatal("cahNotes not emitted")
	}
}

// Revert-check: Transform name/slug/effort mapping for the frozen 0.16.0 catalog.
func TestFixtureNameMapping(t *testing.T) {
	out, e := Transform(fixture(t), Overrides())
	if e != nil {
		t.Fatal(e)
	}
	want := map[string][2]string{
		"ox": {"cli-claude-opus-5-5", "xhigh"}, "o1xx": {"cli-claude-opus-5-1m", "max"},
		"s2xx": {"cli-claude-sonnet-4-6", "max"}, "h1m": {"cli-claude-haiku-4-5", "medium"},
		"fx": {"cli-claude-fable-5-1", "xhigh"}, "ls": {"cli-codex-gpt-6-1-sol", "low"},
		"xxs1": {"cli-codex-gpt-6-sol", "max"}, "us": {"cli-codex-gpt-6-1-sol", "ultra"},
		"ut": {"cli-codex-terra", "ultra"}, "xa": {"cli-codex-gpt-6-astra", "xhigh"},
		"hl": {"cli-claude-haiku-5-5", "low"},
	}
	for code, w := range want {
		if n, ok := out.Names[code]; !ok || n.Slug != w[0] || n.Effort != w[1] {
			t.Fatalf("%s: got %+v, want %v", code, n, w)
		}
	}
	if _, ok := out.Names["ul1"]; ok {
		t.Fatal("ul1 present")
	}
}

// Revert-check: Transform sorting must ignore upstream array order.
func TestDeterminism(t *testing.T) {
	m := fixture(t)
	a, e := Transform(m, Overrides())
	if e != nil {
		t.Fatal(e)
	}
	for _, list := range [][]Entry{m.Claude, m.Codex} {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	b, e := Transform(m, Overrides())
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("order-dependent")
	}
}

// Revert-check: Decode/Transform must reject schema, identity and context drift.
func TestValidation(t *testing.T) {
	cases := map[string]func(*Manifest){
		"version": func(m *Manifest) { m.Version = "oops" }, "empty": func(m *Manifest) { m.Codex = nil },
		"name": func(m *Manifest) { m.Claude[0].Name = "../x" }, "model": func(m *Manifest) { m.Claude[0].Model = "claude-unknown-5" },
		"effort": func(m *Manifest) { m.Claude[0].Effort = "ultra" }, "display": func(m *Manifest) { m.Claude[0].Display = "Fable (10M) – low" },
		"negative": func(m *Manifest) { m.Codex[0].ContextWindow = -1 }, "conflict": func(m *Manifest) { m.Claude[0].ContextWindow = 200000 },
		"duplicate": func(m *Manifest) { m.Claude = append(m.Claude, m.Claude[0]) }, "pair": func(m *Manifest) { e := m.Claude[0]; e.Name = "f99l"; m.Claude = append(m.Claude, e) },
		"missing": func(m *Manifest) { m.Claude[0].Display = "" },
	}
	for n, mut := range cases {
		t.Run(n, func(t *testing.T) {
			m := fixture(t)
			mut(&m)
			if _, e := Transform(m, Overrides()); e == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, raw := range []string{`{"unknown":1}`, `{} {}`, `{"claude":[{"other":true}]}`, `{"codex":[{"contextWindow":1.5}]}`, `{"codex":[{"contextWindow":"100"}]}`} {
		if _, e := Decode([]byte(raw)); e == nil {
			t.Fatal(raw)
		}
	}
}

// Revert-check: Transform override precedence, stale warnings and per-model caps.
func TestOverrides(t *testing.T) {
	m := fixture(t)
	o := Overrides()
	v := o["gpt-6.1-sol"]
	v.Context = 12345
	o["gpt-6.1-sol"] = v
	for i := range m.Codex {
		if m.Codex[i].Model == "gpt-6.1-sol" {
			m.Codex[i].ContextWindow = 54321
		}
	}
	if _, err := Transform(m, o); err == nil || !strings.Contains(err.Error(), "conflicting measured context") {
		t.Fatal("explicit Codex context must agree with measurement", err)
	}
	v.Context = 54321
	o["gpt-6.1-sol"] = v
	o["unused"] = Override{}
	delete(o, "claude-fable-5-1")
	out, e := Transform(m, o)
	if e != nil {
		t.Fatal(e)
	}
	if out.Names["ls"].Context != 54321 {
		t.Fatal("field precedence")
	}
	if !strings.Contains(strings.Join(out.Warnings, "\n"), "stale override unused") || !strings.Contains(strings.Join(out.Warnings, "\n"), "default spec (unmeasured): claude-fable-5-1") {
		t.Fatal(out.Warnings)
	}
	for _, caps := range [][]string{{"invalid"}, {"high", "low"}, {"low", "low"}} {
		o = Overrides()
		v = o["gpt-6-sol"]
		v.EffortCaps = caps
		o["gpt-6-sol"] = v
		if _, e := Transform(fixture(t), o); e == nil {
			t.Fatal(caps)
		}
	}
	o = Overrides()
	v = o["claude-opus-5"]
	v.Slug = "cli-claude-fable-5"
	o["claude-opus-5"] = v
	if _, e := Transform(fixture(t), o); e == nil {
		t.Fatal("slug collision")
	}
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Decode(raw); e != nil {
		t.Fatal(e)
	}
}
