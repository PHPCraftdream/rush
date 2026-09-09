package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"container/heap"
	"container/list"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

const regexCacheCapacity = 128

// regexCache is a bounded LRU of compiled patterns. Invalid patterns are not
// retained; concurrent callers share one in-flight compilation instead.
type regexCache struct {
	mu         sync.Mutex
	capacity   int
	entries    map[string]*list.Element
	lru        *list.List
	pending    map[string]*regexCompile
	generation uint64
}

type regexCacheEntry struct {
	key string
	re  *regexp.Regexp
}

type regexCompile struct {
	done chan struct{}
	re   *regexp.Regexp
	err  error
}

// newRegexCache creates a cache using regexCacheCapacity, or a requested
// capacity for deterministic tests. Capacity zero disables retention.
func newRegexCache(capacities ...int) *regexCache {
	capacity := regexCacheCapacity
	if len(capacities) > 0 {
		capacity = max(0, capacities[0])
	}
	return &regexCache{
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		pending:  make(map[string]*regexCompile),
	}
}

// get retrieves a compiled regex from the cache or compiles and caches it.
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	rc.mu.Lock()
	if element := rc.entries[pattern]; element != nil {
		rc.lru.MoveToFront(element)
		re := element.Value.(regexCacheEntry).re
		rc.mu.Unlock()
		return re, nil
	}
	if pending := rc.pending[pattern]; pending != nil {
		rc.mu.Unlock()
		<-pending.done
		return pending.re, pending.err
	}
	pending := &regexCompile{done: make(chan struct{})}
	rc.pending[pattern] = pending
	generation := rc.generation
	rc.mu.Unlock()

	re, err := regexp.Compile(pattern)

	rc.mu.Lock()
	pending.re, pending.err = re, err
	delete(rc.pending, pattern)
	if err == nil && rc.capacity > 0 && generation == rc.generation {
		element := rc.lru.PushFront(regexCacheEntry{key: pattern, re: re})
		rc.entries[pattern] = element
		for rc.lru.Len() > rc.capacity {
			element := rc.lru.Back()
			delete(rc.entries, element.Value.(regexCacheEntry).key)
			rc.lru.Remove(element)
		}
	}
	close(pending.done)
	rc.mu.Unlock()
	return re, err
}

// ResetCache clears compiled regex caches. In-flight compilations complete for
// their current callers but are not inserted into a cache reset meanwhile.
func ResetCache() {
	searchRegexCache.reset()
	globRegexCache.reset()
}

func (rc *regexCache) reset() {
	rc.mu.Lock()
	rc.entries = make(map[string]*list.Element)
	rc.lru.Init()
	rc.generation++
	rc.mu.Unlock()
}

// Global regex cache instances
var (
	searchRegexCache = newRegexCache()
	globRegexCache   = newRegexCache()
	// Pre-compiled regex for glob conversion (used frequently)
	globBraceRegex = regexp.MustCompile(`\{([^}]+)\}`)
)

type GrepParams struct {
	Pattern     string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path        string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include     string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
}

type grepMatch struct {
	path     string
	modTime  time.Time
	lineNum  int
	charNum  int
	lineText string
	seq      int64 // insertion order, for stable sort tie-breaking
}

const ripgrepStatCacheCapacity = 128

type statCacheEntry struct {
	path string
	info os.FileInfo
}

// boundedStatCache keeps the one-stat-per-active-file optimization without
// retaining a path for every file in a high-cardinality rg stream.
type boundedStatCache struct {
	capacity int
	entries  map[string]*list.Element
	lru      *list.List
}

func newBoundedStatCache(capacity int) *boundedStatCache {
	return &boundedStatCache{
		capacity: max(0, capacity),
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
	}
}

func (c *boundedStatCache) get(path string, stat func(string) (os.FileInfo, error)) (os.FileInfo, error) {
	if element := c.entries[path]; element != nil {
		c.lru.MoveToFront(element)
		return element.Value.(statCacheEntry).info, nil
	}
	info, err := stat(path)
	if err != nil {
		return nil, err
	}
	if c.capacity == 0 {
		return info, nil
	}
	element := c.lru.PushFront(statCacheEntry{path: path, info: info})
	c.entries[path] = element
	if c.lru.Len() > c.capacity {
		oldest := c.lru.Back()
		delete(c.entries, oldest.Value.(statCacheEntry).path)
		c.lru.Remove(oldest)
	}
	return info, nil
}

