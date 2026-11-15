// Package audit is an append-only central journal: one JSON record per
// line, one file per local day, shared safely by concurrent rush processes.
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	maxStringRunes = 300
	maxRecordBytes = 3500
	filePrefix     = "audit-"
	fileSuffix     = ".jsonl"
	dateLayout     = "2006-01-02"
	maxDepth       = 8
)

// autoFields are written by Write itself and are never caller-droppable.
var autoFields = map[string]bool{
	"ts": true, "pid": true, "ppid": true, "parent": true, "launch_cwd": true,
}

// launchCwd is captured during package initialisation, before any
// os.Chdir (internal/cmd's ResolveCwd chdirs the process for --cwd).
var launchCwd = func() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}()

var (
	dirFunc    atomic.Pointer[func() string]
	parentName = sync.OnceValue(func() string { return parentProcessName(os.Getppid()) })
	now        = time.Now
)

// recoverPanic converts a recovered panic into the returned error.
func recoverPanic(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("audit: recovered from panic: %v", r)
	}
}

// LaunchCwd returns the directory the process was started in, or "" if it
// could not be determined.
func LaunchCwd() string { return launchCwd }

// SetDirFunc installs the directory resolver, called lazily at write time.
// Safe for concurrent use; passing nil clears the resolver.
func SetDirFunc(f func() string) {
	if f == nil {
		dirFunc.Store(nil)
		return
	}
	dirFunc.Store(&f)
}

func resolveDir() string {
	if fp := dirFunc.Load(); fp != nil {
		return (*fp)()
	}
	return ""
}

// Event is a free-form record: callers set fields like kind, cmd, flags,
// exit, err, scope, keys, outcome.
type Event map[string]any

// Write appends one JSON record to <dir>/audit-YYYY-MM-DD.jsonl. Caller
// data is normalised and never mutated; every record fits the byte cap.
func Write(ev Event) (err error) {
	defer recoverPanic(&err)
	dir := resolveDir()
	if dir == "" {
		return errors.New("audit: no directory resolver set")
	}
	rec, err := normaliseEvent(ev)
	if err != nil {
		return err
	}
	ts := now()
	rec["ts"] = ts.Format(time.RFC3339Nano)
	rec["pid"] = os.Getpid()
	rec["ppid"] = os.Getppid()
	rec["parent"] = parentName()
	rec["launch_cwd"] = launchCwd
	b, err := boundRecord(rec)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, filePrefix+ts.Format(dateLayout)+fileSuffix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("audit: create %s: %w", dir, err)
	}
	// O_APPEND only, no rotation: several rush processes append to the
	// same file concurrently.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("audit: write %s: %w", path, err)
	}
	return f.Close()
}

// normaliseEvent round-trips the event through JSON so every value
// becomes a plain JSON type and the caller's data is never shared.
func normaliseEvent(ev Event) (map[string]any, error) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal record: %w", err)
	}
	var rec map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep large integers exact
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("audit: normalise record: %w", err)
	}
	if rec == nil {
		rec = make(map[string]any, 6)
	}
	delete(rec, "truncated") // reserved: only boundRecord sets it
	truncateStrings(rec, 0)
	return rec, nil
}

// truncateStrings shortens every string value at any depth; subtrees
// beyond the depth cap are replaced by a placeholder string.
func truncateStrings(v any, depth int) any {
	if depth > maxDepth {
		return "<depth limit>"
	}
	switch t := v.(type) {
	case string:
		return truncateRunes(t, maxStringRunes)
	case map[string]any:
		for k, val := range t {
			t[k] = truncateStrings(val, depth+1)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = truncateStrings(val, depth+1)
		}
		return t
	default:
		return v
	}
}

// truncateRunes cuts s to max runes, reserving the last rune for the
// ellipsis so the result never exceeds max runes.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// boundRecord flags and shrinks the record until the encoded form fits
// the byte cap, dropping whole caller fields by encoded size.
func boundRecord(rec map[string]any) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal record: %w", err)
	}
	if len(b) <= maxRecordBytes {
		return b, nil
	}
	rec["truncated"] = true
	for len(b) > maxRecordBytes && dropLargestCallerField(rec) {
		if b, err = json.Marshal(rec); err != nil {
			return nil, fmt.Errorf("audit: marshal record: %w", err)
		}
	}
	if len(b) > maxRecordBytes {
		// Absurdly long keys: emit only the auto fields plus the flag.
		for k := range rec {
			if !autoFields[k] && k != "truncated" {
				delete(rec, k)
			}
		}
		b, err = json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("audit: marshal record: %w", err)
		}
	}
	return b, nil
}

// dropLargestCallerField removes the non-auto field with the largest
// encoded size, reporting whether one was found.
func dropLargestCallerField(rec map[string]any) bool {
	best := ""
	bestSize := -1
	for k, v := range rec {
		if autoFields[k] || k == "truncated" {
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			continue
		}
		if len(enc) > bestSize {
			best, bestSize = k, len(enc)
		}
	}
	if bestSize < 0 {
		return false
	}
	delete(rec, best)
	return true
}

// Files returns sorted audit-*.jsonl paths, or (nil, nil) without a resolver
// or when the directory does not exist yet.
func Files() (paths []string, err error) {
	defer recoverPanic(&err)
	dir := resolveDir()
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) && dirIsAbsent(dir) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

// Prune deletes dated audit files older than now-olderThan; olderThan
// <= 0 deletes every dated file. A missing directory is not an error;
// removal failures are skipped, not fatal.
func Prune(olderThan time.Duration) (removed int, err error) {
	defer recoverPanic(&err)
	dir := resolveDir()
	if dir == "" {
		return 0, errors.New("audit: no directory resolver set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) && dirIsAbsent(dir) {
			return 0, nil
		}
		return 0, fmt.Errorf("audit: read %s: %w", dir, err)
	}
	cutoff := now().Add(-olderThan).Format(dateLayout)
	for _, e := range entries {
		name := e.Name()
		date, ok := fileDate(name)
		if !ok {
			continue
		}
		if olderThan > 0 && date >= cutoff { // zero-padded dates compare lexically
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, name)); rmErr != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

// dirIsAbsent reports that the path itself does not exist at all; a
// path that exists as a file must stay an error in the callers.
func dirIsAbsent(dir string) bool {
	_, err := os.Stat(dir)
	return os.IsNotExist(err)
}

func fileDate(name string) (string, bool) {
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return "", false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	if len(date) != len(dateLayout) {
		return "", false
	}
	if _, err := time.ParseInLocation(dateLayout, date, time.Local); err != nil {
		return "", false
	}
	return date, true
}
