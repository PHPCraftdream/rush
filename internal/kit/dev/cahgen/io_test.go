package cahgen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func archive(t *testing.T, headers []*tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for i, h := range headers {
		if e := w.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if i < len(bodies) {
			if _, e := w.Write(bodies[i]); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}

func validArchive(t *testing.T) []byte { return validArchiveVersion(t, "0.16.0") }

func validArchiveVersion(t *testing.T, version string) []byte {
	source := []byte("export const AllModelCommands=[]; export const AllCodexAgents=[];")
	pkg := []byte(`{"name":"cc-arch-hands","version":"` + version + `"}`)
	return archive(t, []*tar.Header{{Name: "package/lib/manifest.js", Size: int64(len(source)), Mode: 0o600}, {Name: "package/package.json", Size: int64(len(pkg)), Mode: 0o600}}, [][]byte{source, pkg})
}

// Revert-check: Extract must reject unsafe tar paths/types and bounded bombs.
func TestExtract(t *testing.T) {
	if _, e := Extract(validArchive(t)); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"/absolute", "../escape", "package/../escape", "C:/escape", "package/a\\b", "package/a/../../b", "package/./a"} {
		t.Run(name, func(t *testing.T) {
			b := archive(t, []*tar.Header{{Name: name}}, nil)
			_, e := Extract(b)
			require.ErrorContains(t, e, "unsafe archive path")
		})
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo} {
		b := archive(t, []*tar.Header{{Name: "package/bad", Typeflag: typ, Linkname: "target"}}, nil)
		_, e := Extract(b)
		require.ErrorContains(t, e, "unsafe archive entry type")
	}
	duplicate := archive(t, []*tar.Header{{Name: "package/a"}, {Name: "package/a"}}, nil)
	_, e := Extract(duplicate)
	require.ErrorContains(t, e, "duplicate archive entry")
	if _, e := Extract([]byte("bad gzip")); e == nil {
		t.Fatal("bad gzip")
	}
	if _, e := Extract(make([]byte, MaxArchive+1)); e == nil {
		t.Fatal("compressed size")
	}
	b := validArchive(t)
	b[len(b)-5] ^= 0xff
	if _, e := Extract(b); e == nil {
		t.Fatal("checksum")
	}
	big := make([]byte, MaxFile+1)
	b = archive(t, []*tar.Header{{Name: "package/big", Size: int64(len(big))}}, [][]byte{big})
	_, e = Extract(b)
	require.ErrorContains(t, e, "oversized archive entry")
	var hs []*tar.Header
	var bs [][]byte
	for i := 0; i < 18; i++ {
		hs = append(hs, &tar.Header{Name: fmt.Sprintf("package/%d", i), Size: MaxFile})
		bs = append(bs, make([]byte, MaxFile))
	}
	_, e = Extract(archive(t, hs, bs))
	require.Error(t, e)
	require.NotContains(t, e.Error(), "missing manifest")
	require.True(t, strings.Contains(e.Error(), "unexpected EOF") || strings.Contains(e.Error(), "expanded archive too large"), e.Error())
}

// Revert-check: ValidateModule dependency guard must run before Node evaluation.
func TestModuleGuard(t *testing.T) {
	for _, s := range []string{`import x from 'x'`, `import('x')`, `require /* x */ ('x')`, `const x = \u0069mport('x')`, "const x = `${require('x')}`"} {
		if e := ValidateModule([]byte(s)); e == nil {
			t.Fatal(s)
		}
	}
	if e := ValidateModule([]byte("// skills require runtime\nexport const x=['require'];")); e != nil {
		t.Fatal(e)
	}
}

type fakeSource struct {
	data   map[string][]byte
	err    string
	writes int
}

func (f *fakeSource) TempDir() (string, error) {
	if f.err == "temp" {
		return "", errors.New("temp")
	}
	return "workspace", nil
}

func (f *fakeSource) Read(p string) ([]byte, error) {
	if f.err == "read" {
		return nil, errors.New("read")
	}
	b, ok := f.data[filepath.Base(p)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeSource) Write(p string, b []byte) error {
	f.writes++
	if f.err == "write" {
		return errors.New("write")
	}
	f.data[filepath.Base(p)] = b
	return nil
}
func (f *fakeSource) RemoveAll(string) error { return nil }

type fakeRunner struct {
	raw, packed []byte
	fail        string
	err         error
	calls       []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.fail == name {
		if f.err != nil {
			return nil, f.err
		}
		return nil, errors.New(name)
	}
	if name == "npm" {
		return f.packed, nil
	}
	return f.raw, nil
}

// acquisition serves the frozen 0.16.0 fixture, never the live snapshot.
func acquisition(t *testing.T) (*fakeRunner, *fakeSource) {
	t.Helper()
	return acquisitionOf(t, fixture(t), "0.16.0")
}

func acquisitionOf(t *testing.T, m Manifest, version string) (*fakeRunner, *fakeSource) {
	t.Helper()
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	tgz := "cc-arch-hands-" + version + ".tgz"
	return &fakeRunner{raw: raw, packed: []byte(`[{"filename":"` + tgz + `"}]`)}, &fakeSource{data: map[string][]byte{tgz: validArchiveVersion(t, version)}}
}

// Revert-check: Acquire npm --ignore-scripts, JSON arrays and pre-eval guard.
func TestAcquire(t *testing.T) {
	r, s := acquisition(t)
	m, e := Acquire(context.Background(), "latest", r, s)
	if e != nil || m.Version != "0.16.0" {
		t.Fatal(m.Version, e)
	}
	if !strings.Contains(r.calls[0], "--ignore-scripts") || !strings.HasPrefix(r.calls[1], "node ") {
		t.Fatal(r.calls)
	}
	for _, failure := range []string{"npm", "node", "temp", "read", "write", "pack", "filename", "json", "archive", "guard", "version"} {
		t.Run(failure, func(t *testing.T) {
			r, s := acquisition(t)
			switch failure {
			case "npm", "node":
				r.fail = failure
			case "temp", "read", "write":
				s.err = failure
			case "pack":
				r.packed = []byte(`[]`)
			case "filename":
				r.packed = []byte(`[{"filename":"../bad"}]`)
			case "json":
				r.raw = []byte(`{"claude":[],"codex":[],"unknown":1}`)
			case "archive":
				s.data["cc-arch-hands-0.16.0.tgz"] = []byte("bad")
			case "guard":
				src := []byte(`require('x')`)
				pkg := []byte(`{"name":"cc-arch-hands","version":"0.16.0"}`)
				s.data["cc-arch-hands-0.16.0.tgz"] = archive(t, []*tar.Header{{Name: "package/lib/manifest.js", Size: int64(len(src))}, {Name: "package/package.json", Size: int64(len(pkg))}}, [][]byte{src, pkg})
			case "version":
			}
			version := "latest"
			if failure == "version" {
				version = "0.15.0"
			}
			if _, e := Acquire(context.Background(), version, r, s); e == nil {
				t.Fatal("accepted")
			}
			if failure == "guard" && len(r.calls) != 1 {
				t.Fatal("evaluated unsafe module")
			}
		})
	}
}

type fakeWriter struct {
	readErr               error
	files                 map[string][]byte
	stage, rename         int
	failStage, failRename int
}

func (f *fakeWriter) Read(p string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	b, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (f *fakeWriter) Stage(p string, b []byte) (string, error) {
	f.stage++
	if f.stage == f.failStage {
		return "", errors.New("stage")
	}
	n := fmt.Sprintf("temp%d", f.stage)
	f.files[n] = append([]byte(nil), b...)
	return n, nil
}

func (f *fakeWriter) Rename(a, b string) error {
	f.rename++
	if f.rename == f.failRename {
		return errors.New("rename")
	}
	f.files[b] = f.files[a]
	delete(f.files, a)
	return nil
}
func (f *fakeWriter) Remove(p string) error { delete(f.files, p); return nil }

// Revert-check: Publish stages both outputs, changes only drift, rolls back failures.
func TestPublish(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, failure := range []string{"none", "stage1", "stage2", "stage3", "stage4", "rename1", "rename2"} {
			t.Run(fmt.Sprint(existing, failure), func(t *testing.T) {
				w := &fakeWriter{files: map[string][]byte{}}
				if existing {
					w.files["a"] = []byte("oldA")
					w.files["b"] = []byte("oldB")
				}
				switch failure {
				case "stage1":
					w.failStage = 1
				case "stage2":
					w.failStage = 2
				case "stage3":
					w.failStage = 3
				case "stage4":
					w.failStage = 4
				case "rename1":
					w.failRename = 1
				case "rename2":
					w.failRename = 2
				}
				e := Publish(w, []string{"a", "b"}, [][]byte{[]byte("newA"), []byte("newB")})
				failed := failure != "none" && (existing || failure != "stage3" && failure != "stage4")
				if failed {
					if e == nil {
						t.Fatal("expected failure")
					}
					if existing {
						if string(w.files["a"]) != "oldA" || string(w.files["b"]) != "oldB" {
							t.Fatal("rollback", w.files)
						}
					} else if len(w.files) != 0 {
						t.Fatal("created output after failure", w.files)
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					before := w.stage
					if e := Publish(w, []string{"a", "b"}, [][]byte{[]byte("newA"), []byte("newB")}); e != nil || w.stage != before {
						t.Fatal("unchanged write")
					}
				}
				for p := range w.files {
					if strings.HasPrefix(p, "temp") {
						t.Fatal("temp leaked")
					}
				}
			})
		}
	}
}

// Revert-check: Run owns offline/check/refresh, fallback and strict CI policy.
func TestModes(t *testing.T) {
	if code, e := Run(context.Background(), Options{Mode: "offline"}, Dependencies{Env: func(string) string { panic("offline I/O") }}); code != 0 || e != nil {
		t.Fatal(code, e)
	}
	for _, mode := range []string{"check", "refresh"} {
		for _, strict := range []bool{false, true} {
			for _, ci := range []string{"", "true"} {
				r, s := acquisition(t)
				r.fail = "npm"
				w := &fakeWriter{files: map[string][]byte{"untouched": []byte("old")}}
				code, e := Run(context.Background(), Options{Mode: mode, Strict: strict}, Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return ci }})
				if strict || ci == "true" {
					if code != 1 || e == nil {
						t.Fatal("strict")
					}
				} else if code != 0 || e != nil {
					t.Fatal("fallback")
				}
				if w.stage != 0 || len(w.files) != 1 {
					t.Fatal("failure wrote")
				}
			}
		}
	}
	r, s := acquisition(t)
	w := &fakeWriter{files: map[string][]byte{}}
	d := Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }}
	if c, e := Run(context.Background(), Options{Mode: "check"}, d); c != 1 || e != nil || w.stage != 0 {
		t.Fatal("check drift", c, e)
	}
	if c, e := Run(context.Background(), Options{Mode: "refresh"}, d); c != 0 || e != nil {
		t.Fatal(c, e)
	}
	if c, e := Run(context.Background(), Options{Mode: "check"}, d); c != 0 || e != nil {
		t.Fatal("check clean", c, e)
	}
	if c, e := Run(context.Background(), Options{Mode: "invalid"}, d); c != 1 || e == nil {
		t.Fatal("invalid mode")
	}
}