// boundedMatchHeap is a min-heap keyed by eviction priority: the root is the
// match that should be discarded FIRST when the heap is full — the one with
// the oldest modTime, and among ties the one inserted latest (largest seq).
// This retains the top-K newest matches while preserving line order within
// ties (same modTime), matching the previous sort.SliceStable behaviour.
type boundedMatchHeap []grepMatch

func (h boundedMatchHeap) Len() int { return len(h) }
func (h boundedMatchHeap) Less(i, j int) bool {
	if !h[i].modTime.Equal(h[j].modTime) {
		return h[i].modTime.Before(h[j].modTime) // older = more evictable
	}
	return h[i].seq > h[j].seq // same modTime: later line = more evictable
}
func (h boundedMatchHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *boundedMatchHeap) Push(x any)   { *h = append(*h, x.(grepMatch)) }
func (h *boundedMatchHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// evictFirst reports whether a should be evicted before b (a is "less" in heap
// order — more evictable). Used to compare a candidate against the heap root.
func evictFirst(a, b grepMatch) bool {
	if !a.modTime.Equal(b.modTime) {
		return a.modTime.Before(b.modTime)
	}
	return a.seq > b.seq
}

type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

const (
	GrepToolName            = "grep"
	maxGrepContentWidth     = 500
	maxRipgrepJSONLineBytes = 4 * 1024 * 1024
	// maxFallbackLineBytes bounds the bytes accumulated for a single source
	// line in the regex fallback scanner (fileMatches). A single pathological
	// line with no newline — a minified bundle, a base64 blob — would
	// otherwise be read in full by bufio.Reader.ReadString and force an
	// unbounded allocation. Lines longer than this are truncated and flagged.
	// Mirrors the 4 MiB cap already used by the ripgrep branch's scanner.
	maxFallbackLineBytes = 4 * 1024 * 1024
	// fallbackTruncateSuffix marks a line that exceeded maxFallbackLineBytes.
	fallbackTruncateSuffix = "...[truncated]"
)

//go:embed grep.md.tpl
var grepDescriptionTmpl []byte

var grepDescriptionTpl = template.Must(
	template.New("grepDescription").
		Parse(string(grepDescriptionTmpl)),
)

type grepDescriptionData struct {
	MaxResults int
}

func grepDescription() string {
	return renderTemplate(grepDescriptionTpl, grepDescriptionData{
		MaxResults: 100,
	})
}

// escapeRegexPattern escapes special regex characters so they're treated as literal characters
func escapeRegexPattern(pattern string) string {
	specialChars := []string{"\\", ".", "+", "*", "?", "(", ")", "[", "]", "{", "}", "^", "$", "|"}
	escaped := pattern

	for _, char := range specialChars {
		escaped = strings.ReplaceAll(escaped, char, "\\"+char)
	}

	return escaped
}

func NewGrepTool(workingDir string, config config.ToolGrep, permissionServices ...permission.Service) fantasy.AgentTool {
	var permissions permission.Service
	if len(permissionServices) > 0 {
		permissions = permissionServices[0]
	}
	return fantasy.NewAgentTool(
		GrepToolName,
		grepDescription(),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			searchPattern := params.Pattern
			if params.LiteralText {
				searchPattern = escapeRegexPattern(params.Pattern)
			}

			searchPath := filepathext.SmartJoin(workingDir, cmp.Or(params.Path, workingDir))
			anchor, allowed, err := authorizeWorkspaceRead(
				ctx,
				permissions,
				workingDir,
				searchPath,
				GrepToolName,
				"read",
				call.ID,
				params,
			)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error authorizing search path: %v", err)), nil
			}
			if !allowed {
				return NewPermissionDeniedResponse(), nil
			}
			defer anchor.Close()

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()

			matches, truncated, err := searchFilesFS(searchCtx, searchPattern, anchor, params.Include, 100)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}

			var output strings.Builder
			if len(matches) == 0 {
				output.WriteString("No files found")
			} else {
				fmt.Fprintf(&output, "Found %d matches\n", len(matches))

				currentFile := ""
				for _, match := range matches {
					if currentFile != match.path {
						if currentFile != "" {
							output.WriteString("\n")
						}
						currentFile = match.path
						fmt.Fprintf(&output, "%s:\n", filepath.ToSlash(match.path))
					}
					if match.lineNum > 0 {
						lineText := match.lineText
						if len(lineText) > maxGrepContentWidth {
							lineText = stringext.Truncate(lineText, maxGrepContentWidth) + "..."
						}
						if match.charNum > 0 {
							fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
						} else {
							fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
						}
					} else {
						fmt.Fprintf(&output, "  %s\n", match.path)
					}
				}

				if truncated {
					output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output.String()),
				GrepResponseMetadata{
					NumberOfMatches: len(matches),
					Truncated:       truncated,
				},
			), nil
		},
	)
}

