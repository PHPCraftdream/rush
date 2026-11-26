package verifystatic

// Native port of .githooks/check_layers.sh (the internal layer-direction
// ratchet kit < store < session < config < agent < app < serve).
//
// Data source is `go list -json ./...` (Imports, TestImports and
// XTestImports) plus `go list -m` for the module path, both run through
// the platform.Command-based invoke callback supplied by the verify
// runner, so every spawn stays hardened and recorded in provenance.
//
// LOCK NOTE: the shell guard wraps go in the machine-local lock wrapper
// when it exists. The Go port deliberately does NOT acquire the
// go-build machinelock slot: the tests of this very package are run
// under `rush lock run go-build`, so a nested acquisition of the same
// slot would deadlock against ourselves, and `go list` is a light
// metadata query, not a build. The hardened environment (GOFLAGS unset,
// GOMEMLIMIT set, GOTOOLCHAIN inherited exactly as the shell inherits
// it) is applied instead.
//
// RATCHET: .githooks/layer_exceptions.txt ("<package-import-path>
// <count>") and .githooks/layer_test_exceptions.txt ("<repo-relative
// test file> <count>") must hold only live debt. A drop to zero, a
// stale entry, or growth past the recorded count FAILS the guard --
// deliberately stricter than the sibling guards, which only warn.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// InvokeFunc runs one argv in cwd with an optional environment override
// and returns stdout; it records provenance for every command that
// actually started. It is supplied by the verify runner.
type InvokeFunc func(cwd string, env []string, args ...string) ([]byte, error)

// GoEnv returns the hardened environment for child Go commands: GOFLAGS
// is removed (a caller's -p or tags must not leak in), GOMEMLIMIT is
// capped, and GOTOOLCHAIN is inherited untouched, mirroring the shell
// guard, which never overrides it.
func GoEnv() []string {
	out := make([]string, 0, 32)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOFLAGS=") {
			continue
		}
		out = append(out, kv)
	}
	// GOFLAGS= (empty) is appended deliberately rather than dropping the
	// variable entirely: an empty value also overrides flags saved with
	// `go env -w` in Go's environment file, which deleting the variable
	// would leave in force.
	return append(out, "GOFLAGS=", "GOMEMLIMIT=700MiB")
}

var layerGroups = map[string]int{
	"kit": 0, "store": 1, "session": 2, "config": 3,
	"agent": 4, "app": 5, "serve": 6,
}

const layerChain = "kit < store < session < config < agent < app < serve"

