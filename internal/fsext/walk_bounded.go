package fsext

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// Package-level defaults so tests can shrink them.
var (
	BoundedWalkMaxEntries = 200000
	BoundedWalkTimeout    = 60 * time.Second
)

// WalkStopReason explains why a bounded walk ended.
type WalkStopReason int

const (
	WalkCompleted      WalkStopReason = iota // finished the whole subtree
	WalkStoppedEntries                       // visited-entry budget exhausted
	WalkStoppedTime                          // wall-clock budget exhausted
	WalkStoppedContext                       // ctx cancelled or deadline exceeded
)

// String returns a short, stable identifier for the stop reason.
func (r WalkStopReason) String() string {
	switch r {
	case WalkStoppedEntries:
		return "budget-entries"
	case WalkStoppedTime:
		return "budget-time"
	case WalkStoppedContext:
		return "ctx"
	default:
		return "completed"
	}
}

// PartialNote renders the user-facing incompleteness note used by all tools.
func PartialNote(entries int, duration time.Duration, dir string) string {
	return fmt.Sprintf("(search stopped after %d entries / %s under %s: results are incomplete; narrow path or pattern)", entries, duration, dir)
}

// BoundedWalkLimits holds the resource budgets for a bounded walk.
type BoundedWalkLimits struct {
	MaxEntries int           // <=0 means use BoundedWalkMaxEntries
	Timeout    time.Duration // <=0 means use BoundedWalkTimeout
}

// BoundedWalkOptions configures WalkDirBounded.
type BoundedWalkOptions struct {
	MaxDepth int               // 0 or negative = unlimited; direct children of start are depth 1; when a dir is AT MaxDepth, do not descend into it
	Limits   BoundedWalkLimits //
	// Visited is optional; when non-nil it is set to the number of entries read.
	Visited *int
}

// BoundedWalkFunc mirrors fs.WalkDirFunc: return fs.SkipDir to skip a
// directory's contents, fs.SkipAll to abort the walk silently (WalkCompleted
// is still returned, with no error), any other error aborts the walk and is
// returned.
type BoundedWalkFunc func(path string, d fs.DirEntry, err error) error

// boundedWalkBatchSize is the number of entries read per ReadDir call.
const boundedWalkBatchSize = 256

// WalkDirBounded walks fsys from start, reading directories in batches of
// ~256 entries via fs.ReadDirFile.ReadDir(n) so one huge directory is also
// bounded. It checks ctx, the deadline and the visited-entry count between
// batches and between entries. Entries read from ReadDir count against the
// entry budget even if they are later skipped. Any error returned by fn other
// than fs.SkipDir/fs.SkipAll aborts the walk and is reported with reason
// WalkCompleted. Returns the stop reason and error.
func WalkDirBounded(ctx context.Context, fsys fs.FS, start string, opts BoundedWalkOptions, fn BoundedWalkFunc) (WalkStopReason, error) {
	maxEntries := opts.Limits.MaxEntries
	if maxEntries <= 0 {
		maxEntries = BoundedWalkMaxEntries
	}
	timeout := opts.Limits.Timeout
	if timeout <= 0 {
		timeout = BoundedWalkTimeout
	}
	deadline := time.Now().Add(timeout)

	w := &boundedWalker{
		ctx:        ctx,
		fsys:       fsys,
		fn:         fn,
		maxEntries: maxEntries,
		deadline:   deadline,
		maxDepth:   opts.MaxDepth,
	}
	err := w.walk(start, 1)
	if opts.Visited != nil {
		*opts.Visited = w.visited
	}
	return w.reason, err
}

type boundedWalker struct {
	ctx        context.Context
	fsys       fs.FS
	fn         BoundedWalkFunc
	maxEntries int
	deadline   time.Time
	maxDepth   int
	visited    int
	reason     WalkStopReason
}

// check verifies the context, deadline and entry budget; it reports which
// budget stopped the walk via the returned reason and whether to stop.
func (w *boundedWalker) check() (WalkStopReason, error, bool) {
	if err := w.ctx.Err(); err != nil {
		return WalkStoppedContext, err, true
	}
	if !w.deadline.IsZero() && time.Now().After(w.deadline) {
		return WalkStoppedTime, nil, true
	}
	if w.visited >= w.maxEntries {
		return WalkStoppedEntries, nil, true
	}
	return WalkCompleted, nil, false
}

// stop records the stop reason for the final result.
func (w *boundedWalker) stop(reason WalkStopReason) {
	w.reason = reason
}

