// The deterministic review-evidence block — design §4 of
// docs/plans/2026-10-02-reviewer-pass-verification.md.
//
// Everything here is computed by rush itself (git working tree + session
// database) rather than taken from the orchestrator's words, so the reviewer
// pass can be handed facts it does not have to trust. The block is appended to
// the fixed reviewer prompt and wrapped in <review_evidence>, and it is built
// ONLY when a reviewer pass will actually run: with basis == nil (no reviewer
// configured, or a call that is not a `rush run` loop's first turn) the prompt
// goes out byte-identical to what it was before this block existed.

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
)

// reviewEvidenceMaxChars is the hard cap on the whole block, suffix included.
// Beyond it the block is cut and closed with the truncation marker so the
// reviewer can tell an incomplete listing from a complete one.
const reviewEvidenceMaxChars = 6000

// Per-section caps. Each section is trimmed on its own so one runaway section
// (a 5000-rune prompt, a 60-file diff) cannot silently eat the others.
const (
	reviewEvidenceRequestRunes = 1500
	reviewEvidenceChangedLines = 40
	reviewEvidenceDirtyLines   = 20
	reviewEvidenceCommandCount = 8
	reviewEvidenceCommandRunes = 160
	reviewEvidenceTodoCount    = 10
	reviewEvidenceTodoRunes    = 120
	// reviewEvidenceGitTimeout bounds each git invocation. Three commands at
	// 10s is 30s worst case, against a review turn that may run for an hour.
	reviewEvidenceGitTimeout = 10 * time.Second
	// reviewEvidenceVeryShortRunes is the "very_short" floor. It is the
	// weakest of the flags: see finalTextFlags for the exclusive chain.
	reviewEvidenceVeryShortRunes = 80
	// reviewEvidenceTruncationMinRunes is the shortest answer for which
	// "possibly_truncated" says anything. Below it there is no report to
	// truncate — a 6-word stub is short, it is not "cut off" — and reporting
	// both would make the flag useless for the reviewer prompt to triage.
	reviewEvidenceTruncationMinRunes = 20
)

// gitSnapshot is one read of the working tree: the branch/HEAD plus the
// porcelain-status XY columns and the diff --numstat add/delete columns, both
// keyed by path so before/after can be diffed file by file.
type gitSnapshot struct {
	OK      bool
	Head    string
	Branch  string
	Status  map[string]string // path -> "XY"
	Numstat map[string]string // path -> "<added>\t<deleted>"
}

// reviewBasis is what the first turn records for the reviewer pass: when the
// run started, what was asked, where the tree lives, and the tree's state
// before the run touched it (so a file that was already dirty is not blamed on
// this run — the shared-tree guard, e.g. web/dist/.gitkeep).
type reviewBasis struct {
	Start      time.Time
	Prompt     string
	WorkingDir string
	Before     gitSnapshot
}

// commandResult is one test/build command the transcript shows actually ran,
// with the exit code rush could recover for it.
type commandResult struct {
	Cmd   string
	Exit  int
	Async bool
	// Known is false when the exit code could not be recovered: an async job
	// whose completion notice never arrived (still running, or timed out /
	// stopped / failed without an exit).
	Known bool
}

// captureReviewBasis snapshots the run's git state before its first turn. It
// returns nil unless the run is a candidate for the reviewer pass at all, so
// every run that will not be reviewed pays nothing and stays byte-identical.
func (app *App) captureReviewBasis(ctx context.Context, role config.SelectedModelType, prompt string, start time.Time) *reviewBasis {
	cfg := app.config.Config()
	if !shouldRunReviewerPass(role, cfg) {
		return nil
	}
	dir := app.config.WorkingDir()
	return &reviewBasis{
		Start:      start,
		Prompt:     prompt,
		WorkingDir: dir,
		Before:     readGitSnapshot(ctx, dir),
	}
}