type goListPackage struct {
	ImportPath   string
	Dir          string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// RunLayers ports check_layers.sh. Broken inputs (go list failure,
// unknown internal group, malformed ratchet list) return an error, which
// the verify runner maps to exit 3.
func RunLayers(root string, invoke InvokeFunc) (Result, error) {
	started := time.Now()
	r := Result{Name: "layers", Status: "pass"}
	moduleBytes, err := invoke(root, GoEnv(), "go", "list", "-m")
	if err != nil {
		return r, fmt.Errorf("broken guard input: go list -m failed: %s %s", boundedExcerpt(string(moduleBytes)), boundedExcerpt(err.Error()))
	}
	module := strings.TrimSuffix(strings.TrimSuffix(string(moduleBytes), "\r"), "\n")
	module = strings.TrimSuffix(module, "\r")
	if module == "" {
		return r, errors.New("broken guard input: go list -m returned no module path")
	}
	listBytes, err := invoke(root, GoEnv(), "go", "list", "-json", "./...")
	if err != nil {
		return r, fmt.Errorf("broken guard input: go list -json failed: %s %s", boundedExcerpt(string(listBytes)), boundedExcerpt(err.Error()))
	}
	prodExc, err := readAllow(root, "layer_exceptions.txt")
	if err != nil {
		return r, err
	}
	testExc, err := readAllow(root, "layer_test_exceptions.txt")
	if err != nil {
		return r, err
	}

	prefix := module + "/internal/"
	rankOf := func(p string) (int, string, bool) {
		if !strings.HasPrefix(p, prefix) {
			return -1, "", false
		}
		rel := p[len(prefix):]
		if rel == "app/itest" || strings.HasPrefix(rel, "app/itest/") {
			return -1, "", false
		}
		group := rel
		if i := strings.Index(rel, "/"); i > 0 {
			group = rel[:i]
		}
		rank, ok := layerGroups[group]
		if !ok {
			return -2, group, true
		}
		return rank, group, false
	}

	var pkgs []goListPackage
	dec := json.NewDecoder(strings.NewReader(string(listBytes)))
	for dec.More() {
		var p goListPackage
		if e := dec.Decode(&p); e != nil {
			return r, fmt.Errorf("broken guard input: cannot parse go list output: %v", e)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		return r, errors.New("broken guard input: go list -json examined no packages")
	}

	seen := map[string]bool{}
	pint := map[string]int{}
	pgrp := map[string]string{}
	pkgdirseen := map[string]bool{}
	pkgdir := map[string]string{}
	npkg, nint := 0, 0
	prodEdges := map[string][]string{}
	prodEdgeGroups := map[string][]string{}
	testFlag := map[string]bool{}
	for _, p := range pkgs {
		rank, group, unknown := rankOf(p.ImportPath)
		if unknown {
			return r, fmt.Errorf("broken guard input: package %s is under internal/%s/ which is not one of the seven layer groups (%s)", p.ImportPath, group, layerChain)
		}
		seen[p.ImportPath] = true
		npkg++
		pint[p.ImportPath] = rank
		pgrp[p.ImportPath] = group
		if rank < 0 {
			continue
		}
		nint++
		dir := relDir(root, p.Dir)
		pkgdir[p.ImportPath] = dir
		pkgdirseen[dir] = true
		distinct := map[string]bool{}
		for _, imp := range p.Imports {
			tr, tg, _ := rankOf(imp)
			if tr <= rank {
				continue
			}
			if !distinct[imp] {
				distinct[imp] = true
				prodEdges[p.ImportPath] = append(prodEdges[p.ImportPath], imp)
				prodEdgeGroups[p.ImportPath] = append(prodEdgeGroups[p.ImportPath], tg)
			}
		}
		if p.testUpward(rank, rankOf) {
			testFlag[p.ImportPath] = true
		}
	}

	// Per-file test scan, only for internal packages whose aggregated
	// test imports already point upward (mirrors scanpkgtests).
	fgrp := map[string]string{}
	fviol := map[string][]string{}
	fviolGroups := map[string][]string{}
	fviolCount := map[string]int{}
	seentestf := map[string]bool{}
	quoted := regexp.MustCompile(`"[^"]+"`)
	reBlockEnd := regexp.MustCompile(`^[ \t]*\)`)
	reComment := regexp.MustCompile(`^[ \t]*//`)
	reImportOpen := regexp.MustCompile(`^[ \t]*import[ \t]*\(`)
	reImport := regexp.MustCompile(`^[ \t]*import`)
	for _, ip := range sortedKeys(seen) {
		if pint[ip] < 0 || !testFlag[ip] {
			continue
		}
		dir := pkgdir[ip]
		entries, e := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if e != nil {
			return r, fmt.Errorf("broken guard input: cannot read package dir %s: %v", dir, e)
		}
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasSuffix(name, "_test.go") {
				continue
			}
			relf := name
			if dir != "" {
				relf = dir + "/" + name
			}
			seentestf[relf] = true
			fgrp[relf] = pgrp[ip]
			data, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(relf)))
			if e != nil {
				return r, fmt.Errorf("broken guard input: cannot read test file %s: %v", relf, e)
			}
			var imps []string
			seenImp := map[string]bool{}
			inBlock := false
			for _, ln := range strings.Split(string(data), "\n") {
				ln = strings.TrimSuffix(ln, "\r")
				if inBlock {
					if reBlockEnd.MatchString(ln) {
						inBlock = false
						continue
					}
					if reComment.MatchString(ln) {
						continue
					}
					if m := quoted.FindString(ln); m != "" {
						recordImp(m[1:len(m)-1], seenImp, &imps)
					}
					continue
				}
				if reImportOpen.MatchString(ln) {
					inBlock = true
					continue
				}
				if reImport.MatchString(ln) {
					if m := quoted.FindString(ln); m != "" {
						recordImp(m[1:len(m)-1], seenImp, &imps)
					}
				}
			}
			var up, upg []string
			for _, imp := range imps {
				gr, gg, _ := rankOf(imp)
				if gr < 0 || gr <= pint[ip] {
					continue
				}
				up = append(up, imp)
				upg = append(upg, gg)
			}
			if len(up) > 0 {
				fviol[relf] = up
				fviolGroups[relf] = upg
				fviolCount[relf] = len(up)
			}
		}
	}

	// Verdicts (same classes as the shell: LAYER VIOLATION, TEST LAYER
	// VIOLATION, RATCHET VIOLATION, RATCHET CLEANUP, STALE EXCEPTION).
	type verdict struct{ key, text string }
	var verdicts []verdict
	add := func(key, text string) { verdicts = append(verdicts, verdict{key, text}) }
	sub := "\x1c" // awk SUBSEP equivalent, keeps the shell's sort order
	keptp, keptt := 0, 0
	for _, ip := range sortedKeys(seen) {
		if pint[ip] < 0 {
			continue
		}
		pv := len(prodEdges[ip])
		if prev, ok := prodExc[ip]; ok {
			if pv == 0 {
				add(ip, fmt.Sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from layer_exceptions.txt; the list must hold only live debt", ip))
			} else if pv > prev {
				add(ip, fmt.Sprintf("RATCHET VIOLATION: %s now violates with %d import(s) (recorded %d)", ip, pv, prev))
			} else {
				keptp += pv
			}
		} else if pv > 0 {
			for i, imp := range prodEdges[ip] {
				add(ip+sub+imp, fmt.Sprintf("LAYER VIOLATION: %s (group %s) imports %s (group %s)", ip, pgrp[ip], imp, prodEdgeGroups[ip][i]))
			}
			add(ip+sub+"fix", fmt.Sprintf("fix the import, or record the CURRENT count in layer_exceptions.txt as \"%s %d\" with a one-line reason", ip, pv))
		}
	}
	for _, p := range sortedKeysMap(prodExc) {
		if !seen[p] {
			add(p, fmt.Sprintf("STALE EXCEPTION: %s no longer exists -- remove it from layer_exceptions.txt; the list must hold only live debt", p))
		}
	}
	for _, f := range sortedKeysMap(fviolCount) {
		if _, ok := testExc[f]; ok {
			continue
		}
		for i, imp := range fviol[f] {
			add(f+sub+imp, fmt.Sprintf("TEST LAYER VIOLATION: %s imports %s (group %s test may not import group %s)", f, imp, fgrp[f], fviolGroups[f][i]))
		}
		add(f+sub+"fix", fmt.Sprintf("fix the test, or record it in layer_test_exceptions.txt as \"%s %d\"", f, fviolCount[f]))
	}
	for _, f := range sortedKeysMap(testExc) {
		if _, scanned := seentestf[f]; scanned {
			c := fviolCount[f]
			if c == 0 {
				add(f, fmt.Sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from layer_test_exceptions.txt; the list must hold only live debt", f))
			} else if c > testExc[f] {
				add(f, fmt.Sprintf("RATCHET VIOLATION: %s now violates with %d import(s) (recorded %d)", f, c, testExc[f]))
			} else {
				keptt++
			}
			continue
		}
		if pkgdirseen[pathDir(f)] {
			add(f, fmt.Sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from layer_test_exceptions.txt; the list must hold only live debt", f))
		} else {
			add(f, fmt.Sprintf("STALE EXCEPTION: %s no longer exists -- remove it from layer_test_exceptions.txt; the list must hold only live debt", f))
		}
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].key < verdicts[j].key })
	r.Violations = len(verdicts)
	for _, v := range verdicts {
		r.Messages = append(r.Messages, v.text)
	}
	if len(verdicts) > 0 {
		r.Status = "fail"
		r.Excerpt = boundedExcerpt(verdicts[0].text)
	}
	r.Directories = nint
	r.Files = npkg
	r.Summary = fmt.Sprintf("layer guard: %d package(s) checked, %d internal package(s) checked, %d production edge(s) allowlisted (kept), %d test file(s) allowlisted (kept); %s", npkg, nint, keptp, keptt, layerChain)
	r.DurationMS = time.Since(started).Milliseconds()
	return r, nil
}

func recordImp(imp string, seen map[string]bool, out *[]string) {
	if !seen[imp] {
		seen[imp] = true
		*out = append(*out, imp)
	}
}

// testUpward reports whether the aggregated test imports of the package
// point to a strictly higher group.
func (p goListPackage) testUpward(rank int, rankOf func(string) (int, string, bool)) bool {
	for _, imp := range append(append([]string{}, p.TestImports...), p.XTestImports...) {
		if tr, _, _ := rankOf(imp); tr > rank {
			return true
		}
	}
	return false
}

func relDir(root, dir string) string {
	d := filepath.ToSlash(dir)
	r := filepath.ToSlash(root)
	if d == r {
		return ""
	}
	if strings.HasPrefix(d, r+"/") {
		return d[len(r)+1:]
	}
	return d
}

func pathDir(f string) string {
	if i := strings.LastIndex(f, "/"); i > 0 {
		return f[:i]
	}
	return ""
}

func sortedKeys(m map[string]bool) []string { return sortedKeysMap(m) }

func sortedKeysMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