func searchFiles(ctx context.Context, pattern, rootPath, include string, limit int) ([]grepMatch, bool, error) {
	matches, truncated, err := searchWithRipgrep(ctx, pattern, rootPath, include, limit)
	if err != nil {
		var outputErr *RipgrepJSONError
		if errors.As(err, &outputErr) {
			return nil, false, err
		}
		// If the operation was cancelled or the deadline expired — either the
		// ripgrep error says so, or the context is already done for any other
		// reason — do NOT fall back to the much heavier regex tree walk. The
		// caller has given up, and a full walk would just burn CPU/IO for a
		// result nobody wants.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		matches, err = searchFilesWithRegex(ctx, pattern, rootPath, include)
		if err != nil {
			return nil, false, err
		}
		// Regex fallback path: sort + truncate as before.
		sort.SliceStable(matches, func(i, j int) bool {
			return matches[i].modTime.After(matches[j].modTime)
		})
		truncated = len(matches) > limit
		if truncated {
			matches = matches[:limit]
		}
	}
	return matches, truncated, nil
}

// searchFilesFS performs the bounded fallback search through an anchored FS.
// Ripgrep cannot consume fs.FS, so rooted searches deliberately use this
// equivalent bounded walker instead of reopening a path by name.
func searchFilesFS(ctx context.Context, pattern string, anchor *readAnchor, include string, limit int) ([]grepMatch, bool, error) {
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("invalid regex pattern: %w", err)
	}
	var includePattern *regexp.Regexp
	if include != "" {
		includePattern, err = globRegexCache.get(globToRegex(include))
		if err != nil {
			return nil, false, fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	root := anchor.FS()
	start := anchor.rootPath()
	walker := fsext.NewFastGlobWalkerFSAt(root, start)
	h := &boundedMatchHeap{}
	capacity := limit
	if capacity < 1 {
		capacity = 1
	}
	var seq int64
	var totalMatches int64
	err = fs.WalkDir(root, start, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			if walker.ShouldSkipDir(path) {
				return fs.SkipDir
			}
			return nil
		}
		if walker.ShouldSkip(path) || fsext.SkipHidden(path) {
			return nil
		}
		relPath := path
		if start != "." {
			relPath, _ = filepath.Rel(start, path)
		}
		if includePattern != nil && !includePattern.MatchString(filepath.ToSlash(relPath)) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		displayPath := filepath.Join(anchor.displayRoot(), filepath.FromSlash(path))
		if path == start {
			displayPath = anchor.path
		} else if start != "." {
			displayPath = filepath.Join(anchor.displayRoot(), filepath.FromSlash(relPath))
		}
		if hook, ok := ctx.Value(regexWalkHookKey{}).(func(string)); ok {
			hook(displayPath)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		matchErr := fileMatchesFS(ctx, root, path, regex, func(lm lineMatch) bool {
			seq++
			totalMatches++
			candidate := grepMatch{path: displayPath, modTime: info.ModTime(), lineNum: lm.lineNum, charNum: lm.charNum, lineText: lm.lineText, seq: seq}
			if h.Len() < capacity {
				heap.Push(h, candidate)
			} else if !evictFirst(candidate, (*h)[0]) {
				(*h)[0] = candidate
				heap.Fix(h, 0)
			}
			return true
		})
		if matchErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, false, err
	}
	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})
	if limit < 1 {
		matches = nil
	}
	return matches, totalMatches > int64(limit), nil
}

func searchWithRipgrep(ctx context.Context, pattern, path, include string, limit int) ([]grepMatch, bool, error) {
	cmd := getRgSearchCmd(ctx, pattern, path, include, 0)
	if cmd == nil {
		return nil, false, fmt.Errorf("ripgrep not found in $PATH")
	}
	appendRgIgnoreFiles(cmd, path)
	return searchWithRipgrepCommand(cmd, limit, maxRipgrepJSONLineBytes)
}

// ErrRipgrepJSONOutput identifies output that could not be consumed as a
// complete JSON stream. Callers must not silently switch search engines after
// this error: doing so can miss data beyond the parser's line limit.
var ErrRipgrepJSONOutput = errors.New("ripgrep JSON output error")