// readGitSnapshot reads the working tree at dir. A missing git binary, a
// directory that is not inside a work tree, or any failure of the status
// command yields gitSnapshot{OK: false} — the evidence then says plainly that
// there is no repository instead of guessing.
func readGitSnapshot(ctx context.Context, dir string) gitSnapshot {
	if strings.TrimSpace(dir) == "" {
		return gitSnapshot{}
	}
	out, err := evidenceGit(ctx, dir, "-c", "core.quotepath=off", "status", "--porcelain=v1", "--branch")
	if err != nil {
		return gitSnapshot{}
	}
	snap := gitSnapshot{
		OK:      true,
		Status:  make(map[string]string),
		Numstat: make(map[string]string),
	}
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if i == 0 {
			snap.Branch = evidenceStatusBranch(line)
			continue
		}
		if path, xy, ok := evidenceStatusEntry(line); ok {
			snap.Status[path] = xy
		}
	}
	// An empty repository has no HEAD, and `git diff HEAD` fails there; the
	// plain working-tree diff is the best available answer for it.
	numstat, numErr := evidenceGit(ctx, dir, "-c", "core.quotepath=off", "diff", "--numstat", "HEAD")
	if numErr != nil {
		if numstat, numErr = evidenceGit(ctx, dir, "-c", "core.quotepath=off", "diff", "--numstat"); numErr != nil {
			numstat = ""
		}
	}
	for _, line := range strings.Split(numstat, "\n") {
		if path, entry, ok := evidenceNumstatEntry(line); ok {
			snap.Numstat[path] = entry
		}
	}
	if head, headErr := evidenceGit(ctx, dir, "rev-parse", "--short", "HEAD"); headErr == nil {
		snap.Head = strings.TrimSpace(head)
	}
	return snap
}

