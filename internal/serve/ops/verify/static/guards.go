package verifystatic

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Name, Status, Summary, Excerpt string
	Directories, Files, Violations int
	DurationMS                     int64
}
type sourceFile struct {
	lines     int
	generated bool
}

func Run(root string, paths []string, countTests bool) ([]Result, error) {
	entry, err := readAllow(root, "dir_entries_allowlist.txt")
	if err != nil {
		return nil, err
	}
	dirFiles, err := readAllow(root, "dir_files_allowlist.txt")
	if err != nil {
		return nil, err
	}
	size1000, err := readAllow(root, "file_size_allowlist.txt")
	if err != nil {
		return nil, err
	}
	size900, err := readAllow(root, "file_size_900_allowlist.txt")
	if err != nil {
		return nil, err
	}
	all := map[string]sourceFile{}
	for _, p := range paths {
		if !source(p) {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if e != nil {
			return nil, fmt.Errorf("cannot read tracked file %q: %w", p, e)
		}
		text := string(data)
		lines := 0
		if len(data) > 0 {
			lines = strings.Count(text, "\n")
			if data[len(data)-1] != '\n' {
				lines++
			}
		}
		gen := false
		lineCount := 0
		for len(data) > 0 && lineCount < 10 {
			i := strings.IndexByte(string(data), '\n')
			line := string(data)
			if i >= 0 {
				line = string(data[:i])
				data = data[i+1:]
			} else {
				data = nil
			}
			lineCount++
			if strings.Contains(line, "Code generated") || strings.Contains(line, "DO NOT EDIT") {
				gen = true
			}
		}
		all[p] = sourceFile{lines, gen}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no tracked source inputs")
	}
	return []Result{
		directoryGuard("dir-entries", all, entry, 7, countTests),
		directoryGuard("dir-files", all, dirFiles, 21, true),
		sizeGuard("file-size", all, size1000, 1000),
		sizeGuard("file-size-900", all, size900, 900),
	}, nil
}

func source(p string) bool {
	return strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".tsx")
}

func ignored(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == "testdata" || s == "node_modules" || s == "dist" {
			return true
		}
	}
	return false
}

func isTest(p string) bool {
	return strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".test.ts") || strings.HasSuffix(p, ".test.tsx") || strings.HasSuffix(p, ".spec.ts") || strings.HasSuffix(p, ".spec.tsx")
}