// ErrRipgrepJSONOutputTooLong identifies a JSON line larger than the parser's
// configured limit.
var ErrRipgrepJSONOutputTooLong = errors.New("ripgrep JSON output line too long")

// RipgrepJSONError preserves the scanner error after rg has been reaped.
type RipgrepJSONError struct {
	err     error
	tooLong bool
}

func (e *RipgrepJSONError) Error() string {
	if e.tooLong {
		return fmt.Sprintf("ripgrep JSON output line exceeds parser limit: %v", e.err)
	}
	return fmt.Sprintf("ripgrep JSON output could not be scanned: %v", e.err)
}

func (e *RipgrepJSONError) Unwrap() error { return e.err }

func (e *RipgrepJSONError) Is(target error) bool {
	if target == ErrRipgrepJSONOutput {
		return true
	}
	return e.tooLong && target == ErrRipgrepJSONOutputTooLong
}

func newRipgrepJSONError(err error) *RipgrepJSONError {
	return &RipgrepJSONError{
		err:     err,
		tooLong: errors.Is(err, bufio.ErrTooLong),
	}
}

func searchWithRipgrepCommand(cmd *exec.Cmd, limit, scannerMaxBytes int, statFns ...func(string) (os.FileInfo, error)) ([]grepMatch, bool, error) {

	// Stream rg's stdout line-by-line instead of buffering the entire output.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	stat := os.Stat
	if len(statFns) > 0 && statFns[0] != nil {
		stat = statFns[0]
	}
	capacity := max(0, limit)
	statCapacity := ripgrepStatCacheCapacity
	if capacity == 0 {
		statCapacity = 0
	}
	statCache := newBoundedStatCache(statCapacity)
	h := &boundedMatchHeap{}
	var seq int64

	scanner := bufio.NewScanner(stdout)
	// Allow long lines (minified JS etc.) — up to 4 MiB per JSON line.
	initialBufferBytes := min(64*1024, scannerMaxBytes)
	scanner.Buffer(make([]byte, 0, initialBufferBytes), scannerMaxBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type != "match" || len(match.Data.Submatches) == 0 {
			continue
		}
		// Only take the first submatch per line (matches original behaviour).
		sub := match.Data.Submatches[0]

		fi, statErr := statCache.get(match.Data.Path.Text, stat)
		if statErr != nil {
			continue // Skip files we can't access.
		}

		seq++
		gm := grepMatch{
			path:     match.Data.Path.Text,
			modTime:  fi.ModTime(),
			lineNum:  match.Data.LineNumber,
			charNum:  sub.Start + 1, // ensure 1-based
			lineText: strings.TrimSpace(match.Data.Lines.Text),
			seq:      seq,
		}

		if capacity > 0 && h.Len() < capacity {
			heap.Push(h, gm)
		} else if capacity > 0 && !evictFirst(gm, (*h)[0]) {
			// gm is less evictable than the root — it deserves a spot.
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	// If the scan loop stopped early due to a scanner error (most notably
	// bufio.ErrTooLong: a single JSON line — e.g. one match inside a
	// pathologically long line, a minified bundle, a base64 blob — exceeding
	// the 4 MiB buffer above) rather than reaching the pipe's natural EOF,
	// rg may still be mid-write with more output queued. Per os/exec's
	// documented contract, Wait must not be called until all reads from the
	// pipe have completed; skipping this drain lets rg block forever on a
	// full OS pipe buffer once nobody is reading it, and Wait then hangs
	// forever waiting for a process that will never exit on its own.
	// Confirmed by reproduction: a single ~6 MiB matched line reliably hung
	// Wait() indefinitely without this drain.
	scanErr := scanner.Err()
	if scanErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	// Wait for rg to finish and check exit code (1 = no matches, not an error).
	if waitErr := cmd.Wait(); waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// rg exit code 1 = no matches found.
		} else {
			return nil, false, waitErr
		}
	}
	if scanErr != nil {
		return nil, false, newRipgrepJSONError(scanErr)
	}

	// Extract and sort the bounded heap: newest modTime first, ties by seq asc
	// (preserves original line order within the same file/modTime).
	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})

	return matches, (limit <= 0 && seq > 0) || (limit > 0 && seq > int64(limit)), nil
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

// regexWalkHookKey is a test-only context seam. Production callers never put
// a value under this key, so the walk remains allocation-free in normal use.
type regexWalkHookKey struct{}