// evidenceGit runs one bounded, non-locking git command in dir.
func evidenceGit(ctx context.Context, dir string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, reviewEvidenceGitTimeout)
	defer cancel()
	cmd := platform.Command(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = evidenceGitEnv()
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// evidenceGitEnv inherits the process environment and forces
// GIT_OPTIONAL_LOCKS=0 unless the operator already set it: a read-only status
// must not be able to refresh .git/index (and leave an index.lock behind) in
// a tree several agents share. Same reasoning as shell.go's A8 fix.
func evidenceGitEnv() []string {
	env := os.Environ()
	const marker = "GIT_OPTIONAL_LOCKS"
	for _, entry := range env {
		if entry == marker || strings.HasPrefix(entry, marker+"=") {
			return env
		}
	}
	return append(env, marker+"=0")
}

// evidenceStatusBranch parses the `## branch...upstream [ahead N]` header of
// `git status --porcelain=v1 --branch`. An empty repository prints
// "## No commits yet on <branch>" instead.
func evidenceStatusBranch(line string) string {
	rest := strings.TrimPrefix(strings.TrimRight(line, "\r"), "## ")
	if after, ok := strings.CutPrefix(rest, "No commits yet on "); ok {
		return after
	}
	if i := strings.Index(rest, "..."); i > 0 {
		return rest[:i]
	}
	if i := strings.IndexAny(rest, " \t"); i > 0 {
		return rest[:i]
	}
	return rest
}

// evidenceStatusEntry splits one porcelain status line into its two XY
// columns and its path. Rename/copy entries read "XY old -> new", where the
// new path is the one that matters for the diff.
func evidenceStatusEntry(line string) (path, xy string, ok bool) {
	if len(line) < 4 {
		return "", "", false
	}
	xy = line[:2]
	rest := strings.TrimRight(strings.TrimPrefix(line[2:], " "), "\r")
	if rest == "" {
		return "", "", false
	}
	if i := strings.LastIndex(rest, " -> "); i >= 0 {
		rest = rest[i+len(" -> "):]
	}
	if rest == "" {
		return "", "", false
	}
	return rest, xy, true
}

// evidenceNumstatEntry splits one `git diff --numstat` line
// ("<added>\t<deleted>\t<path>") into path and the raw add/delete columns.
// Binary files report "-" for both columns, which is kept verbatim.
func evidenceNumstatEntry(line string) (path, entry string, ok bool) {
	parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
	if len(parts) < 3 {
		return "", "", false
	}
	path = strings.Join(parts[2:], "\t")
	if path == "" {
		return "", "", false
	}
	return path, parts[0] + "\t" + parts[1], true
}

// reviewerTurnPrompt is the user prompt of the review turn: the fixed reviewer
// prompt, plus the deterministic evidence block when a basis was captured.
// A nil basis (no reviewer configured, or a call that is not the first turn of
// a `rush run` loop) returns the prompt alone, byte-identical to before.
func (app *App) reviewerTurnPrompt(ctx context.Context, sessionID string, basis *reviewBasis) string {
	if basis == nil {
		return reviewerPassPrompt
	}
	msgs, err := app.Messages.List(ctx, sessionID)
	if err != nil {
		slog.Warn("run: failed to read the session transcript for the review evidence; the reviewer prompt goes out without it",
			"session", sessionID, "err", err)
		return reviewerPassPrompt
	}
	sess, err := app.Sessions.Get(ctx, sessionID)
	if err != nil {
		slog.Warn("run: failed to read the session row for the review evidence; the reviewer prompt goes out without it",
			"session", sessionID, "err", err)
		return reviewerPassPrompt
	}
	after := readGitSnapshot(ctx, basis.WorkingDir)
	return reviewerPassPrompt + "\n\n" + buildReviewEvidence(basis, after, msgs, sess.Todos)
}

// buildReviewEvidence renders the block. Pure: every input is passed in, so
// the size-cap behaviour is testable without a repository or a database.
func buildReviewEvidence(basis *reviewBasis, after gitSnapshot, msgs []message.Message, todos []session.Todo) string {
	var b strings.Builder
	b.WriteString(`<review_evidence trust="computed by rush, not by the orchestrator">` + "\n")
	writeEvidenceGitSection(&b, after)
	writeEvidenceRequestSection(&b, basis)
	writeEvidenceChangesSection(&b, basis, after)
	writeEvidenceCommandSection(&b, msgs, basis.Start.Unix())
	writeEvidenceTodoSection(&b, todos)
	writeEvidenceToolCallSection(&b, msgs, basis.Start.Unix())
	writeEvidenceFinalTextSection(&b, msgs)
	b.WriteString("note: worker sub-sessions are not scanned; check worker claims in files or with read_delegation_transcript\n")
	b.WriteString("</review_evidence>")

	out := b.String()
	if len(out) <= reviewEvidenceMaxChars {
		return out
	}
	suffix := "\n(evidence truncated)\n</review_evidence>"
	if len(suffix) >= reviewEvidenceMaxChars {
		return suffix
	}
	cut := reviewEvidenceMaxChars - len(suffix)
	// Never cut a UTF-8 sequence in half: the block is read by the model.
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + suffix
}

func writeEvidenceGitSection(b *strings.Builder, after gitSnapshot) {
	if !after.OK {
		b.WriteString("git: not a repository\n")
		return
	}
	branch := after.Branch
	if branch == "" {
		branch = "(detached)"
	}
	head := after.Head
	if head == "" {
		head = "(no commits)"
	}
	fmt.Fprintf(b, "git: branch %s @ %s\n", branch, head)
}

func writeEvidenceRequestSection(b *strings.Builder, basis *reviewBasis) {
	b.WriteString("original_request:\n")
	b.WriteString(truncateRunes(basis.Prompt, reviewEvidenceRequestRunes))
	b.WriteString("\n")
}

// writeEvidenceChangesSection lists what this run touched and what was
// already dirty before it started. Both come from the two snapshots, so a
// file the run inherited dirty is named separately and the reviewer is told
// not to attribute it to this run.
func writeEvidenceChangesSection(b *strings.Builder, basis *reviewBasis, after gitSnapshot) {
	b.WriteString("changed_during_run:\n")
	if !after.OK {
		b.WriteString("  (no repository)\n")
	} else {
		changed := changedDuringRun(basis.Before, after)
		if len(changed) == 0 {
			b.WriteString("  (none)\n")
		}
		for i, path := range changed {
			if i >= reviewEvidenceChangedLines {
				fmt.Fprintf(b, "  ... and %d more\n", len(changed)-i)
				break
			}
			fmt.Fprintf(b, "  %s %s %s\n", statusColumns(basis.Before, after, path), path, numstatColumns(basis.Before, after, path))
		}
	}

	b.WriteString("dirty_before_run:\n")
	if !basis.Before.OK {
		b.WriteString("  (no repository)\n")
		return
	}
	dirty := snapshotPaths(basis.Before)
	if len(dirty) == 0 {
		b.WriteString("  (none)\n")
	}
	for i, path := range dirty {
		if i >= reviewEvidenceDirtyLines {
			fmt.Fprintf(b, "  ... and %d more\n", len(dirty)-i)
			break
		}
		fmt.Fprintf(b, "  %s %s\n", path, basis.Before.Status[path])
	}
}

// statusColumns is the after-snapshot XY pair. A path that carried a status
// line before and has none now was cleaned up by the run; one that never had
// one is an untracked arrival.
func statusColumns(before, after gitSnapshot, path string) string {
	if xy, ok := after.Status[path]; ok {
		return xy
	}
	if _, existed := before.Status[path]; existed {
		return "  " // was listed, is now clean
	}
	return "??"
}

func numstatColumns(before, after gitSnapshot, path string) string {
	if ns, ok := after.Numstat[path]; ok {
		return ns
	}
	if ns, ok := before.Numstat[path]; ok {
		return ns
	}
	return "0\t0"
}

// changedDuringRun is the set of paths whose status line OR numstat line moved
// between the two snapshots, plus paths that appear only after the run. Sorted
// so the block is reproducible.
func changedDuringRun(before, after gitSnapshot) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(path string) {
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	for path, xy := range after.Status {
		if was, ok := before.Status[path]; !ok || was != xy || xy == "??" {
			add(path)
		}
	}
	for path, entry := range after.Numstat {
		if was, ok := before.Numstat[path]; !ok || was != entry {
			add(path)
		}
	}
	// Also reported: a path the run CLEANED or deleted, whose line is gone
	// from `after` entirely — "it changed", not "it did not change".
	for path := range before.Status {
		if _, still := after.Status[path]; !still {
			add(path)
		}
	}
	for path := range before.Numstat {
		if _, still := after.Numstat[path]; !still {
			add(path)
		}
	}
	slices.Sort(out)
	return out
}

func snapshotPaths(snap gitSnapshot) []string {
	seen := make(map[string]struct{})
	var out []string
	for path := range snap.Status {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	for path := range snap.Numstat {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// reviewCommandFilters are the substrings that make a shell command a
// test/build command worth reporting to the reviewer.
var reviewCommandFilters = []string{
	"go test", "go build", "go vet", "gofmt", "golangci-lint",
	"npm", "pnpm", "pytest", "cargo", "make",
}

func isReviewCommand(cmd string) bool {
	for _, filter := range reviewCommandFilters {
		if strings.Contains(cmd, filter) {
			return true
		}
	}
	return false
}

// commandResults recovers the test/build commands that actually ran in the
// window, with their exit codes. An async command's code comes from its
// background-job completion notice; a synchronous one from its tool result.
func commandResults(msgs []message.Message, since int64) []commandResult {
	results := make(map[string]message.ToolResult)
	notices := make(map[string]message.Message)
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID != "" {
				results[tr.ToolCallID] = tr
			}
		}
		if msg.Role != message.User || !msg.BackgroundJobNotice {
			continue
		}
		jobID := noticeJobID(msg.FullText())
		if jobID == "" {
			continue
		}
		if _, dup := notices[jobID]; !dup {
			notices[jobID] = msg
		}
	}

	var out []commandResult
	for _, msg := range msgs {
		if msg.CreatedAt < since {
			continue
		}
		for _, part := range msg.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok {
				continue
			}
			cmd, ok := evidenceCommandString(tc)
			if !ok || !isReviewCommand(cmd) {
				continue
			}
			out = append(out, evidenceCommandResult(cmd, results[tc.ID], notices))
		}
	}
	if len(out) > reviewEvidenceCommandCount {
		out = out[len(out)-reviewEvidenceCommandCount:]
	}
	return out
}

// evidenceCommandString renders the command a tool_call ran: bash's raw
// command line, or run_command's program plus its arguments.
func evidenceCommandString(tc message.ToolCall) (string, bool) {
	var params map[string]any
	if err := json.Unmarshal([]byte(tc.Input), &params); err != nil {
		return "", false
	}
	switch tc.Name {
	case "bash":
		cmd, _ := params["command"].(string)
		return cmd, cmd != ""
	case "run_command":
		program, _ := params["program"].(string)
		if program == "" {
			return "", false
		}
		rawArgs, _ := params["args"].([]any)
		parts := make([]string, 0, len(rawArgs))
		for _, item := range rawArgs {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return program, true
		}
		return program + " " + strings.Join(parts, " "), true
	}
	return "", false
}

// evidenceCommandResult turns one tool call and whatever the transcript holds
// about it into a commandResult.
func evidenceCommandResult(cmd string, result message.ToolResult, notices map[string]message.Message) commandResult {
	async, jobID := evidenceAsyncTag(result)
	if !async {
		// A synchronous result: "Exit code N" in the content means a non-zero
		// exit, its absence means 0. Either way the code IS known.
		exit := 0
		if m := reviewSyncExitRe.FindStringSubmatch(result.Content); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				exit = n
			}
		}
		return commandResult{Cmd: cmd, Exit: exit, Known: true}
	}
	if jobID != "" {
		if notice, ok := notices[jobID]; ok {
			if exit, known := evidenceNoticeExit(notice.FullText()); known {
				return commandResult{Cmd: cmd, Exit: exit, Async: true, Known: true}
			}
		}
	}
	// No notice (the job may still be running) or a notice without an exit
	// code (it timed out / was stopped / failed): "unknown", never a false 0.
	return commandResult{Cmd: cmd, Async: true}
}

