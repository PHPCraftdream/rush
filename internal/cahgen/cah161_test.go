package cahgen

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixture161(t *testing.T) Manifest {
	t.Helper()
	b, err := os.ReadFile("testdata/fixture/manifest-0.16.1.json")
	require.NoError(t, err)
	m, err := Decode(b)
	require.NoError(t, err)
	return m
}

func TestCAH161Acquire(t *testing.T) {
	src, err := os.ReadFile("testdata/fixture/manifest-0.16.1.js")
	require.NoError(t, err)
	specs, err := os.ReadFile("testdata/fixture/codex-model-specs-0.16.1.js")
	require.NoError(t, err)
	for _, tc := range []struct {
		name          string
		source, specs []byte
		omit, fail    bool
	}{
		{name: "actual", source: src, specs: specs},
		{name: "missing", source: src, omit: true, fail: true},
		{name: "require", source: append(bytes.Clone(src), []byte("require('bad')")...), specs: specs, fail: true},
		{name: "adjacent import", source: append(bytes.Clone(src), []byte("import x from 'bad';")...), specs: specs, fail: true},
		{name: "wrong binding", source: []byte("import { Other } from './codex-model-specs.js';"), specs: specs, fail: true},
		{name: "wrong source", source: []byte("import { CodexModelSpecs } from './other.js';"), specs: specs, fail: true},
		{name: "specs import", source: src, specs: []byte("import x from 'bad';"), fail: true},
		{name: "specs require", source: src, specs: []byte("require('bad');"), fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := acquisitionOf(t, fixture161(t), "0.16.1")
			pkg := []byte(`{"name":"cc-arch-hands","version":"0.16.1","type":"module"}`)
			hs := []*tar.Header{{Name: "package/lib/manifest.js", Size: int64(len(tc.source))}, {Name: "package/package.json", Size: int64(len(pkg))}}
			bs := [][]byte{tc.source, pkg}
			if !tc.omit {
				hs = append(hs, &tar.Header{Name: "package/lib/codex-model-specs.js", Size: int64(len(tc.specs))})
				bs = append(bs, tc.specs)
			}
			s.data["cc-arch-hands-0.16.1.tgz"] = archive(t, hs, bs)
			m, err := Acquire(context.Background(), "0.16.1", r, s)
			if tc.fail {
				require.Error(t, err)
				require.Len(t, r.calls, 1)
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(872000), m.Codex[0].MaxContextWindow)
			require.Equal(t, tc.specs, s.data["codex-model-specs.js"])
			require.JSONEq(t, `{"type":"module"}`, string(s.data["package.json"]))
			require.Contains(t, r.calls[1], "spec.efforts")
			require.NoError(t, ValidateManifest([]byte(`import { CodexModelSpecs } from "./codex-model-specs.js";`), true))
			require.Error(t, ValidateModule(src))
		})
	}
}

func TestCAH161DowngradeAndSnapshotDrift(t *testing.T) {
	for _, strict := range []bool{false, true} {
		w := &fakeWriter{files: map[string][]byte{}}
		r, s := acquisitionOf(t, fixture161(t), "0.16.1")
		d := Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }}
		code, err := Run(context.Background(), Options{Root: "root", Strict: true}, d)
		require.NoError(t, err)
		require.Zero(t, code)
		before := map[string][]byte{}
		for p, b := range w.files {
			before[p] = bytes.Clone(b)
		}
		old := fixture(t)
		old.Version = "0.15.1"
		r, s = acquisitionOf(t, old, "0.15.1")
		d.Runner = r
		d.Source = s
		var logs []string
		d.Log = func(s string) { logs = append(logs, s) }
		code, err = Run(context.Background(), Options{Root: "root", Strict: strict}, d)
		if strict {
			require.Error(t, err)
			require.Equal(t, 1, code)
		} else {
			require.NoError(t, err)
			require.Zero(t, code)
		}
		require.Equal(t, before, w.files)
		require.Contains(t, strings.Join(logs, "\n"), "npm returned 0.15.1, older than committed 0.16.1")
		require.NoError(t, CheckCommitted(w.Read, "root"))
		for p, b := range w.files {
			if strings.HasSuffix(p, "manifest.json") {
				w.files[p] = bytes.Replace(b, []byte(`"maxContextWindow": 872000`), []byte(`"maxContextWindow": 873000`), 1)
			}
		}
		// Max is metadata only, so valid max drift is caught by live check, not generated specs.
		r, s = acquisitionOf(t, fixture161(t), "0.16.1")
		d.Runner = r
		d.Source = s
		code, err = Run(context.Background(), Options{Root: "root", Mode: "check", Strict: true}, d)
		require.NoError(t, err)
		require.Equal(t, 1, code)
	}
}

func TestCAH161SchemaAndCrosschecks(t *testing.T) {
	m := fixture161(t)
	out, err := Transform(m, Overrides())
	require.NoError(t, err)
	require.Contains(t, out.Names, "us1")
	require.Contains(t, out.Names, "ua")
	require.NotContains(t, out.Names, "ul1")
	require.Len(t, out.Notes, 1)
	raw, err := RenderSnapshot(m)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"maxContextWindow": 872000`)
	require.Contains(t, string(raw), `"acceptedEfforts"`)
	for _, registry := range []string{`null`, `[]`, `{"gpt-6.1-sol":null}`, `{"gpt-6.1-sol":[1]}`} {
		_, err := Decode([]byte(`{"acceptedEfforts":` + registry + `}`))
		require.Error(t, err)
	}
	for _, value := range []string{"null", "0", "-1", "1.5", `"872000"`, "271999"} {
		_, err := Decode(bytes.ReplaceAll(raw, []byte(`"maxContextWindow": 872000`), []byte(`"maxContextWindow": `+value)))
		require.Error(t, err, value)
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Claude[0].ContextWindow = 200000 },
		func(m *Manifest) { m.Codex[1].ContextWindow = 273000 },
		func(m *Manifest) {
			m.AcceptedEfforts[m.Codex[0].Model] = []string{"low", "medium", "high", "xhigh", "max"}
		},
		func(m *Manifest) { m.AcceptedEfforts["gpt-9-sol"] = []string{"low"} },
		func(m *Manifest) { m.AcceptedEfforts[m.Codex[0].Model] = []string{"low", "low"} },
		func(m *Manifest) { m.AcceptedEfforts[m.Codex[0].Model] = []string{"medium", "low"} },
		func(m *Manifest) { m.AcceptedEfforts[m.Codex[0].Model] = []string{"unknown"} },
	} {
		m := fixture161(t)
		change(&m)
		_, err := Transform(m, Overrides())
		require.Error(t, err)
	}
	overrides := Overrides()
	o := overrides[m.Codex[0].Model]
	o.Context = 300000
	overrides[m.Codex[0].Model] = o
	_, err = Transform(m, overrides)
	require.ErrorContains(t, err, "conflicting measured context")
	// A valid but different registry must fail against measured caps even when no alias uses the missing level.
	m = fixture161(t)
	model := "gpt-6-luna"
	m.AcceptedEfforts[model] = append(m.AcceptedEfforts[model], "ultra")
	_, err = Transform(m, Overrides())
	require.ErrorContains(t, err, "measured cap mismatch")
	b, _ := json.Marshal(m)
	_, err = Decode(b)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(out.Specs), "872000"))
}