func searchFilesWithRegex(ctx context.Context, pattern, rootPath, include string) ([]grepMatch, error) {
	matches := []grepMatch{}

	// Use cached regex compilation
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	var includePattern *regexp.Regexp
	if include != "" {
		regexPattern := globToRegex(include)
		includePattern, err = globRegexCache.get(regexPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	// Create walker with gitignore and rushignore support
	walker := fsext.NewFastGlobWalker(rootPath)

	err = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Honour context cancellation before doing any per-file work.
		// Returning the context error aborts filepath.Walk and propagates to
		// the caller instead of grinding through the whole tree.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if info.IsDir() {
			// Check if directory should be skipped
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil // Continue into directory
		}

		// Use walker's shouldSkip method for files
		if walker.ShouldSkip(path) {
			return nil
		}

		// Skip hidden files (starting with a dot) to match ripgrep's default behavior
		base := filepath.Base(path)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		if includePattern != nil && !includePattern.MatchString(path) {
			return nil
		}

		if hook, ok := ctx.Value(regexWalkHookKey{}).(func(string)); ok {
			hook(path)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		stopWalk := false
		walkErr := fileMatches(ctx, path, regex, func(lm lineMatch) bool {
			matches = append(matches, grepMatch{
				path:     path,
				modTime:  info.ModTime(),
				lineNum:  lm.lineNum,
				charNum:  lm.charNum,
				lineText: lm.lineText,
			})
			if len(matches) >= 200 {
				stopWalk = true
				return false
			}
			return true
		})
		if walkErr != nil {
			// Propagate cancellation so the whole walk aborts immediately;
			// other read errors just skip the file (previous behaviour).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil // Skip files we can't read.
		}
		if stopWalk {
			return filepath.SkipAll
		}

		return nil
	})
	if err != nil {
		// filepath.Walk may return a context error returned by the callback
		// above; surface it directly.
		return nil, err
	}

	return matches, nil
}

// lineMatch is a single matching line within a file: its 1-based line
// number, the 1-based column of the first match on that line, and the
// line text (with the trailing newline stripped).
type lineMatch struct {
	lineNum  int
	charNum  int
	lineText string
}