func directoryGuard(name string, all map[string]sourceFile, allow map[string]int, limit int, countTests bool) Result {
	started := time.Now()
	r := Result{Name: name, Status: "pass"}
	dirs := map[string]bool{}
	counts := map[string]int{}
	generated := 0
	fileCount := 0
	for _, p := range sorted(all) {
		f := all[p]
		if ignored(p) {
			continue
		}
		parts := strings.Split(p, "/")
		parent := strings.Join(parts[:len(parts)-1], "/")
		if parent == "" {
			continue
		}
		if name == "dir-entries" {
			for i := 1; i < len(parts); i++ {
				d := strings.Join(parts[:i], "/")
				if !dirs[d] {
					dirs[d] = true
					if i > 1 {
						counts[strings.Join(parts[:i-1], "/")]++
					}
				}
			}
		} else {
			dirs[parent] = true
		}
		if name == "dir-entries" && !countTests && isTest(p) {
			continue
		}
		if name == "dir-entries" && isTest(p) {
			dirs[parent] = true
		}
		if f.generated {
			generated++
			continue
		}
		counts[parent]++
		if countTests || !isTest(p) {
			fileCount++
		}
	}
	keys := make([]string, 0, len(dirs))
	for d := range dirs {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	r.Directories = len(keys)
	for _, d := range keys {
		n := counts[d]
		if n > limit {
			if name == "dir-entries" {
				if previous, ok := allow[d]; !ok {
					r.add(fmt.Sprintf("TOO MANY ENTRIES: %s has %d elements (max %d)", d, n, limit))
				} else if n > previous {
					r.add(fmt.Sprintf("RATCHET VIOLATION: %s grew to %d elements (allowlist records %d)", d, n, previous))
				}
			} else if previous, ok := allow[d]; !ok {
				r.add(fmt.Sprintf("TOO MANY FILES: %s has %d code files (max %d)", d, n, limit))
			} else if n > previous {
				r.add(fmt.Sprintf("RATCHET VIOLATION: %s grew to %d code files (allowlist records %d)", d, n, previous))
			}
		} else if _, ok := allow[d]; ok {
			if name == "dir-entries" {
				r.add(fmt.Sprintf("RATCHET CLEANUP: %s is now %d elements (max %d); remove it from allowlist", d, n, limit))
			} else {
				r.add(fmt.Sprintf("RATCHET CLEANUP: %s is now %d code files (max %d); remove it from allowlist", d, n, limit))
			}
		}
	}
	r.Files = fileCount
	if name == "dir-entries" && countTests {
		r.Summary = fmt.Sprintf("%d code directories checked, %d element file(s) counted, %d generated skipped (test files counted)", r.Directories, r.Files, generated)
	} else {
		r.Summary = fmt.Sprintf("%d code directories checked, %d element file(s) counted, %d generated skipped", r.Directories, r.Files, generated)
	}
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}

func sizeGuard(name string, all map[string]sourceFile, allow map[string]int, limit int) Result {
	started := time.Now()
	r := Result{Name: name, Status: "pass"}
	generated, kept := 0, 0
	for _, p := range sorted(all) {
		f := all[p]
		r.Files++
		if f.generated {
			generated++
			continue
		}
		if previous, ok := allow[p]; ok {
			if f.lines > previous {
				if limit == 1000 {
					r.add(fmt.Sprintf("RATCHET VIOLATION: %s grew to %d lines (allowlist records %d)", p, f.lines, previous))
				} else {
					r.add(fmt.Sprintf("RATCHET VIOLATION: %s grew to %d lines (allowlist records %d)", p, f.lines, previous))
				}
			} else if f.lines <= limit {
				r.add(fmt.Sprintf("RATCHET CLEANUP: %s is now %d lines; remove it from allowlist", p, f.lines))
			} else {
				kept++
			}
		} else if f.lines > limit {
			message := fmt.Sprintf("OVER %d: %s is %d lines", limit, p, f.lines)
			if limit == 1000 {
				message = fmt.Sprintf("TOO LONG: %s is %d lines", p, f.lines)
			} else if limit == 900 {
				message = fmt.Sprintf("OVER 900: %s is %d lines", p, f.lines)
			}
			r.add(message)
		}
	}
	r.Summary = fmt.Sprintf("%d file(s) checked, %d generated skipped, %d allowlisted over %d lines, max %d", r.Files, generated, kept, limit, limit)
	if limit == 1000 {
		warnings := make([]sourceFileEntry, 0)
		for p, f := range all {
			if !f.generated && f.lines > 900 && f.lines <= 1000 {
				warnings = append(warnings, sourceFileEntry{p, f.lines})
			}
		}
		sort.Slice(warnings, func(i, j int) bool {
			if warnings[i].lines == warnings[j].lines {
				return warnings[i].path < warnings[j].path
			}
			return warnings[i].lines > warnings[j].lines
		})
		r.Summary += fmt.Sprintf("\n%d hand-written file(s) over 900 lines (warning, limit 1000)", len(warnings))
		for i, w := range warnings {
			if i == 5 {
				break
			}
			r.Summary += fmt.Sprintf("\n  %d %s", w.lines, w.path)
		}
	}
	if limit == 900 {
		stale := make([]string, 0)
		for p, n := range allow {
			if _, ok := all[p]; !ok {
				stale = append(stale, fmt.Sprintf("%s %d", p, n))
			}
		}
		sort.Strings(stale)
		if len(stale) > 0 {
			r.Summary += "\nwarning: stale allowlist entries (path no longer tracked; delete the line):\n  " + strings.Join(stale, "\n  ")
			if r.Excerpt == "" {
				r.Excerpt = boundedExcerpt(r.Summary[strings.Index(r.Summary, "warning: stale"):])
			}
		}
	}
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}

type sourceFileEntry struct {
	path  string
	lines int
}

func boundedExcerpt(s string) string {
	const limit = 4096
	const marker = "\n…[truncated]"
	if len(s) <= limit {
		return s
	}
	return s[:limit-len(marker)] + marker
}

func (r *Result) add(msg string) {
	r.Violations++
	r.Status = "fail"
	if r.Excerpt == "" {
		r.Excerpt = boundedExcerpt(msg)
	}
}

func sorted(m map[string]sourceFile) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func readAllow(root, name string) (map[string]int, error) {
	path := filepath.Join(root, ".githooks", name)
	f, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("broken guard input %s: %w", path, e)
	}
	defer f.Close()
	out := map[string]int{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSuffix(s.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexAny(line, " \t")
		if i <= 0 {
			return nil, fmt.Errorf("broken guard input %s: malformed entry %q", path, line)
		}
		countText := strings.TrimSpace(line[i:])
		if countText == "" {
			return nil, fmt.Errorf("broken guard input %s: invalid count", path)
		}
		for _, c := range countText {
			if c < '0' || c > '9' {
				return nil, fmt.Errorf("broken guard input %s: invalid count", path)
			}
		}
		n, e := strconv.Atoi(countText)
		if e != nil {
			return nil, fmt.Errorf("broken guard input %s: invalid count", path)
		}
		key := strings.TrimRight(line[:i], " \t")
		if key == "" {
			return nil, fmt.Errorf("broken guard input %s: malformed entry %q", path, line)
		}
		if _, ok := out[key]; ok {
			return nil, fmt.Errorf("broken guard input %s: duplicate %s", path, key)
		}
		out[key] = n
	}
	if e := s.Err(); e != nil {
		return nil, e
	}
	return out, nil
}
