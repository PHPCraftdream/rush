package cahgen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	MaxArchive  = 32 << 20
	MaxExpanded = 64 << 20
	MaxFile     = 4 << 20
)

// Extract validates the entire archive before returning the two required files
// and optional specs module. Links and special files fail; entries stay in memory.
func Extract(data []byte) (map[string][]byte, error) {
	if len(data) > MaxArchive {
		return nil, fmt.Errorf("archive too large")
	}
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	limited := &io.LimitedReader{R: z, N: MaxExpanded + 1}
	t := tar.NewReader(limited)
	files := map[string][]byte{}
	seen := map[string]bool{}
	count := 0
	for {
		h, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		count++
		if count > 10000 {
			return nil, fmt.Errorf("too many archive entries")
		}
		n := h.Name
		if strings.Contains(n, "\\") || strings.Contains(n, ":") || strings.HasPrefix(n, "/") || path.Clean(n) != strings.TrimSuffix(n, "/") || !strings.HasPrefix(n, "package/") || strings.Contains(n, "\x00") {
			return nil, fmt.Errorf("unsafe archive path %q", n)
		}
		for _, part := range strings.Split(n, "/") {
			if part == ".." {
				return nil, fmt.Errorf("archive traversal")
			}
		}
		if seen[n] {
			return nil, fmt.Errorf("duplicate archive entry")
		}
		seen[n] = true
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("unsafe archive entry type")
		}
		if h.Size < 0 || h.Size > MaxFile {
			return nil, fmt.Errorf("oversized archive entry")
		}
		if h.Typeflag == tar.TypeDir {
			if h.Size != 0 {
				return nil, fmt.Errorf("nonempty directory")
			}
			continue
		}
		if n == "package/lib/manifest.js" || n == "package/package.json" || n == "package/lib/codex-model-specs.js" {
			b, err := io.ReadAll(io.LimitReader(t, MaxFile+1))
			if err != nil {
				return nil, err
			}
			files[n] = b
		}
	}
	// Drain gzip to verify checksum and bound padding/trailing decompressed data.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("expanded archive too large")
	}
	if files["package/lib/manifest.js"] == nil || files["package/package.json"] == nil {
		return nil, fmt.Errorf("missing manifest or package metadata")
	}
	return files, nil
}

// ValidateManifest permits only the observed leading static specs import.
// The remaining source and the specs module still use the strict guard.
func ValidateManifest(source []byte, hasSpecs bool) error {
	for _, prefix := range []string{"import { CodexModelSpecs } from './codex-model-specs.js';", "import { CodexModelSpecs } from \"./codex-model-specs.js\";"} {
		if bytes.HasPrefix(source, []byte(prefix)) {
			if !hasSpecs {
				return fmt.Errorf("missing codex-model-specs.js")
			}
			return ValidateModule(source[len(prefix):])
		}
	}
	return ValidateModule(source)
}

// ValidateModule rejects import/require before evaluation, ignoring quoted prose.
// Templates and escaped identifiers are conservatively rejected.
func ValidateModule(source []byte) error {
	if len(source) > MaxFile {
		return fmt.Errorf("manifest exceeds bound")
	}
	for i := 0; i < len(source); {
		c := source[i]
		if c == '`' {
			return fmt.Errorf("manifest template literals unsupported")
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '/' {
			i += 2
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(source) && source[i+1] == '*' {
			i += 2
			for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
				i++
			}
			if i+1 >= len(source) {
				return fmt.Errorf("unterminated comment")
			}
			i += 2
			continue
		}
		if c == '\'' || c == '"' {
			quote := c
			i++
			closed := false
			for i < len(source) {
				if source[i] == '\\' {
					i += 2
					continue
				}
				if source[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("unterminated literal")
			}
			continue
		}
		if c == '\\' {
			return fmt.Errorf("escaped identifier unsupported")
		}
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' {
			start := i
			i++
			for i < len(source) {
				v := source[i]
				if !(v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '_' || v == '$') {
					break
				}
				i++
			}
			word := string(source[start:i])
			if word == "import" || word == "require" {
				return fmt.Errorf("manifest contains import/require")
			}
			continue
		}
		i++
	}
	return nil
}

// Runner is injectable: tests never execute npm, node, or provider CLIs.
type Runner interface {
	Run(context.Context, string, string, ...string) ([]byte, error)
}
type (
	ExecRunner    struct{}
	boundedBuffer struct {
		bytes.Buffer
		limit int
	}
)

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("process output exceeds bound")
	}
	return b.Buffer.Write(p)
}