func (w *boundedWalker) walk(start string, depth int) error {
	info, statErr := fs.Stat(w.fsys, start)
	if statErr != nil {
		// Start path does not exist (or is unreadable): visit nothing.
		return nil
	}
	if reason, err, stop := w.check(); stop {
		w.stop(reason)
		return err
	}
	// Visit the start entry itself, like fs.WalkDir does.
	if err := w.fn(start, entryFromInfo(start, info), nil); err != nil {
		if err == fs.SkipAll {
			return nil
		}
		if err != fs.SkipDir && !info.IsDir() {
			return err
		}
		// fs.SkipDir on the start directory: nothing to descend into.
		if err != fs.SkipDir {
			return err
		}
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	if w.maxDepth > 0 && depth > w.maxDepth {
		return nil
	}
	return w.walkDir(start, depth)
}

func (w *boundedWalker) walkDir(dir string, depth int) error {
	f, err := w.fsys.Open(dir)
	if err != nil {
		if err := w.fn(dir, nil, err); err != nil {
			if err == fs.SkipAll {
				return nil
			}
			return err
		}
		return nil
	}
	dirs, ok := f.(fs.ReadDirFile)
	if !ok {
		f.Close()
		return w.readDirFallback(dir, depth)
	}
	defer f.Close()
	for {
		if reason, err, stop := w.check(); stop {
			w.stop(reason)
			return err
		}
		batch, err := dirs.ReadDir(boundedWalkBatchSize)
		if err != nil {
			if err := w.fn(dir, nil, err); err != nil {
				if err == fs.SkipAll {
					return fs.SkipAll
				}
				return err
			}
			return nil
		}
		// fs.WalkDir sorts entries. Keep identical order for ordinary
		// (small) directories by sorting a first batch that came back
		// complete-but-small. Larger directories get per-batch sorting;
		// callers do not rely on cross-batch ordering for huge directories.
		sort.Slice(batch, func(i, j int) bool { return batch[i].Name() < batch[j].Name() })
		for _, e := range batch {
			// Count every entry read from ReadDir against the budget,
			// including entries later skipped.
			w.visited++
			if name := e.Name(); name == "." || name == ".." {
				continue
			}
			if reason, err, stop := w.check(); stop {
				w.stop(reason)
				return err
			}
			child := joinPath(dir, e.Name())
			if err := w.fn(child, e, nil); err != nil {
				if err == fs.SkipAll {
					return nil
				}
				if err == fs.SkipDir {
					continue
				}
				return err
			}
			if e.IsDir() {
				if w.maxDepth > 0 && depth+1 > w.maxDepth {
					continue
				}
				if err := w.walkDir(child, depth+1); err != nil {
					return err
				}
			}
		}
		if len(batch) < boundedWalkBatchSize {
			return nil
		}
	}
}

func (w *boundedWalker) readDirFallback(dir string, depth int) error {
	entries, err := fs.ReadDir(w.fsys, dir)
	if err != nil {
		if err := w.fn(dir, nil, err); err != nil {
			if err == fs.SkipAll {
				return nil
			}
			return err
		}
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		w.visited++
		if name := e.Name(); name == "." || name == ".." {
			continue
		}
		if reason, err, stop := w.check(); stop {
			w.stop(reason)
			return err
		}
		child := joinPath(dir, e.Name())
		if err := w.fn(child, e, nil); err != nil {
			if err == fs.SkipAll {
				return nil
			}
			if err == fs.SkipDir {
				continue
			}
			return err
		}
		if e.IsDir() {
			if w.maxDepth > 0 && depth+1 > w.maxDepth {
				continue
			}
			if err := w.walkDir(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// joinPath joins dir and name with "/" in fs.FS path space.
func joinPath(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return dir + "/" + name
}

// entryFromInfo adapts an fs.FileInfo to an fs.DirEntry.
type infoDirEntry struct {
	info fs.FileInfo
}

func (e infoDirEntry) Name() string               { return e.info.Name() }
func (e infoDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e infoDirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e infoDirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

func entryFromInfo(path string, info fs.FileInfo) fs.DirEntry {
	return infoDirEntry{info: info}
}

// globPatternBase splits pattern into its literal base directory (segments
// before the first segment containing meta characters) and the remaining
// pattern relative to that base. pattern must use forward slashes.
func globPatternBase(pattern string) (base, rest string) {
	const meta = "*?[{"
	segments := splitPath(pattern)
	i := 0
	for ; i < len(segments); i++ {
		if strings.ContainsAny(segments[i], meta) {
			break
		}
	}
	base = strings.Join(segments[:i], "/")
	rest = strings.Join(segments[i:], "/")
	// A fully literal pattern forms the base with an empty rest.
	if i == len(segments) {
		return pattern, ""
	}
	// Strip a trailing "/" from base if the pattern was "a/b/".
	base = strings.TrimSuffix(base, "/")
	return base, rest
}

// splitPath splits a forward-slash pattern into non-empty segments, keeping
// an empty rest distinguishable.
func splitPath(pattern string) []string {
	var segments []string
	for len(pattern) > 0 {
		i := strings.IndexByte(pattern, '/')
		if i < 0 {
			segments = append(segments, pattern)
			break
		}
		if i > 0 {
			segments = append(segments, pattern[:i])
		}
		pattern = pattern[i+1:]
	}
	return segments
}

// hasDoubleStar reports whether the pattern contains "**".
func hasDoubleStar(pattern string) bool {
	return strings.Contains(pattern, "**")
}

// globMaxDepth returns the depth cap for a pattern without "**": the number
// of segments in the pattern relative to its base. Returns 0 (unlimited) for
// "**" patterns or when rest is "" (the base itself matched).
func globMaxDepth(base, rest string) int {
	if hasDoubleStar(rest) {
		return 0
	}
	if rest == "" {
		return 0
	}
	return len(splitPath(rest))
}