// evidenceAsyncTag reports whether a tool result is an async started-ack
// (metadata `{"async":true,"job_id":...}`) and which job it belongs to.
func evidenceAsyncTag(result message.ToolResult) (async bool, jobID string) {
	if strings.TrimSpace(result.Metadata) == "" {
		return false, ""
	}
	var meta struct {
		Async  bool   `json:"async"`
		Inline bool   `json:"inline"`
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(result.Metadata), &meta); err != nil {
		return false, ""
	}
	if !meta.Async {
		return false, ""
	}
	return true, meta.JobID
}

// reviewSyncExitRe matches the bash tool's own failure tail
// (`formatOutput`: "\nExit code %d").
var reviewSyncExitRe = regexp.MustCompile(`Exit code (\d+)`)

// reviewAsyncExitRe matches backgroundJobSummary's "finished: exit N".
var reviewAsyncExitRe = regexp.MustCompile(`finished:?\s+exit\s+(-?\d+)`)

// reviewAsyncStatusRe matches agent.FormatAsyncCompletion's finished/failed
// notices ("Async job <id> (<tool>) finished.").
var reviewAsyncStatusRe = regexp.MustCompile(`(?s)\AAsync job \S+ \([^)]*\) (finished|failed)\.`)

// evidenceNoticeExit reads the exit code out of a background-job notice. Both
// notice families are handled; the other terminal wordings
// FormatAsyncCompletion uses ("timed out", "was stopped", "was cancelled",
// "was interrupted") carry no exit code, and the caller maps that to unknown
// rather than to zero.
func evidenceNoticeExit(content string) (int, bool) {
	if m := reviewAsyncExitRe.FindStringSubmatch(content); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n, true
		}
	}
	if m := reviewAsyncStatusRe.FindStringSubmatch(content); m != nil {
		if m[1] == "finished" {
			return 0, true
		}
		// A failed job's bash output carries the real "\nExit code N" tail.
		if m := reviewSyncExitRe.FindStringSubmatch(content); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n, true
			}
		}
		return 0, false
	}
	return 0, false
}