// runnerCommand resolves Windows npm's batch shim through the command processor.
func runnerCommand(platform, name string, args []string, lookup func(string) (string, error)) (string, []string, error) {
	if platform != "windows" || name != "npm" {
		return name, args, nil
	}
	shim, err := lookup("npm.cmd")
	if err != nil {
		return "", nil, err
	}
	return "cmd.exe", append([]string{"/d", "/s", "/c", shim}, args...), nil
}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	executable, argv, err := runnerCommand(runtime.GOOS, name, args, exec.LookPath)
	if err != nil {
		return nil, fmt.Errorf("%s resolution failed: %w", name, err)
	}
	c := exec.CommandContext(ctx, executable, argv...)
	configureRunnerCommand(c, executable, argv)
	c.Dir = dir
	out := &boundedBuffer{limit: MaxFile}
	errout := &boundedBuffer{limit: 64 << 10}
	c.Stdout = out
	c.Stderr = errout
	err = c.Run()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(errout.String()))
	}
	return out.Bytes(), nil
}

// SourceIO isolates the temporary acquisition workspace from output publication.
type SourceIO interface {
	TempDir() (string, error)
	Read(string) ([]byte, error)
	Write(string, []byte) error
	RemoveAll(string) error
}
type OSSource struct{}

func (OSSource) TempDir() (string, error) { return os.MkdirTemp("", "cahsync-") }
func (OSSource) Read(p string) ([]byte, error) {
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, MaxArchive+1))
	if len(b) > MaxArchive {
		return nil, fmt.Errorf("source file exceeds bound")
	}
	return b, e
}
func (OSSource) Write(p string, b []byte) error { return os.WriteFile(p, b, 0o600) }
func (OSSource) RemoveAll(p string) error       { return os.RemoveAll(p) }
func Acquire(ctx context.Context, version string, r Runner, fs SourceIO) (Manifest, error) {
	var zero Manifest
	if version == "" {
		version = "latest"
	}
	if version != "latest" && !versionRE.MatchString(version) {
		return zero, fmt.Errorf("invalid package version")
	}
	dir, err := fs.TempDir()
	if err != nil {
		return zero, err
	}
	defer fs.RemoveAll(dir)
	packed, err := r.Run(ctx, dir, "npm", "pack", "cc-arch-hands@"+version, "--ignore-scripts", "--json")
	if err != nil {
		return zero, err
	}
	var packs []struct {
		Filename string `json:"filename"`
	}
	if err = json.Unmarshal(packed, &packs); err != nil || len(packs) != 1 {
		return zero, fmt.Errorf("invalid npm pack response")
	}
	filename := packs[0].Filename
	if !regexp.MustCompile(`^cc-arch-hands-[a-zA-Z0-9.-]+\.tgz$`).MatchString(filename) {
		return zero, fmt.Errorf("unsafe npm filename")
	}
	archive, err := fs.Read(filepath.Join(dir, filename))
	if err != nil {
		return zero, err
	}
	files, err := Extract(archive)
	if err != nil {
		return zero, err
	}
	source := files["package/lib/manifest.js"]
	specs, hasSpecs := files["package/lib/codex-model-specs.js"]
	if err = ValidateManifest(source, hasSpecs); err != nil {
		return zero, err
	}
	if hasSpecs {
		if err = ValidateModule(specs); err != nil {
			return zero, err
		}
	}
	var pkg struct{ Name, Version string }
	if err = json.Unmarshal(files["package/package.json"], &pkg); err != nil || pkg.Name != "cc-arch-hands" || !versionRE.MatchString(pkg.Version) {
		return zero, fmt.Errorf("invalid package metadata")
	}
	if version != "latest" && version != pkg.Version {
		return zero, fmt.Errorf("package version mismatch")
	}
	module := filepath.Join(dir, "manifest.mjs")
	if err = fs.Write(module, source); err != nil {
		return zero, err
	}
	if hasSpecs {
		if err = fs.Write(filepath.Join(dir, "codex-model-specs.js"), specs); err != nil {
			return zero, err
		}
		if err = fs.Write(filepath.Join(dir, "package.json"), []byte(`{"type":"module"}`)); err != nil {
			return zero, err
		}
	}
	script := `import {AllModelCommands,AllCodexAgents} from './manifest.mjs'; console.log(JSON.stringify({claude:AllModelCommands,codex:AllCodexAgents}));`
	if hasSpecs {
		script = `import {AllModelCommands,AllCodexAgents} from './manifest.mjs'; import {CodexModelSpecs} from './codex-model-specs.js'; console.log(JSON.stringify({claude:AllModelCommands,codex:AllCodexAgents,acceptedEfforts:Object.fromEntries(Object.entries(CodexModelSpecs).map(([model,spec])=>[model,spec.efforts]))}));`
	}
	raw, err := r.Run(ctx, dir, "node", "--max-old-space-size=64", "--input-type=module", "-e", script)
	if err != nil {
		return zero, err
	}
	m, err := Decode(raw)
	if err != nil {
		return zero, err
	}
	m.Version = pkg.Version
	return m, nil
}

