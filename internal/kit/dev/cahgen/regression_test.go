package cahgen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type brokenRollback struct{ *fakeWriter }

func (w brokenRollback) Rename(a, b string) error {
	w.rename++
	if w.rename >= 2 {
		return errors.New("rename and rollback")
	}
	w.files[b] = w.files[a]
	delete(w.files, a)
	return nil
}

// Revert-check: Publication rollback damage fails closed even outside strict mode.
func TestRollbackFailure(t *testing.T) {
	w := brokenRollback{&fakeWriter{files: map[string][]byte{"a": []byte("old"), "b": []byte("old")}}}
	e := Publish(w, []string{"a", "b"}, [][]byte{[]byte("new"), []byte("new")})
	if _, ok := e.(*RollbackError); !ok {
		t.Fatal(e)
	}
	for _, strict := range []bool{false, true} {
		r, s := acquisition(t)
		w = brokenRollback{&fakeWriter{files: map[string][]byte{filepath.FromSlash(CmdPath): []byte("old"), filepath.FromSlash(SpecsPath): []byte("old")}}}
		var logs []string
		c, e := Run(context.Background(), Options{Mode: "refresh", Strict: strict}, Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }, Log: func(s string) { logs = append(logs, s) }})
		require.Equal(t, 1, c)
		require.Error(t, e)
		var rollback *RollbackError
		require.ErrorAs(t, e, &rollback)
		for _, log := range logs {
			require.NotContains(t, log, "committed outputs untouched")
			require.NotContains(t, log, "outputs may be changed")
		}
	}
}

// Revert-check: Run writer failures must obey strict policy without output drift.
func TestWriterFallback(t *testing.T) {
	for _, strict := range []bool{false, true} {
		for _, failure := range []string{"stage", "rename"} {
			r, s := acquisition(t)
			w := &fakeWriter{files: map[string][]byte{filepath.FromSlash(CmdPath): []byte("oldA"), filepath.FromSlash(SpecsPath): []byte("oldB")}}
			if failure == "stage" {
				w.failStage = 3
			} else {
				w.failRename = 2
			}
			c, e := Run(context.Background(), Options{Mode: "refresh", Strict: strict}, Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }})
			if strict {
				if c != 1 || e == nil {
					t.Fatal(c, e)
				}
			} else if c != 0 || e != nil {
				t.Fatal(c, e)
			}
			if string(w.files[filepath.FromSlash(CmdPath)]) != "oldA" || string(w.files[filepath.FromSlash(SpecsPath)]) != "oldB" {
				t.Fatal("changed fallback")
			}
		}
	}
}

// Revert-check: Entry.UnmarshalJSON rejects absent, null, zero and unknown fields.
func TestExactFields(t *testing.T) {
	for _, s := range []string{
		`{"claude":[{"name":"x","model":"claude-opus-5","effort":"low"}]}`,
		`{"claude":[{"name":"x","model":"claude-opus-5","effort":"low","display":"x","contextWindow":0}]}`,
		`{"claude":[{"name":"x","model":"claude-opus-5","effort":"low","display":"x","contextWindow":null}]}`,
		`{"claude":[{"name":"x","model":"claude-opus-5","effort":"low","display":"x","extra":true}]}`,
	} {
		if _, e := Decode([]byte(s)); e == nil {
			t.Fatal(s)
		}
	}
}

// Revert-check: OSSource bounds reads and removes temporary acquisition workspaces.
func TestOSSource(t *testing.T) {
	s := OSSource{}
	dir, e := s.TempDir()
	if e != nil {
		t.Fatal(e)
	}
	defer s.RemoveAll(dir)
	p := dir + "/source"
	if e = s.Write(p, []byte("source")); e != nil {
		t.Fatal(e)
	}
	b, e := s.Read(p)
	if e != nil || string(b) != "source" {
		t.Fatal(e)
	}
	if e = s.RemoveAll(dir); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal(e)
	}
}