// noticeJobID pulls the job id out of a notice body. Both notice families name
// it first ("Background job <id> (...)" / "Async job <id> (...)").
func noticeJobID(content string) string {
	for _, re := range reviewNoticeIDRes {
		if m := re.FindStringSubmatch(content); m != nil {
			return m[1]
		}
	}
	return ""
}

var reviewNoticeIDRes = []*regexp.Regexp{
	regexp.MustCompile(`Background job ([^\s(` + "`" + `]+)`),
	regexp.MustCompile(`Async job ([^\s(` + "`" + `]+)`),
}

func writeEvidenceCommandSection(b *strings.Builder, msgs []message.Message, since int64) {
	results := commandResults(msgs, since)
	if len(results) == 0 {
		b.WriteString("no_test_or_build_command_seen: true\n")
		return
	}
	b.WriteString("test_or_build_commands:\n")
	for _, res := range results {
		fmt.Fprintf(b, "  $ %s -> %s\n", truncateRunes(res.Cmd, reviewEvidenceCommandRunes), formatCommandExit(res))
	}
}

func formatCommandExit(res commandResult) string {
	if !res.Known {
		return "exit=unknown (still running?)"
	}
	return "exit " + strconv.Itoa(res.Exit)
}

func writeEvidenceTodoSection(b *strings.Builder, todos []session.Todo) {
	b.WriteString("open_todos:\n")
	open := make([]session.Todo, 0, len(todos))
	for _, todo := range todos {
		if todo.Status != session.TodoStatusCompleted {
			open = append(open, todo)
		}
	}
	if len(open) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	shown := open
	if len(shown) > reviewEvidenceTodoCount {
		shown = shown[:reviewEvidenceTodoCount]
	}
	for _, todo := range shown {
		fmt.Fprintf(b, "  [%s] %s\n", todo.Status, truncateRunes(todo.Content, reviewEvidenceTodoRunes))
	}
	if len(open) > len(shown) {
		fmt.Fprintf(b, "  ... and %d more\n", len(open)-len(shown))
	}
}