// Writer supports atomic same-directory staging and rollback without assuming
// OS filesystem operations in tests. Stage must fully write and close its temp.
type Writer interface {
	Read(string) ([]byte, error)
	Stage(string, []byte) (string, error)
	Rename(string, string) error
	Remove(string) error
}
type OSWriter struct{}

func (OSWriter) Read(p string) ([]byte, error) { return os.ReadFile(p) }
func (OSWriter) Stage(p string, b []byte) (string, error) {
	f, e := os.CreateTemp(filepath.Dir(p), ".cahsync-*")
	if e != nil {
		return "", e
	}
	n := f.Name()
	if _, e = f.Write(b); e == nil {
		e = f.Chmod(0o644)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		os.Remove(n)
		return "", e
	}
	return n, nil
}
func (OSWriter) Rename(a, b string) error { return os.Rename(a, b) }
func (OSWriter) Remove(p string) error    { return os.Remove(p) }

const (
	CmdPath   = "internal/cmd/models_cah_gen.go"
	SpecsPath = "internal/agent/cliprovider/models_cah_gen.go"
)

type Options struct {
	Root, Mode, Version    string
	Strict, Check, Offline bool
}
type Dependencies struct {
	Runner Runner
	Source SourceIO
	Writer Writer
	Env    func(string) string
	Log    func(string)
}

// ResolveMode rejects conflicting flags before any acquisition or I/O.
func ResolveMode(mode string, check, offline bool) (string, error) {
	if check && offline {
		return "", fmt.Errorf("-check and -offline conflict")
	}
	selected := ""
	if check {
		selected = "check"
	}
	if offline {
		selected = "offline"
	}
	if selected != "" && mode != "" && mode != selected {
		return "", fmt.Errorf("-mode %s conflicts with -%s", mode, selected)
	}
	if selected != "" {
		mode = selected
	}
	if mode == "" {
		mode = "refresh"
	}
	if mode != "refresh" && mode != "check" && mode != "offline" {
		return "", fmt.Errorf("invalid mode %q", mode)
	}
	return mode, nil
}