// Revert-check: OSWriter performs same-directory staging and cleanup.
func TestOSWriter(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	w := OSWriter{}
	if e := Publish(w, []string{a, b}, [][]byte{[]byte("a"), []byte("b")}); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 2 {
		t.Fatal(entries, e)
	}
}

// Revert-check: Acquire preserves tool error identity before applying policy.
func TestAcquireToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"npm", os.ErrNotExist},
		{"npm", &exec.ExitError{}},
		{"node", os.ErrNotExist},
		{"node", fmt.Errorf("SyntaxError: invalid manifest")},
	} {
		r, source := acquisition(t)
		r.fail, r.err = tc.name, tc.err
		_, err := Acquire(context.Background(), "latest", r, source)
		require.ErrorIs(t, err, tc.err)
		require.Equal(t, tc.err.Error(), err.Error())
	}
}

// Revert-check: all operational failures warn locally and preserve outputs in strict mode.
func TestFailureCategories(t *testing.T) {
	for _, category := range []string{"npm-missing", "npm-exit", "node-missing", "syntax", "temp", "read", "write", "pack", "archive", "guard", "decode", "transform", "output-read", "stage", "rename"} {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprint(category, strict), func(t *testing.T) {
				r, source := acquisition(t)
				w := &fakeWriter{files: map[string][]byte{filepath.FromSlash(CmdPath): []byte("oldA"), filepath.FromSlash(SpecsPath): []byte("oldB")}}
				switch category {
				case "npm-missing":
					r.fail, r.err = "npm", os.ErrNotExist
				case "npm-exit":
					r.fail, r.err = "npm", &exec.ExitError{}
				case "node-missing":
					r.fail, r.err = "node", os.ErrNotExist
				case "syntax":
					r.fail, r.err = "node", errors.New("SyntaxError: invalid manifest")
				case "temp", "read", "write":
					source.err = category
				case "pack":
					r.packed = []byte("[]")
				case "archive":
					source.data["cc-arch-hands-0.16.0.tgz"] = []byte("bad")
				case "guard":
					src, pkg := []byte("require('x')"), []byte(`{"name":"cc-arch-hands","version":"0.16.0"}`)
					source.data["cc-arch-hands-0.16.0.tgz"] = archive(t, []*tar.Header{{Name: "package/lib/manifest.js", Size: int64(len(src))}, {Name: "package/package.json", Size: int64(len(pkg))}}, [][]byte{src, pkg})
				case "decode":
					r.raw = []byte("bad JSON")
				case "transform":
					r.raw = []byte(`{"claude":[],"codex":[]}`)
				case "output-read":
					w.readErr = os.ErrPermission
				case "stage":
					w.failStage = 3
				case "rename":
					w.failRename = 2
				}
				var logs []string
				code, err := Run(context.Background(), Options{Mode: "refresh", Strict: strict}, Dependencies{Runner: r, Source: source, Writer: w, Env: func(string) string { return "" }, Log: func(s string) { logs = append(logs, s) }})
				if strict {
					require.Equal(t, 1, code)
					require.Error(t, err)
				} else {
					require.Zero(t, code)
					require.NoError(t, err)
					require.Contains(t, strings.Join(logs, ";"), "warning: cahsync failed; committed outputs untouched:")
				}
				require.Equal(t, "oldA", string(w.files[filepath.FromSlash(CmdPath)]))
				require.Equal(t, "oldB", string(w.files[filepath.FromSlash(SpecsPath)]))
				require.Len(t, w.files, 2)
			})
		}
	}
}