// writeEvidenceToolCallSection counts the tool calls of the window by name.
// A reviewer that made zero calls has verified nothing, and the counts also
// show where the orchestrator spent its turn.
func writeEvidenceToolCallSection(b *strings.Builder, msgs []message.Message, since int64) {
	counts := make(map[string]int)
	for _, msg := range msgs {
		if msg.CreatedAt < since {
			continue
		}
		for _, part := range msg.Parts {
			if tc, ok := part.(message.ToolCall); ok && tc.Name != "" {
				counts[tc.Name]++
			}
		}
	}
	b.WriteString("tool_calls_this_run:\n")
	if len(counts) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(b, "  %s: %d\n", name, counts[name])
	}
}

// writeEvidenceFinalTextSection describes the orchestrator's own final
// answer: how long it is, which deterministic quality flag it raises, and
// whether it is a report of the work or a reply to a background notice that
// buried the report.
func writeEvidenceFinalTextSection(b *strings.Builder, msgs []message.Message) {
	text := finalAssistantText(msgs)
	b.WriteString("final_text_checks:\n")
	fmt.Fprintf(b, "  chars=%d\n", utf8.RuneCountInString(text))
	flags := finalTextFlags(text)
	if len(flags) == 0 {
		b.WriteString("  flags=none\n")
	} else {
		b.WriteString("  flags=[" + strings.Join(flags, ", ") + "]\n")
	}
	b.WriteString("  source=" + finalTextSource(msgs) + "\n")
}

// finalAssistantText is the text of the last completed assistant row, or ""
// when the transcript has none.
func finalAssistantText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant && msgs[i].IsFinished() {
			return msgs[i].FullText()
		}
	}
	return ""
}