// Run owns modes, strict/CI policy, fallback and publication. Offline returns
// immediately without invoking any dependency (including environment lookup).
func Run(ctx context.Context, o Options, d Dependencies) (int, error) {
	mode, err := ResolveMode(o.Mode, o.Check, o.Offline)
	if err != nil {
		return 1, err
	}
	o.Mode = mode
	if o.Mode == "offline" {
		return 0, nil
	}
	if o.Mode != "refresh" && o.Mode != "check" {
		return 1, fmt.Errorf("invalid mode %q", o.Mode)
	}
	if d.Runner == nil {
		d.Runner = ExecRunner{}
	}
	if d.Source == nil {
		d.Source = OSSource{}
	}
	if d.Writer == nil {
		d.Writer = OSWriter{}
	}
	if d.Env == nil {
		d.Env = os.Getenv
	}
	if d.Log == nil {
		d.Log = func(string) {}
	}
	strict := o.Strict || d.Env("CI") == "true"
	fail := func(err error) (int, error) {
		return failure(strict, err, d.Log)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m, err := Acquire(ctx, o.Version, d.Runner, d.Source)
	if err != nil {
		return fail(err)
	}
	d.Log("cah version: " + m.Version)
	cmdPath := filepath.Join(o.Root, filepath.FromSlash(CmdPath))
	committed, readErr := d.Writer.Read(cmdPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fail(readErr)
	}
	// An older acquisition (e.g. npm release-age policy) must never downgrade committed tables.
	if have := committedVersion(committed); have != "" && semverLess(m.Version, have) {
		msg := fmt.Sprintf("npm returned %s, older than committed %s (npm min-release-age?) — keeping committed files", m.Version, have)
		d.Log("warning: " + msg)
		if strict {
			return 1, errors.New(msg)
		}
		return 0, nil
	}
	out, err := Transform(m, Overrides())
	if err != nil {
		return fail(err)
	}
	for _, s := range out.Warnings {
		d.Log("warning: " + s)
	}
	for _, s := range out.Info {
		d.Log("info: " + s)
	}
	snapshot, err := RenderSnapshot(m)
	if err != nil {
		return fail(err)
	}
	paths := []string{filepath.Join(o.Root, filepath.FromSlash(CmdPath)), filepath.Join(o.Root, filepath.FromSlash(SpecsPath)), filepath.Join(o.Root, filepath.FromSlash(SnapshotPath))}
	contents := [][]byte{out.Cmd, out.Specs, snapshot}
	changed := false
	var changedPaths []string
	for i, p := range paths {
		b, e := d.Writer.Read(p)
		if e != nil && !os.IsNotExist(e) {
			return fail(e)
		}
		if !bytes.Equal(b, contents[i]) || os.IsNotExist(e) {
			changed = true
			changedPaths = append(changedPaths, p)
		}
	}
	if o.Mode == "check" {
		if changed {
			d.Log("generated catalog drift")
			return 1, nil
		}
		return 0, nil
	}
	if !changed {
		return 0, nil
	}
	if err = Publish(d.Writer, paths, contents); err != nil {
		return fail(err)
	}
	for _, p := range changedPaths {
		d.Log(filepath.Base(p) + " changed: commit it: " + p)
	}
	d.Log("cah catalog files changed: commit them together")
	return 0, nil
}

// RollbackError reports publication and restoration failures.
type RollbackError struct{ Cause, Restore error }

func (e *RollbackError) Error() string {
	return fmt.Sprintf("publication failed: %v; rollback failed: %v", e.Cause, e.Restore)
}

func Publish(w Writer, paths []string, contents [][]byte) error {
	if len(paths) != len(contents) {
		return fmt.Errorf("publication length mismatch")
	}
	type item struct {
		path, temp, backup string
		existed            bool
	}
	var items []item
	var temps []string
	defer func() {
		for _, p := range temps {
			_ = w.Remove(p)
		}
	}()
	for i, p := range paths {
		old, e := w.Read(p)
		exists := e == nil
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if exists && bytes.Equal(old, contents[i]) {
			continue
		}
		tmp, e := w.Stage(p, contents[i])
		if e != nil {
			return e
		}
		temps = append(temps, tmp)
		it := item{path: p, temp: tmp, existed: exists}
		if exists {
			it.backup, e = w.Stage(p, old)
			if e != nil {
				return e
			}
			temps = append(temps, it.backup)
		}
		items = append(items, it)
	}
	for i, it := range items {
		if e := w.Rename(it.temp, it.path); e != nil {
			var rollback error
			for j := i - 1; j >= 0; j-- {
				prev := items[j]
				var re error
				if prev.existed {
					re = w.Rename(prev.backup, prev.path)
				} else {
					re = w.Remove(prev.path)
				}
				if re != nil {
					rollback = re
				}
			}
			if rollback != nil {
				return &RollbackError{Cause: e, Restore: rollback}
			}
			return e
		}
	}
	return nil
}
