package tools

// fs_grep is the scoped, batch-capable counterpart of grep with a
// context radius: every hit comes back as one numbered block containing
// up to ContextLines lines on each side of the hit, with overlapping or
// adjacent windows in the same file merged into a single block.
//
// Two search engines, mirroring grep.go:
//
//   - ripgrep, invoked with -C N. rg --json then emits "type":"context"
//     messages with the same data shape as "match" messages; both are
//     collected (one parser, shared below) and windows are rebuilt from
//     the hit line numbers at render time.
//   - a regex fallback tree walk, used when ripgrep is unavailable
//     (getRg returns "" under testing.Testing, so `go test` always runs
//     this path). The per-file scanner keeps a ring buffer of the last
//     ContextLines lines for before-context and a read-ahead counter
//     that records the ContextLines lines following each hit.
//
// Every rendered match path is resolved and checked against the folder
// scope with FileOpGrep — the batch runner only checked the item's ROOT,
// and matches can sit under deny-carved subtrees below it — so
// out-of-scope matches are dropped, never rendered.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

const FSGrepToolName = "fs_grep"

//go:embed fs_grep.md
var fsGrepDescription string

// FSGrepItem is one search inside an fs_grep batch. Path is the
// directory searched recursively; empty means the tool's working
// directory. ContextLines is the radius: lines shown on EACH side of a
// hit (0 = hit line only), capped at FSBatchMaxContextLines.
type FSGrepItem struct {
	Pattern      string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path         string `json:"path,omitempty" description:"The directory to search in. Defaults to the working directory."`
	Include      string `json:"include,omitempty" description:"File pattern to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText  bool   `json:"literal_text,omitempty" description:"If true, the pattern is treated as literal text with special regex characters escaped. Default is false."`
	ContextLines int    `json:"context_lines,omitempty" description:"Lines of context to show on each side of every hit (0-50, default 0). Overlapping windows are merged."`
}

// FSGrepParams is the tool's decoded input.
type FSGrepParams struct {
	Items []FSGrepItem `json:"items" description:"The searches to run in this batch"`
}

// NewFSGrepTool builds the scoped fs_grep tool. The scope is checked
// twice: once per item root by the batch runner's preflight, and once
// per match path here — a root check cannot vouch for paths found
// underneath it.
func NewFSGrepTool(workingDir string, scope permission.FolderScope) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		FSGrepToolName,
		fsGrepDescription,
		func(ctx context.Context, params FSGrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return RunFSBatch(ctx, FSBatch[FSGrepItem]{
				Tool:       FSGrepToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				PathOf:     func(item FSGrepItem) string { return cmp.Or(item.Path, ".") },
				Preflight:  fsGrepPreflight,
				Execute: func(ctx context.Context, group FSBatchGroup[FSGrepItem]) ([]FSItemOutcome, error) {
					outcomes := make([]FSItemOutcome, len(group.Items))
					for i, member := range group.Items {
						outcomes[i] = fsGrepRunItem(ctx, workingDir, scope, group.Path, member.Item)
					}
					return outcomes, nil
				},
			})
		},
	)
}

// fsGrepPreflight validates one item structurally; the runner applies
// the scope check to the item's root afterwards. An out-of-range
// radius is a structural failure, not a silent clamp: the model chose
// the number and must see it rejected.
func fsGrepPreflight(item FSGrepItem, _ int, _ string) (permission.FileOp, error) {
	if strings.TrimSpace(item.Pattern) == "" {
		return "", errors.New("pattern is required")
	}
	if item.ContextLines < 0 || item.ContextLines > FSBatchMaxContextLines {
		return "", fmt.Errorf("context_lines %d out of range (0-%d)",
			item.ContextLines, FSBatchMaxContextLines)
	}
	return permission.FileOpGrep, nil
}

// fsGrepRunItem searches one root and renders the item's block. Search
// errors fail the item (level 1: the model can correct the pattern or
// path); they never become Go errors.
func fsGrepRunItem(ctx context.Context, workingDir string, scope permission.FolderScope, rootPath string, item FSGrepItem) FSItemOutcome {
	pattern := item.Pattern
	if item.LiteralText {
		pattern = escapeRegexPattern(pattern)
	}

	searchCtx, cancel := context.WithTimeout(ctx, config.ToolGrep{}.GetTimeout())
	defer cancel()

	budget := newFSGrepBudget()
	files := make(map[string]*fsGrepFileHits)
	if err := fsGrepSearchContext(searchCtx, pattern, rootPath, item.Include,
		item.ContextLines, files, &budget); err != nil {
		return FSItemOutcome{Status: FSStatusFailed, Error: fmt.Sprintf("error searching files: %v", err)}
	}
	block, spent := fsGrepRender(files, &budget, scope, workingDir, item.ContextLines)
	if block == "" {
		block = fmt.Sprintf("No matches found for pattern %q under %s.",
			item.Pattern, filepath.ToSlash(rootPath))
	}
	if spent {
		block += fmt.Sprintf("\n(...output truncated at %d rendered lines per item...)",
			FSBatchMaxGrepMatchesPerItem)
	}
	return FSItemOutcome{Status: FSStatusOK, Block: block}
}

// fsGrepLine is one collected line of a hit window: its trimmed text and
// whether it is itself a hit.
type fsGrepLine struct {
	text string
	hit  bool
}

// fsGrepFileHits collects, for one file, every line that belongs to some
// rendered window plus which lines are hits. Lines may arrive twice —
// rg emits a line once per role (context of one hit, match of the next)
// — so adds are deduplicated by line number and the hit flag is sticky.
type fsGrepFileHits struct {
	lines   map[int]fsGrepLine
	hits    []int
	seen    map[int]struct{}
	maxLine int
}

func newFSGrepFileHits() *fsGrepFileHits {
	return &fsGrepFileHits{
		lines: make(map[int]fsGrepLine),
		seen:  make(map[int]struct{}),
	}
}

// add records one line and returns false when the item's line budget is
// spent, telling the scanners to stop. A line longer than
// maxGrepContentWidth is truncated the same way the legacy grep tool
// truncates its match lines.
func (f *fsGrepFileHits) add(lineNum int, text string, hit bool, budget *fsGrepBudget) bool {
	prev := f.lines[lineNum]
	if _, dup := f.seen[lineNum]; !dup {
		if !budget.take() {
			return false
		}
		f.seen[lineNum] = struct{}{}
		if len(text) > maxGrepContentWidth {
			text = stringext.Truncate(text, maxGrepContentWidth) + "..."
		}
		f.lines[lineNum] = fsGrepLine{text: text, hit: hit}
		f.maxLine = max(f.maxLine, lineNum)
	} else if hit && !prev.hit {
		// The same line arrived earlier as another hit's context:
		// upgrade it to a hit without spending the budget twice.
		f.lines[lineNum] = fsGrepLine{text: prev.text, hit: true}
	}
	if hit && !prev.hit {
		f.hits = append(f.hits, lineNum)
	}
	return true
}

// fsGrepBudget is the per-item output cap: FSBatchMaxGrepMatchesPerItem
// rendered lines, counting hits AND context lines (deduplicated), the
// same number the legacy grep tool caps its flat match list at.
type fsGrepBudget struct{ remaining int }

func newFSGrepBudget() fsGrepBudget {
	return fsGrepBudget{remaining: FSBatchMaxGrepMatchesPerItem}
}

func (b *fsGrepBudget) take() bool {
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	return true
}

func (b *fsGrepBudget) spent() bool { return b.remaining <= 0 }

// fsGrepSearchContext runs the radius-aware search: ripgrep when
// available, the regex fallback walk otherwise. If the ripgrep run
// failed after emitting partial output, the partial lines are discarded
// before the fallback so one file can never render twice under two path
// spellings; budget already spent on them is not refunded, which can
// only shrink output.
func fsGrepSearchContext(ctx context.Context, pattern, rootPath, include string, contextLines int, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	err := searchWithRipgrepContext(ctx, pattern, rootPath, include, contextLines, files, budget)
	if err == nil {
		return nil
	}
	// A given-up caller must not trigger the heavier walk (same rule as
	// grep.go's searchFiles).
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	clear(files)
	return searchFilesWithRegexContext(ctx, pattern, rootPath, include, contextLines, files, budget)
}

// searchWithRipgrepContext runs one rg search with -C N and parses both
// match and context messages into files. It reports the same
// "ripgrep not found" error as the legacy search when rg is absent, so
// the dispatcher falls back identically.
func searchWithRipgrepContext(ctx context.Context, pattern, rootPath, include string, contextLines int, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	cmd := getRgSearchCmd(ctx, pattern, rootPath, include, contextLines)
	if cmd == nil {
		return errors.New("ripgrep not found in $PATH")
	}
	appendRgIgnoreFiles(cmd, rootPath)
	return runRipgrepContextSearch(ctx, cmd, files, budget)
}

// runRipgrepContextSearch starts cmd and streams its rg --json output
// into the collectors. It is split from searchWithRipgrepContext so
// tests can run a real rg binary (resolved via exec.LookPath, bypassing
// the getRg testing guard) through the identical start/parse/drain/wait
// sequence. Early stops — budget spent or a scan error — drain the pipe
// before Wait, per os/exec's contract (see searchWithRipgrep in
// grep.go for the deadlock this prevents).
func runRipgrepContextSearch(ctx context.Context, cmd *exec.Cmd, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	parseErr := parseRipgrepContextStream(stdout, files, budget)
	if parseErr != nil || budget.spent() {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return parseErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			return nil // rg exit code 1 = no matches found.
		}
		return waitErr
	}
	return nil
}

// parseRipgrepContextStream parses rg --json lines. Exactly one new
// event type is handled relative to the legacy parser: "context", whose
// data shape (path, lines.text, line_number) is identical to "match".
// begin/end/summary events, unparseable lines and lines without a
// usable number are skipped. Stops at EOF, on the item's spent budget,
// or on a scanner error.
func parseRipgrepContextStream(r io.Reader, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	scanner := bufio.NewScanner(r)
	// Allow long lines (minified JS etc.) — up to 4 MiB per JSON line,
	// matching the legacy parser.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg ripgrepMatch
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Type != "match" && msg.Type != "context" {
			continue
		}
		if msg.Data.Path.Text == "" || msg.Data.LineNumber <= 0 {
			continue
		}
		file := files[msg.Data.Path.Text]
		if file == nil {
			file = newFSGrepFileHits()
			files[msg.Data.Path.Text] = file
		}
		if !file.add(msg.Data.LineNumber,
			strings.TrimSpace(msg.Data.Lines.Text), msg.Type == "match", budget) {
			return nil // Budget spent; caller drains and stops.
		}
	}
	return scanner.Err()
}

// searchFilesWithRegexContext walks rootPath with the same skip rules as
// the legacy searchFilesWithRegex (ignore files, hidden files, include
// glob, text-file detection) and scans every file with the radius-aware
// scanner, filling the shared collectors until the budget is spent.
func searchFilesWithRegexContext(ctx context.Context, pattern, rootPath, include string, contextLines int, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return fmt.Errorf("invalid regex pattern: %w", err)
	}
	var includePattern *regexp.Regexp
	if include != "" {
		includePattern, err = globRegexCache.get(globToRegex(include))
		if err != nil {
			return fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	walker := fsext.NewFastGlobWalker(rootPath)
	stop := false
	return filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if stop {
			return filepath.SkipAll
		}
		if err != nil {
			return nil // Skip errors.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			if walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if walker.ShouldSkip(path) {
			return nil
		}
		if base := filepath.Base(path); base != "." && strings.HasPrefix(base, ".") {
			return nil // Match ripgrep's default: skip hidden files.
		}
		if includePattern != nil && !includePattern.MatchString(path) {
			return nil
		}

		if err := scanFileWithContext(ctx, path, regex, contextLines, files, budget); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil // Skip files we cannot read, like the legacy walk.
		}
		if budget.spent() {
			stop = true
		}
		return nil
	})
}

// scanFileWithContext scans one text file and records every matching
// line plus up to contextLines lines of surrounding context for each
// hit. Before-context comes from a ring buffer holding the last
// contextLines lines; after-context comes from a read-ahead counter
// that keeps recording for contextLines lines past every hit. Window
// merging (overlapping or adjacent) is NOT done here: the collector
// just records which lines are hits, and fsGrepRender merges from the
// hit numbers. Budget exhaustion stops the scan mid-file; the caller
// reports the truncation.
func scanFileWithContext(ctx context.Context, filePath string, regex *regexp.Regexp, contextLines int, files map[string]*fsGrepFileHits, budget *fsGrepBudget) error {
	if regex == nil || !isTextFile(filePath) {
		return nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	collector := newFSGrepFileHits()
	files[filePath] = collector

	type ringEntry struct {
		num  int
		text string
	}
	var ring []ringEntry // The last contextLines lines not yet recorded.
	readAhead := 0       // Lines that must still be recorded after a hit.

	reader := bufio.NewReader(file)
	var lineBuf bytes.Buffer
	lineNum := 0
	for {
		truncated, rerr := readBoundedLine(ctx, reader, &lineBuf, maxFallbackLineBytes)
		lineNum++
		// A non-EOF error means lineBuf holds a partial line that must
		// not be matched (see fileMatches in grep.go); EOF with an empty
		// buffer is the clean end of a newline-terminated file.
		if rerr != nil && rerr != io.EOF {
			return rerr
		}
		if rerr == io.EOF && lineBuf.Len() == 0 {
			break
		}

		text := strings.TrimSuffix(lineBuf.String(), "\r")
		isHit := regex.MatchString(text)
		if truncated {
			text += fallbackTruncateSuffix
		}

		if isHit || readAhead > 0 {
			if isHit {
				readAhead = contextLines
			} else {
				readAhead--
			}
			// Before-context: the ring holds exactly the recorded-candidate
			// lines preceding this one; adds are deduplicated, so a line
			// already recorded (e.g. as a previous hit's after-context) is
			// not double-counted against the budget.
			from := lineNum - contextLines
			for _, entry := range ring {
				if entry.num >= from && !collector.add(entry.num, entry.text, false, budget) {
					return nil // Budget spent.
				}
			}
			if !collector.add(lineNum, text, isHit, budget) {
				return nil // Budget spent.
			}
			if budget.spent() {
				return nil
			}
		} else {
			ring = append(ring, ringEntry{lineNum, text})
			if len(ring) > contextLines {
				ring = ring[len(ring)-contextLines:]
			}
		}

		if rerr == io.EOF {
			break
		}
		// Honour cancellation mid-file on the same cadence as the legacy
		// scanner.
		if lineNum%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// fsGrepWindow is one merged context window: the rendered line range and
// the hit that opened it.
type fsGrepWindow struct {
	start, end, firstHit int
}

// mergeFSGrepWindows turns ascending hit line numbers into merged
// windows: each hit claims contextLines lines on each side, and windows
// that overlap or touch (next start <= current end + 1) coalesce into
// one so no line is rendered twice.
func mergeFSGrepWindows(hits []int, contextLines int) []fsGrepWindow {
	sorted := slices.Clone(hits)
	slices.Sort(sorted)
	windows := make([]fsGrepWindow, 0, len(sorted))
	for _, hit := range sorted {
		start, end := max(1, hit-contextLines), hit+contextLines
		if n := len(windows); n > 0 && start <= windows[n-1].end+1 {
			windows[n-1].end = max(windows[n-1].end, end)
			continue
		}
		windows = append(windows, fsGrepWindow{start: start, end: end, firstHit: hit})
	}
	return windows
}

// fsGrepRender renders every in-scope file's merged hit windows into
// <match> blocks joined by newlines, dropping files whose resolved path
// the scope denies (a root check cannot vouch for matches found under
// deny-carved subtrees). Unresolvable match paths are dropped too:
// a path that cannot be resolved cannot be judged safe. It returns the
// blocks and whether the budget was spent (output truncated).
func fsGrepRender(files map[string]*fsGrepFileHits, budget *fsGrepBudget, scope permission.FolderScope, workingDir string, contextLines int) (string, bool) {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	var b strings.Builder
	resolvedCache := make(map[string]string, len(paths))
	for _, path := range paths {
		collector := files[path]
		if len(collector.hits) == 0 {
			continue
		}
		abs, ok := resolvedCache[path]
		if !ok {
			resolved, err := resolveScopedPath(workingDir, path)
			if err != nil {
				continue
			}
			resolvedCache[path] = resolved
			abs = resolved
		}
		if scope.Check(abs, permission.FileOpGrep) != nil {
			continue
		}
		for _, window := range mergeFSGrepWindows(collector.hits, contextLines) {
			// A window may claim lines past the file's end or past the
			// budget cut; render only what was actually collected.
			window.end = min(window.end, collector.maxLine)
			b.WriteString(renderFSMatchBlock(path, window, collector.lines))
			b.WriteString("\n")
		}
	}
	return b.String(), budget.spent()
}

// renderFSMatchBlock renders one merged window as a <match> block: the
// path, the window range and the first hit line number in the
// attributes, then every collected line numbered and padded, hits
// prefixed with "> " and pure context with two spaces.
func renderFSMatchBlock(path string, window fsGrepWindow, lines map[int]fsGrepLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<match path=%q lines=\"%d-%d\" hit=\"%d\">\n",
		filepath.ToSlash(path), window.start, window.end, window.firstHit)
	width := len(strconv.Itoa(window.end))
	for lineNum := window.start; lineNum <= window.end; lineNum++ {
		line, ok := lines[lineNum]
		if !ok {
			continue
		}
		marker := "  "
		if line.hit {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%*d | %s\n", marker, width, lineNum, line.text)
	}
	b.WriteString("</match>")
	return b.String()
}