// fileMatches calls onMatch for every line in filePath that matches pattern.
// Like ripgrep, it reports one entry per matching line (using the first match
// on the line for the column) instead of stopping at the first match in the
// file. If onMatch returns false, scanning stops immediately — the caller can
// bail out as soon as it has enough matches without reading the entire file.
// Lines longer than maxFallbackLineBytes are truncated and flagged rather than
// forcing an unbounded allocation. The context is honoured periodically so a
// cancelled caller does not wait for a huge file to finish scanning.
func fileMatches(ctx context.Context, filePath string, pattern *regexp.Regexp, onMatch func(lineMatch) bool) error {
	if pattern == nil {
		return nil
	}
	// Only search text files.
	if !isTextFile(filePath) {
		return nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var lineBuf bytes.Buffer
	lineNum := 0
	for {
		truncated, rerr := readBoundedLine(ctx, reader, &lineBuf, maxFallbackLineBytes)
		lineNum++

		// A non-EOF error (e.g. context cancellation surfaced mid-line by
		// readBoundedLine's in-line cadence, or a real I/O error) means buf
		// holds a partial, not-fully-read line: matching against it could
		// report a spurious/truncated hit to onMatch right before the error
		// is returned. Only io.EOF is a legitimate "line complete" case for a
		// final line with no trailing '\n' — that one must still be matched.
		if rerr != nil && rerr != io.EOF {
			return rerr
		}

		// If the file ends exactly at a '\n' (the common case), the previous
		// iteration already consumed and matched that final line; this
		// iteration's read hits io.EOF immediately with an empty lineBuf —
		// there is no line N+1. Without this check, a pattern that matches
		// the empty string (e.g. "a*", "^$", ".*") would fire onMatch for a
		// phantom line past the real end of file. This is distinct from the
		// #147 fix above: that one keeps io.EOF matchable for a genuine
		// final line with NO trailing '\n' (lineBuf non-empty); this one
		// only short-circuits when EOF arrives with nothing accumulated.
		if rerr == io.EOF && lineBuf.Len() == 0 {
			break
		}

		line := lineBuf.String()
		line = strings.TrimSuffix(line, "\r")
		if loc := pattern.FindStringIndex(line); loc != nil {
			lineText := line
			if truncated {
				lineText = line + fallbackTruncateSuffix
			}
			if !onMatch(lineMatch{
				lineNum:  lineNum,
				charNum:  loc[0] + 1,
				lineText: lineText,
			}) {
				return nil
			}
		}

		if rerr == io.EOF {
			break
		}

		// Honour cancellation mid-file (e.g. a single huge file). Checking on
		// every line adds overhead on files with many short lines, so check on
		// a fixed cadence.
		if lineNum%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return nil
}

func fileMatchesFS(ctx context.Context, fsys fs.FS, path string, pattern *regexp.Regexp, onMatch func(lineMatch) bool) error {
	if pattern == nil {
		return nil
	}
	file, err := fsys.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	prefix := make([]byte, 512)
	n, err := file.Read(prefix)
	if err != nil && err != io.EOF {
		return err
	}
	contentType := http.DetectContentType(prefix[:n])
	if !(strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml" || contentType == "application/javascript" || contentType == "application/x-sh") {
		return nil
	}
	var reader io.Reader = io.MultiReader(bytes.NewReader(prefix[:n]), file)
	if seeker, ok := file.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
		reader = file
	}
	return fileMatchesReader(ctx, reader, pattern, onMatch)
}

func fileMatchesReader(ctx context.Context, source io.Reader, pattern *regexp.Regexp, onMatch func(lineMatch) bool) error {
	reader := bufio.NewReader(source)
	var lineBuf bytes.Buffer
	lineNum := 0
	for {
		truncated, rerr := readBoundedLine(ctx, reader, &lineBuf, maxFallbackLineBytes)
		lineNum++
		if rerr != nil && rerr != io.EOF {
			return rerr
		}
		if rerr == io.EOF && lineBuf.Len() == 0 {
			break
		}
		line := strings.TrimSuffix(lineBuf.String(), "\r")
		if loc := pattern.FindStringIndex(line); loc != nil {
			lineText := line
			if truncated {
				lineText += fallbackTruncateSuffix
			}
			if !onMatch(lineMatch{lineNum: lineNum, charNum: loc[0] + 1, lineText: lineText}) {
				return nil
			}
		}
		if rerr == io.EOF {
			break
		}
		if lineNum%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// readBoundedLine reads a single line (up to and including the next '\n', or
// until the underlying reader is exhausted) from r into buf, resetting buf
// first. It accumulates at most maxLen bytes of the line into buf; any bytes
// beyond maxLen are read and discarded so a single pathological line with no
// newline cannot force an unbounded allocation. The trailing '\n' is consumed
// but not stored; a preceding '\r' (CRLF) is left in buf for the caller to
// trim. Returns io.EOF when the reader is exhausted. If the line was longer
// than maxLen, truncated is true.
//
// The context is honoured every cancellationCheckBytes so a cancelled caller
// does not wait for a single pathological line (no newline, many MiB) to be
// read in full before the per-line cancellation check in fileMatches fires: a
// single line means that check never runs, so without this in-line cadence a
// huge line would keep this byte loop (and the caller) alive long past the
// deadline. The maxLen cap bounds memory; this cadence bounds latency.
func readBoundedLine(ctx context.Context, r *bufio.Reader, buf *bytes.Buffer, maxLen int) (truncated bool, err error) {
	buf.Reset()
	const cancellationCheckBytes = 64 * 1024
	var read int64
	for {
		b, rerr := r.ReadByte()
		if rerr != nil {
			return truncated, rerr
		}
		read++
		if b == '\n' {
			return truncated, nil
		}
		if buf.Len() < maxLen {
			buf.WriteByte(b)
		} else {
			truncated = true
		}
		if read%cancellationCheckBytes == 0 && ctx.Err() != nil {
			return truncated, ctx.Err()
		}
	}
}

// isTextFile checks if a file is a text file by examining its MIME type.
func isTextFile(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()

	// Read first 512 bytes for MIME type detection.
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}

	// Detect content type.
	contentType := http.DetectContentType(buffer[:n])

	// Check if it's a text MIME type.
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/javascript" ||
		contentType == "application/x-sh"
}

func globToRegex(glob string) string {
	regexPattern := strings.ReplaceAll(glob, ".", "\\.")
	regexPattern = strings.ReplaceAll(regexPattern, "*", ".*")
	regexPattern = strings.ReplaceAll(regexPattern, "?", ".")

	// Use pre-compiled regex instead of compiling each time
	regexPattern = globBraceRegex.ReplaceAllStringFunc(regexPattern, func(match string) string {
		inner := match[1 : len(match)-1]
		return "(" + strings.ReplaceAll(inner, ",", "|") + ")"
	})

	return regexPattern
}