// finalTextFlags is the pure, ML-free quality check of a final answer.
//
// The predicates are NOT independent: a short fragment that also carries
// mojibake is "garbage_chars", not two overlapping labels, so the reviewer
// prompt gets ONE decisive diagnosis instead of a pile it has to interpret.
// The checks therefore run as an exclusive chain in a fixed priority — the
// specific diagnosis outranks the generic "it is short" one — and the order
// the flags would take in the output is the same fixed order. Every branch is
// reachable on its own: replacing any one of them with a bare `return nil`
// breaks its table case, because the case then falls through to the next
// (weaker) diagnosis.
func finalTextFlags(text string) []string {
	if strings.TrimSpace(text) == "" {
		return []string{"empty"}
	}
	if hasGarbageChars(text) {
		return []string{"garbage_chars"}
	}
	if unexpectedScript(text) {
		return []string{"unexpected_script"}
	}
	if repeatedLines(text) {
		return []string{"repeated_lines"}
	}
	if repeatedPhrases(text) {
		return []string{"repeated_phrases"}
	}
	if possiblyTruncated(text) {
		return []string{"possibly_truncated"}
	}
	if utf8.RuneCountInString(strings.TrimSpace(text)) < reviewEvidenceVeryShortRunes {
		return []string{"very_short"}
	}
	return nil
}

// hasGarbageChars is U+FFFD (a failed decode, or a model's own replacement
// character) or a control character that is not ordinary whitespace.
func hasGarbageChars(text string) bool {
	for _, r := range text {
		if r == utf8.RuneError {
			return true
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return true
		}
	}
	return false
}

// unexpectedScript reports text whose letters are mostly outside the two
// scripts a request and its answer are expected to use. Below 40 letters the
// ratio is noise (a lone identifier is not a switch to another script).
func unexpectedScript(text string) bool {
	letters, outside := 0, 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if !unicode.Is(unicode.Latin, r) && !unicode.Is(unicode.Cyrillic, r) {
			outside++
		}
	}
	if letters < 40 {
		return false
	}
	return outside*100 > letters*5
}

// repeatedLines reports a non-empty line of at least 20 runes that appears at
// least three times — the classic stuck-loop report.
func repeatedLines(text string) bool {
	seen := make(map[string]int)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		seen[line]++
		if utf8.RuneCountInString(line) >= 20 && seen[line] >= 3 {
			return true
		}
	}
	return false
}

// repeatedPhrases reports a 6-word shingle that appears at least three times.
func repeatedPhrases(text string) bool {
	words := strings.Fields(text)
	if len(words) < 6 {
		return false
	}
	seen := make(map[string]int)
	for i := 0; i+6 <= len(words); i++ {
		shingle := strings.Join(words[i:i+6], " ")
		seen[shingle]++
		if seen[shingle] >= 3 {
			return true
		}
	}
	return false
}

// reviewEvidenceTerminalRunes are the runes a finished sentence may end on.
var reviewEvidenceTerminalRunes = []rune{'.', '!', '?', '…', ')', '»', '"', '\'', '`', '*', '|', ':', '>', ']'}

// possiblyTruncated reports text that stops mid-sentence and is long enough
// for that to mean something.
func possiblyTruncated(text string) bool {
	if utf8.RuneCountInString(strings.TrimSpace(text)) < reviewEvidenceTruncationMinRunes {
		return false
	}
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return false
	}
	if strings.HasSuffix(trimmed, "```") {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(trimmed)
	return !slices.Contains(reviewEvidenceTerminalRunes, last)
}

// finalTextSource says which user row the final assistant turn was answering:
// a background-job notice (the r3 degeneracy, where a one-line reaction buried
// the real report) or an ordinary answer turn. The reviewer's own prompt row
// is never a source.
func finalTextSource(msgs []message.Message) string {
	lastAssistant := -1
	for i, msg := range msgs {
		if msg.Role == message.Assistant && msg.IsFinished() {
			lastAssistant = i
		}
	}
	if lastAssistant < 0 {
		return "answer_turn"
	}
	for i := lastAssistant - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.User {
			continue
		}
		if strings.Contains(msg.FullText(), reviewerPassMarker) {
			continue
		}
		if msg.BackgroundJobNotice {
			return "reaction_to_background_notice"
		}
		return "answer_turn"
	}
	return "answer_turn"
}

// truncateRunes cuts s to at most limit runes, marking the cut.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "(truncated)"
}
