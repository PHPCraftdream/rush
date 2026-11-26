package verifystatic

// Native port of .githooks/check_pkg_names.sh (the unique-package-name
// ratchet). Every non-test, non-main Go package directory's declared
// package name must be unique across the repository. *_test.go files
// (convention, not identity), package main (entry points may share the
// name by design) and anything under a testdata segment are skipped.
//
// RATCHET: .githooks/pkg_names_allowlist.txt lists known collisions as
// "<package-name> <count-of-declaring-directories>". A listed name may
// stay duplicated but fails the moment it GROWS; a name back down to a
// single declaring directory must be removed from the list -- entries
// may only be removed.
//
// Exit-code contract note: the shell collapses its exit-3 broken-input
// cases onto exit 1 at the very end of the script; the native port
// keeps the repository guards' fail-closed contract instead and returns
// broken inputs as errors the verify runner maps to exit 3. Verdict
// classes and messages are identical.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxPackageLine = 200

var packageRe = regexp.MustCompile(`^package[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)

// RunPkgNames ports check_pkg_names.sh. Broken inputs (missing or
// malformed allowlist, unreadable tracked file, missing package clause,
// one directory declaring two names) return an error -> exit 3.
func RunPkgNames(root string, paths []string) (Result, error) {
	started := time.Now()
	r := Result{Name: "pkg-names", Status: "pass"}
	allowed, err := readAllow(root, "pkg_names_allowlist.txt")
	if err != nil {
		return r, err
	}
	pkgname := map[string]string{} // dir -> declared name
	member := map[string]bool{}    // (name, dir) pairs
	ndirs := map[string]int{}      // name -> declaring directories
	names := []string{}            // unique names, discovery order
	dirscount, filesRead := 0, 0
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || hasSegment(p, "testdata") {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if e != nil {
			return r, fmt.Errorf("broken guard input: cannot read tracked file %s: %v", p, e)
		}
		name, found := packageClause(data)
		if !found {
			return r, fmt.Errorf("broken guard input: cannot find a package clause in %s", p)
		}
		filesRead++
		if name == "main" {
			// Deliberate divergence from the shell: a `package main;`
			// line yields the token "main;" in the shell's field extract,
			// so the shell does NOT exempt it; the native regex extracts
			// the identifier "main". The shell's INTENT is kept -- the
			// exemption exists for package main entry points -- so the
			// port keeps exempting it (pinned by the semicolon table
			// test in pkgnames_test.go).
			continue
		}
		label := pathDir(p)
		if label == "" {
			label = "(root)"
		}
		if prev, ok := pkgname[label]; ok && prev != name {
			return r, fmt.Errorf("broken guard input: directory %s declares both %s and %s", label, prev, name)
		}
		if _, ok := pkgname[label]; !ok {
			pkgname[label] = name
			dirscount++
		}
		if !member[name+"\x1c"+label] {
			member[name+"\x1c"+label] = true
			if ndirs[name] == 0 {
				names = append(names, name)
			}
			ndirs[name]++
		}
	}
	if filesRead == 0 {
		return r, fmt.Errorf("broken guard input: pkg-names guard examined no applicable inputs (no tracked non-test .go files)")
	}

	sort.Strings(names)
	type verdict struct{ key, text string }
	var verdicts []verdict
	for _, name := range names {
		c := ndirs[name]
		if prev, ok := allowed[name]; ok {
			if c > 1 {
				if c > prev {
					verdicts = append(verdicts, verdict{name, fmt.Sprintf("RATCHET VIOLATION: \"%s\" is now declared by %d directories (allowlist records %d)", name, c, prev)})
				}
				continue
			}
			verdicts = append(verdicts, verdict{name, fmt.Sprintf("RATCHET CLEANUP: \"%s\" is now unique -- remove it from pkg_names_allowlist.txt. Thanks!", name)})
		} else if c > 1 {
			dirs := make([]string, 0, c)
			for label, n := range pkgname {
				if n == name {
					dirs = append(dirs, label)
				}
			}
			sort.Strings(dirs)
			verdicts = append(verdicts, verdict{name, fmt.Sprintf("DUPLICATE PACKAGE NAME: \"%s\" is declared by %d directories: %s\nrename one of them, or record the CURRENT count as \"%s %d\" in pkg_names_allowlist.txt", name, c, strings.Join(dirs, " "), name, c)})
		}
	}
	kept := 0
	for name, prev := range allowed {
		if ndirs[name] > 1 && ndirs[name] <= prev {
			kept++
		}
	}
	r.Violations = len(verdicts)
	for _, v := range verdicts {
		r.Messages = append(r.Messages, v.text)
	}
	if len(verdicts) > 0 {
		r.Status = "fail"
		r.Excerpt = boundedExcerpt(verdicts[0].text)
	}
	r.Directories = dirscount
	r.Files = filesRead
	r.Summary = fmt.Sprintf("pkg-names guard: %d package dir(s) checked, %d unique name(s), %d allowlisted collision(s)", dirscount, len(names), kept)
	// Stale allowlist entries only warn (never fail), exactly like the
	// shell's stderr note.
	var stale []string
	for name, prev := range allowed {
		if ndirs[name] == 0 {
			stale = append(stale, fmt.Sprintf("  %s %d", name, prev))
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		r.Summary += "\nwarning: stale allowlist entries (package name no longer declared; delete the line):\n" + strings.Join(stale, "\n")
	}
	r.DurationMS = time.Since(started).Milliseconds()
	return r, nil
}

func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// packageClause finds the first `package <ident>` line within the first
// maxPackageLine lines, mirroring the shell's read window (long leading
// doc comments are tolerated; unbounded scans are not).
func packageClause(data []byte) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for i := 0; i < maxPackageLine && sc.Scan(); i++ {
		if m := packageRe.FindStringSubmatch(strings.TrimSuffix(sc.Text(), "\r")); m != nil {
			return m[1], true
		}
	}
	return "", false
}
