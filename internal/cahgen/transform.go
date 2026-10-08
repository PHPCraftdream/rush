// Package cahgen transforms cc-arch-hands catalogs without importing their consumers.
package cahgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"regexp"
	"sort"
	"strings"
)

type Entry struct {
	Name             string `json:"name"`
	Model            string `json:"model"`
	Effort           string `json:"effort"`
	Display          string `json:"display"`
	ContextWindow    int64  `json:"contextWindow,omitempty"`
	MaxContextWindow int64  `json:"maxContextWindow,omitempty"`
}

func (e *Entry) UnmarshalJSON(data []byte) error {
	type entry Entry
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"name", "model", "effort", "display"} {
		if v, ok := fields[key]; !ok || bytes.Equal(v, []byte("null")) {
			return fmt.Errorf("missing field %s", key)
		}
	}
	if v, ok := fields["contextWindow"]; ok {
		var n int64
		if err := json.Unmarshal(v, &n); err != nil || n <= 0 {
			return fmt.Errorf("contextWindow must be positive integer")
		}
	}
	if v, ok := fields["maxContextWindow"]; ok {
		var n, ctx int64
		if err := json.Unmarshal(v, &n); err != nil || n <= 0 {
			return fmt.Errorf("maxContextWindow must be positive integer")
		}
		if v, ok := fields["contextWindow"]; ok {
			if err := json.Unmarshal(v, &ctx); err != nil || n < ctx {
				return fmt.Errorf("maxContextWindow must be >= contextWindow")
			}
		}
	}
	var decoded entry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	*e = Entry(decoded)
	return nil
}

type Manifest struct {
	Version         string              `json:"version"`
	Claude          []Entry             `json:"claude"`
	Codex           []Entry             `json:"codex"`
	AcceptedEfforts map[string][]string `json:"acceptedEfforts,omitempty"`
}
type Override struct {
	Slug, Arg  string
	Context    int64
	Measured   bool
	EffortCaps []string
}

// Operator measurements 2026-10-08, claude 2.x: new Claude IDs use bare
// IDs (1M for 5.5/5.1; 200k for Haiku 4.5 and Sonnet 4.6/4.5).
// Existing Opus 4.6/4.7/4.8 and Fable 5 measurements are retained;
// Opus 5 and Sonnet 5 require [1m]. Codex caps are operator measured.
func Overrides() map[string]Override {
	m := map[string]Override{}
	for _, id := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-fable-5"} {
		m[id] = Override{Context: 1000000, Measured: true}
	}
	for _, id := range []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-sonnet-4-5"} {
		m[id] = Override{Context: 200000, Measured: true}
	}
	for _, id := range []string{"claude-opus-5", "claude-sonnet-5"} {
		m[id] = Override{Slug: "cli-" + id + "-1m", Arg: id + "[1m]", Context: 1000000, Measured: true}
	}
	for _, id := range []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-luna", "gpt-5.6-luna"} {
		caps := []string{"low", "medium", "high", "xhigh", "max"}
		if !strings.HasSuffix(id, "luna") {
			caps = append(caps, "ultra")
		}
		o := Override{Measured: true, EffortCaps: caps}
		if strings.HasPrefix(id, "gpt-5.6-") {
			o.Slug = "cli-codex-" + strings.TrimPrefix(id, "gpt-5.6-")
		}
		m[id] = o
	}
	return m
}

var (
	claudeNameRE = regexp.MustCompile(`^([oshf])([1-9][0-9]*)?(l|m|h|x|xx)$`)
	codexNameRE  = regexp.MustCompile(`^(l|m|h|x|xx|u)([stla])([1-9][0-9]*)?$`)
	effortTokens = map[string]string{"l": "low", "m": "medium", "h": "high", "x": "xhigh", "xx": "max", "u": "ultra"}
)

// validName checks provider-specific code order, family identity and effort.
func validName(e Entry, family int) bool {
	if family == 0 {
		code, model := claudeNameRE.FindStringSubmatch(e.Name), claudeRE.FindStringSubmatch(e.Model)
		return code != nil && model != nil && code[1] == model[1][:1] && effortTokens[code[3]] == e.Effort
	}
	code, model := codexNameRE.FindStringSubmatch(e.Name), codexRE.FindStringSubmatch(e.Model)
	return code != nil && model != nil && code[2] == model[1][:1] && effortTokens[code[1]] == e.Effort
}

type collisionRule struct {
	PriorFamily, IncomingFamily, PriorEffort, IncomingEffort string
}

// Collision permissions are explicit; unknown collisions fail closed.
var collisionRules = map[string]collisionRule{
	"hl": {"claude", "codex", "low", "high"},
}

func resolveCollision(code string, prior, incoming Name, rules map[string]collisionRule) (string, error) {
	rule, ok := rules[code]
	if !ok || !strings.HasPrefix(prior.Model, rule.PriorFamily+"-") ||
		!(rule.IncomingFamily == "codex" && strings.HasPrefix(incoming.Model, "gpt-") || strings.HasPrefix(incoming.Model, rule.IncomingFamily+"-")) ||
		prior.Effort != rule.PriorEffort || incoming.Effort != rule.IncomingEffort {
		return "", fmt.Errorf("unresolved cross-family collision %s", code)
	}
	return fmt.Sprintf("Claude wins %s collision; skipped Codex code; use local-cli/%s@%s", code, incoming.Slug, incoming.Effort), nil
}

var (
	versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$`)
	claudeRE  = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-[0-9]+(?:-[0-9]+)*$`)
	codexRE   = regexp.MustCompile(`^gpt-[0-9]+(?:\.[0-9]+)?-(sol|terra|luna|astra)$`)
	displayRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9 .]*) \((?:top, )?(1M|200k)\) – (low|medium|high|xhigh|max)$`)
	slugRE    = regexp.MustCompile(`^cli-[a-z0-9][a-z0-9.-]*$`)
	levels    = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
)

func rank(s string) int {
	for i, l := range levels {
		if s == l {
			return i
		}
	}
	return -1
}

func Decode(data []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, fmt.Errorf("trailing JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return m, err
	}
	if v, ok := fields["acceptedEfforts"]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return m, fmt.Errorf("acceptedEfforts must be a model registry")
	}
	if err := validateAcceptedEfforts(m, nil); err != nil {
		return m, err
	}
	return m, nil
}

type Name struct {
	Provider, Slug, Effort, Model, Display string
	Context                                int64
}
type Spec struct {
	Family, Slug, Model, Arg, Display string
	Context                           int64
	Efforts                           []string
}

// Collision records a resolved cross-family code clash; Claude is the Winner.
type Collision struct {
	Code          string
	Winner, Loser Name
}
type Output struct {
	Cmd, Specs     []byte
	Warnings, Info []string
	// Notes are user-facing (emitted as cahNotes); Warnings/Info are dev-facing.
	Notes      []string
	Names      map[string]Name
	Models     []Spec
	Collisions []Collision
}

// validWindows also protects callers constructing entries without JSON decoding.
func validWindows(e Entry) bool {
	return e.ContextWindow >= 0 && e.MaxContextWindow >= 0 &&
		(e.MaxContextWindow == 0 || e.ContextWindow == 0 || e.MaxContextWindow >= e.ContextWindow)
}

// Transform is pure. Overrides are explicit inputs; callers normally use Overrides().
func Transform(m Manifest, overrides map[string]Override) (Output, error) {
	out := Output{Names: map[string]Name{}}
	if !versionRE.MatchString(m.Version) || len(m.Claude) == 0 || len(m.Codex) == 0 {
		return out, fmt.Errorf("invalid version or empty arrays")
	}
	if err := validateAcceptedEfforts(m, overrides); err != nil {
		return out, err
	}
	models := map[string]Spec{}
	used := map[string]bool{}
	slugs := map[string]string{}
	for family, entries := range [][]Entry{m.Claude, m.Codex} {
		seen := map[string]bool{}
		pairs := map[string]bool{}
		for _, e := range entries {
			re := claudeRE
			fam := "claude"
			if family == 1 {
				re = codexRE
				fam = "codex"
			}
			if !validName(e, family) || !re.MatchString(e.Model) || rank(e.Effort) < 0 || e.Display == "" || strings.ContainsAny(e.Display, "\r\n\x00") || !validWindows(e) || seen[e.Name] || pairs[e.Model+"/"+e.Effort] {
				return out, fmt.Errorf("invalid or duplicate %s entry %q", fam, e.Name)
			}
			if family == 0 && e.Effort == "ultra" {
				return out, fmt.Errorf("invalid Claude effort")
			}
			seen[e.Name] = true
			pairs[e.Model+"/"+e.Effort] = true
			o, ok := overrides[e.Model]
			if ok {
				used[e.Model] = true
			}
			slug := "cli-" + e.Model
			if family == 1 {
				slug = "cli-codex-" + strings.ReplaceAll(e.Model, ".", "-")
			}
			arg := e.Model
			if o.Slug != "" {
				slug = o.Slug
			}
			if o.Arg != "" {
				arg = o.Arg
			}
			if !slugRE.MatchString(slug) || strings.ContainsAny(arg, "\r\n\x00") || o.Context < 0 {
				return out, fmt.Errorf("invalid override for %s", e.Model)
			}
			if prev, ok := slugs[slug]; ok && prev != e.Model {
				return out, fmt.Errorf("slug collision %s", slug)
			}
			slugs[slug] = e.Model
			ctx := e.ContextWindow
			var display string
			if family == 0 {
				match := displayRE.FindStringSubmatch(e.Display)
				if match == nil || match[3] != e.Effort {
					return out, fmt.Errorf("invalid Claude context annotation %q", e.Display)
				}
				annotated := int64(200000)
				if match[2] == "1M" {
					annotated = 1000000
				}
				if ctx != 0 && ctx != annotated {
					return out, fmt.Errorf("conflicting Claude context")
				}
				ctx = annotated
				display = strings.TrimSuffix(e.Display, " – "+e.Effort)
				if o.Context != 0 && o.Context != ctx {
					return out, fmt.Errorf("conflicting measured context %s", e.Model)
				}
			} else {
				if ctx != 0 && o.Context != 0 && ctx != o.Context {
					return out, fmt.Errorf("conflicting measured context %s", e.Model)
				}
				if ctx == 0 {
					ctx = o.Context
				}
				if ctx == 0 {
					ctx = 272000
				}
				// Codex display suffixes include "Extra High"; model identity is stable.
				display = strings.Split(e.Display, " - ")[0]
			}
			s, exists := models[e.Model]
			if !exists {
				s = Spec{Family: fam, Slug: slug, Model: e.Model, Arg: arg, Display: display, Context: ctx}
				if !o.Measured {
					out.Warnings = append(out.Warnings, "default spec (unmeasured): "+e.Model)
				}
				if family == 1 && e.ContextWindow == 0 && o.Context == 0 {
					out.Info = append(out.Info, "codex context fallback 272000 (cah has no contextWindow)")
				}
				for i, l := range o.EffortCaps {
					if rank(l) < 0 || i > 0 && rank(o.EffortCaps[i-1]) >= rank(l) {
						return out, fmt.Errorf("invalid effortCaps %s", e.Model)
					}
				}
				s.Efforts = append([]string(nil), o.EffortCaps...)
				if efforts, ok := m.AcceptedEfforts[e.Model]; ok {
					s.Efforts = append([]string(nil), efforts...)
				}
			} else if s.Context != ctx || s.Display != display {
				return out, fmt.Errorf("inconsistent model metadata %s", e.Model)
			}
			allowed := len(o.EffortCaps) == 0
			for _, l := range o.EffortCaps {
				if l == e.Effort {
					allowed = true
				}
			}
			if !allowed {
				out.Warnings = append(out.Warnings, fmt.Sprintf("skipped %s: effort above measured cap", e.Name))
				out.Notes = append(out.Notes, fmt.Sprintf("%s omitted: %s has no %s (measured cap)", e.Name, e.Model, e.Effort))
				models[e.Model] = s
				continue
			}
			if _, registered := m.AcceptedEfforts[e.Model]; len(o.EffortCaps) == 0 && !registered {
				s.Efforts = append(s.Efforts, e.Effort)
			}
			models[e.Model] = s
			n := Name{"local-cli", slug, e.Effort, e.Model, e.Display, ctx}
			if prior, ok := out.Names[e.Name]; ok {
				warning, err := resolveCollision(e.Name, prior, n, collisionRules)
				if err != nil {
					return out, err
				}
				out.Warnings = append(out.Warnings, warning)
				out.Collisions = append(out.Collisions, Collision{Code: e.Name, Winner: prior, Loser: n})
				out.Notes = append(out.Notes, fmt.Sprintf("%s means %s %s; use local-cli/%s@%s for %s %s", e.Name, prior.Model, prior.Effort, n.Slug, n.Effort, n.Model, n.Effort))
				continue
			}
			out.Names[e.Name] = n
		}
	}
	for id := range overrides {
		if !used[id] {
			out.Warnings = append(out.Warnings, "stale override "+id)
		}
	}
	for _, s := range models {
		sort.Slice(s.Efforts, func(i, j int) bool { return rank(s.Efforts[i]) < rank(s.Efforts[j]) })
		out.Models = append(out.Models, s)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Slug < out.Models[j].Slug })
	sort.Slice(out.Collisions, func(i, j int) bool { return out.Collisions[i].Code < out.Collisions[j].Code })
	out.Notes = append(out.Notes, uncodedNotes(out)...)
	sort.Strings(out.Warnings)
	sort.Strings(out.Info)
	sort.Strings(out.Notes)
	var err error
	out.Cmd, err = renderCmd(m.Version, out.Names, out.Notes)
	if err != nil {
		return out, err
	}
	out.Specs, err = renderSpecs(m.Version, out.Models)
	return out, err
}

func header(b *bytes.Buffer, version, pkg string) {
	fmt.Fprintf(b, "// Code generated by cahsync from cc-arch-hands %s; DO NOT EDIT.\npackage %s\nconst cahVersion = %q\n", version, pkg, version)
}

// uncodedNotes lists spec efforts that have no cah code (collision losers have their own note).
func uncodedNotes(out Output) []string {
	coded := map[string]bool{}
	for _, n := range out.Names {
		coded[n.Slug+"@"+n.Effort] = true
	}
	for _, c := range out.Collisions {
		coded[c.Loser.Slug+"@"+c.Loser.Effort] = true
	}
	var notes []string
	for _, s := range out.Models {
		for _, l := range s.Efforts {
			if !coded[s.Slug+"@"+l] {
				notes = append(notes, fmt.Sprintf("local-cli/%s@%s accepted (no cah code)", s.Slug, l))
			}
		}
	}
	return notes
}

func renderCmd(version string, names map[string]Name, notes []string) ([]byte, error) {
	var b bytes.Buffer
	header(&b, version, "cmd")
	b.WriteString("// cahNameMap is an exact (not prefix) code lookup, independent of legacy atoms.\ntype cahName struct { Provider, Slug, Effort, Model, Display string; Context int64 }\nvar cahNameMap = map[string]cahName{\n")
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := names[k]
		fmt.Fprintf(&b, "%q: {%q,%q,%q,%q,%q,%d},\n", k, n.Provider, n.Slug, n.Effort, n.Model, n.Display, n.Context)
	}
	b.WriteString("}\n\n// cahNotes are user-facing catalog notes for help output.\nvar cahNotes = []string{\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "%q,\n", n)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

func renderSpecs(version string, specs []Spec) ([]byte, error) {
	var b bytes.Buffer
	header(&b, version, "cliprovider")
	b.WriteString("// cahSpecs contains the generated model catalog.\n// Constructor adapters preserve current signatures and set per-model metadata.\nfunc cahClaudeSpec(id, display, arg string, ctx int64, efforts []string) CLISpec { s := claudeSpec(id,display,arg,ctx); s.EffortLevels=efforts; return s }\nfunc cahCodexSpec(id, display, arg string, ctx int64, efforts []string) CLISpec { s := codexSpec(id,display,arg,efforts); s.ContextWindow=ctx; return s }\nvar cahSpecs = []CLISpec{\n")
	for _, s := range specs {
		ctor := "cahClaudeSpec"
		if s.Family == "codex" {
			ctor = "cahCodexSpec"
		}
		fmt.Fprintf(&b, "%s(%q,%q,%q,%d,[]string{", ctor, s.Slug, s.Display, s.Arg, s.Context)
		for _, l := range s.Efforts {
			fmt.Fprintf(&b, "%q,", l)
		}
		b.WriteString("}),\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
