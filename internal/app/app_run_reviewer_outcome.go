package app

// The reviewer pass is an ADDITIVE observation about a run that already
// succeeded (A10): the executor's own answer stays the run's final_text and
// what the second model concluded lands beside it in the review field plus
// the parsed verdict. Nothing here may change the run's exit code — a
// wrapper branches on the exit code, and the verdict is one model's opinion
// of the work (docs/plans/2026-10-02-reviewer-pass-verification.md §5, §9).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// reviewVerdictPattern matches the verdict line the reviewer prompt demands
// as the review's first line: "VERDICT: PASS", "VERDICT: PASS_WITH_NOTES" or
// "VERDICT: FAIL". Real reviewer output ships that line in other shapes the
// strict pattern recorded as "unparsed" (the 2026-11-13 fix-audit: a Russian
// first line "ПРОВЕРКА: ПРОЙДЕНО С ЗАМЕЧАНИЯМИ"), so the pattern accepts:
//   - (?i): the keyword's case is format noise; "verdict: pass" and
//     "Verdict: Fail" mean what they say.
//   - ВЕРДИКТ / ПРОВЕРКА: the same line with the label word in Russian —
//     the labels the reviewer prompt itself names ("Зачёт", "Зачёт с
//     замечаниями", "Отказ") are matched in their Russian spelling too.
//   - The prefix [^\p{L}\p{N}_\r\n]* tolerates markdown markers in front of
//     the keyword ("**VERDICT: FAIL**", "## Verdict: PASS") on the SAME
//     line. It must not be \W*: Go's \W is ASCII-only, so it would also
//     swallow Cyrillic letters and newlines and let a quoted "VERDICT: PASS"
//     deep in a Russian review override the real first-line verdict.
//   - The trailing (?:$|[^\p{L}0-9_]) is the Unicode-safe end-of-token
//     anchor. Go's regexp \b is ASCII-only, so a word boundary after a
//     Cyrillic token never fires; the explicit class keeps "PASSED" and
//     "ЗАЧЁТЫ" from parsing as the shorter tokens they contain.
//
// The alternation order is deliberate: the longer tokens MUST come first,
// or PASS would swallow the prefix of PASS_WITH_NOTES and ПРОЙДЕНО the
// prefix of ПРОЙДЕНО С ЗАМЕЧАНИЯМИ. The FIRST match still wins, so a review
// that repeats a token in its body (quoting the prompt's rules, say) cannot
// change what the run recorded.
var reviewVerdictPattern = regexp.MustCompile(`(?mi)^[^\p{L}\p{N}_\r\n]*(?:VERDICT|ВЕРДИКТ|ПРОВЕРКА)\s*:\s*(PASS_WITH_NOTES|ПРОЙДЕНО С ЗАМЕЧАНИЯМИ|ЗАЧЁТ С ЗАМЕЧАНИЯМИ|ЗАЧЕТ С ЗАМЕЧАНИЯМИ|PASS|ПРОЙДЕНО|ЗАЧЁТ|ЗАЧЕТ|FAIL|ОТКАЗ|ПРОВАЛ)(?:$|[^\p{L}0-9_])`)

// parseReviewVerdict returns the review's verdict as the lowercase wire value
// RunResult.ReviewVerdict carries: "pass", "pass_with_notes" or "fail". The
// FIRST match wins, so a review that repeats the token in its body cannot
// change what the run recorded. The captured token is upper-cased before the
// switch because (?i) folds its case ("pass" and "зачёт" must not fall into
// fail). "" when the text carries no verdict line at all — the caller records
// "unparsed"; an unknown captured token also records "unparsed" instead of
// inventing a verdict.
func parseReviewVerdict(text string) string {
	m := reviewVerdictPattern.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	switch strings.ToUpper(m[1]) {
	case "PASS_WITH_NOTES", "ПРОЙДЕНО С ЗАМЕЧАНИЯМИ", "ЗАЧЁТ С ЗАМЕЧАНИЯМИ", "ЗАЧЕТ С ЗАМЕЧАНИЯМИ":
		return "pass_with_notes"
	case "PASS", "ПРОЙДЕНО", "ЗАЧЁТ", "ЗАЧЕТ":
		return "pass"
	case "FAIL", "ОТКАЗ", "ПРОВАЛ":
		return "fail"
	default:
		return ""
	}
}

// reviewerToolCalls counts the tool calls the REVIEW turn made itself: the
// ToolCall parts of assistant rows that come after the LAST user row
// carrying reviewerPassMarker (the review prompt). Whatever the
// orchestrator's earlier turns called is deliberately excluded — a pass has
// to be earned by this turn's own reads, and the transcript is the only
// witness that can say it happened (§1.1, §5).
//
// msgs are read in list order: message.Service.List is a deterministic
// oldest-first total order ((created_at ASC, rowid ASC) in
// internal/db/sql/messages.sql). A copy is sorted by CreatedAt anyway, so a
// caller handing over rows from any other source still gets the
// chronologically LAST marker row, with the caller's own order breaking
// ties (SliceStable) instead of a second, arbitrary reordering.
func reviewerToolCalls(msgs []message.Message) int {
	ordered := make([]message.Message, len(msgs))
	copy(ordered, msgs)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt < ordered[j].CreatedAt })

	markerAt := -1
	for i := range ordered {
		if ordered[i].Role != message.User {
			continue
		}
		if strings.Contains(messageText(ordered[i]), reviewerPassMarker) {
			markerAt = i
		}
	}
	if markerAt < 0 {
		// No review turn in this transcript: nothing the reviewer itself
		// did, which is exactly the "unverified" answer.
		return 0
	}
	calls := 0
	for i := markerAt + 1; i < len(ordered); i++ {
		if ordered[i].Role != message.Assistant {
			continue
		}
		for _, p := range ordered[i].Parts {
			if _, ok := p.(message.ToolCall); ok {
				calls++
			}
		}
	}
	return calls
}

func reviewerTurnPrompt(env RunEnv, ctx context.Context, sessionID string, basis *reviewBasis) string {
	if basis == nil {
		return reviewerPassPrompt
	}
	msgs, err := env.Messages().List(ctx, sessionID)
	if err != nil {
		slog.Warn("run: failed to read the session transcript for the review evidence; the reviewer prompt goes out without it",
			"session", sessionID, "err", err)
		return reviewerPassPrompt
	}
	sess, err := env.Sessions().Get(ctx, sessionID)
	if err != nil {
		slog.Warn("run: failed to read the session row for the review evidence; the reviewer prompt goes out without it",
			"session", sessionID, "err", err)
		return reviewerPassPrompt
	}
	after := readGitSnapshot(ctx, basis.WorkingDir)
	return reviewerPassPrompt + "\n\n" + buildReviewEvidence(basis, after, msgs, sess.Todos)
}

func reviewerSessionMode(cfg *config.Config) string {
	if cfg == nil || cfg.Options == nil {
		return "fresh"
	}
	switch cfg.Options.ReviewerSessionMode {
	case "fresh", "":
		return "fresh"
	case "legacy":
		return "legacy"
	default:
		return "fresh"
	}
}

func runFreshReviewerTurn(env RunEnv, ctx context.Context, parentID string, basis *reviewBasis, report string, overrides RunOverrides, failIfBusy bool, stderr io.Writer) (*RunResult, error) {
	childID, result, resultErr := runFreshReviewerPhase(env, ctx, parentID, basis, report, 1, overrides, failIfBusy, stderr)
	setReviewerResumeSession(result, resultErr, parentID, runRoleName(overrides))
	if result == nil {
		result = &RunResult{SessionID: parentID}
	}
	result.SessionID = parentID
	if childID != "" {
		result.ReviewerSessionID = childID
	}
	if resultErr != nil {
		if errors.Is(resultErr, ErrRunQueued) || errors.Is(resultErr, agent.ErrSessionBusy) || turnRefusedByOwner(resultErr) {
			return result, resultErr
		}
		if childID == "" {
			if ctx.Err() == nil {
				result.Warnings = append(result.Warnings, "reviewer setup failed: "+resultErr.Error())
				result.ReviewVerdict = "error"
				return result, nil
			}
			return result, resultErr
		}
		result.ReviewVerdict = "error"
		if ctx.Err() == nil {
			recordReviewFailure(result, stderr, resultErr)
		}
		return result, resultErr
	}
	if childID != "" && result.ReviewVerdict == "" {
		attachReview(env, ctx, result, childID, result.FinalText, stderr)
	}
	return result, nil
}

func reviewResultSessionID(r *RunResult, parentID string) string {
	if r != nil && r.ReviewerSessionID != "" {
		return r.ReviewerSessionID
	}
	return parentID
}

func runFreshReviewerAttempt(env RunEnv, ctx context.Context, parentID string, basis *reviewBasis, report string, attempt int, overrides RunOverrides, failIfBusy bool, stderr io.Writer) (*RunResult, error) {
	childID, result, err := runFreshReviewerPhase(env, ctx, parentID, basis, report, attempt, overrides, failIfBusy, stderr)
	if result == nil {
		result = &RunResult{SessionID: parentID}
	}
	result.SessionID = parentID
	if childID != "" {
		result.ReviewerSessionID = childID
	}
	if childID == "" && err != nil {
		if errors.Is(err, ErrRunQueued) || errors.Is(err, agent.ErrSessionBusy) || turnRefusedByOwner(err) || ctx.Err() != nil {
			return result, err
		}
		result.Warnings = append(result.Warnings, "reviewer setup failed: "+err.Error())
		result.ReviewVerdict = "error"
		return result, nil
	}
	if err != nil && result.ReviewVerdict == "error" {
		return result, err
	}
	return result, err
}

const reviewerPackageTokenTarget int64 = 30000

// runFreshReviewerPhase executes the automatic review in a clean child session.
func runFreshReviewerPhase(env RunEnv, ctx context.Context, parentID string, basis *reviewBasis, report string, attempt int, overrides RunOverrides, failIfBusy bool, stderr io.Writer) (string, *RunResult, error) {
	if reviewerSessionMode(env.Config.Config()) == "legacy" {
		return "", &RunResult{SessionID: parentID, Warnings: []string{"fresh reviewer phase disabled by legacy mode"}}, nil
	}
	// Fresh reviews execute in a child, but admission belongs to the parent
	// run just as it does for the legacy reviewer turn. Hold both in-process
	// mailbox ownership and the inter-process session lock until the child
	// pass has finished, so a competing owner cannot be bypassed.
	holdCtx, epoch, release, admitted := env.Coord().ReserveExclusive(ctx, parentID)
	if !admitted {
		if failIfBusy {
			return "", &RunResult{SessionID: parentID}, fmt.Errorf("session %q is already processing another request: %w", parentID, agent.ErrSessionBusy)
		}
		return "", &RunResult{SessionID: parentID}, fmt.Errorf("%w: reviewer turn for session %q", ErrRunQueued, parentID)
	}
	defer env.Coord().ReleaseExclusive(parentID, epoch, release)
	parentLock, lockErr := sessionlock.TryAcquireSessionLock(env.DataDir(), parentID)
	if lockErr != nil {
		return "", &RunResult{SessionID: parentID}, fmt.Errorf("reviewer turn for session %q: %w", parentID, lockErr)
	}
	defer func() { _ = parentLock.Release() }()
	if err := holdCtx.Err(); err != nil {
		return "", &RunResult{SessionID: parentID}, err
	}
	if basis == nil {
		basis = &reviewBasis{Prompt: "(original prompt unavailable)", WorkingDir: env.Config.WorkingDir()}
	}
	parent, err := env.Sessions().Get(ctx, parentID)
	if err != nil {
		warning := fmt.Sprintf("reviewer pass failed: read parent session: %v", err)
		return "", &RunResult{SessionID: parentID, Warnings: []string{warning}}, err
	}
	spent, err := env.Sessions().SubtreeSpent(ctx, parentID)
	if err != nil {
		warning := fmt.Sprintf("reviewer pass failed: read parent subtree spend: %v", err)
		return "", &RunResult{SessionID: parentID, Warnings: []string{warning}}, err
	}
	cap := parent.BudgetMaxCost
	if overrides.MaxCost > 0 {
		cap = overrides.MaxCost
	}
	remainingCost := cap - max(0, spent-parent.CostBase)
	if cap > 0 && remainingCost <= 0 {
		return "", &RunResult{SessionID: parentID, Warnings: []string{"reviewer pass skipped: parent cost budget exhausted"}}, nil
	}
	if overrides.MaxTokens > 0 {
		return "", &RunResult{SessionID: parentID, Warnings: []string{"reviewer pass skipped: token budget is finite but subtree token spend is unavailable"}}, nil
	}
	msgs, err := env.Messages().List(ctx, parentID)
	if err != nil {
		warning := fmt.Sprintf("reviewer pass failed: read parent transcript: %v", err)
		return "", &RunResult{SessionID: parentID, Warnings: []string{warning}}, err
	}
	terminalID := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.Assistant && msgs[i].IsFinished() {
			terminalID = msgs[i].ID
			break
		}
	}
	// The primary ExecuteRun result is the authoritative report for this
	// review. Some successful in-process runs can return that report before
	// terminal-message reconciliation has made a finished row available;
	// do not silently suppress the fresh review in that case. Use the report
	// as the deterministic identity input, while retaining the terminal ID
	// when the persisted message is available.
	if terminalID == "" {
		terminalID = "report:" + report
	}
	// The report is a required part of the bounded reviewer package; parent
	// conversation rows are not passed as transcript history to the child.
	prompt, warnings := boundedReviewerPackage(basis, report, msgs, parent.Todos)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", parentID, terminalID, attempt)))
	childID := "reviewer-" + hex.EncodeToString(sum[:16])
	child, err := env.Sessions().CreateTaskSession(ctx, childID, parentID, "Automatic reviewer pass")
	if err != nil {
		warning := fmt.Sprintf("reviewer pass failed: create child: %v", err)
		return "", &RunResult{SessionID: parentID, Warnings: []string{warning}}, err
	}
	reviewerOverrides := overrides
	reviewerOverrides.ModelRole = config.SelectedModelTypeReviewer
	reviewerOverrides.RequestedRole, reviewerOverrides.roleContract = "", nil
	reviewerOverrides.SystemPrompt = ""
	reviewerOverrides.ReasoningEffort = ""
	reviewerOverrides.SmartModel = ""
	reviewerOverrides.FastModel = ""
	reviewerOverrides.OnFinishHook = ""
	reviewerOverrides.DisableSubAgents = true
	reviewerOverrides.AggregationMode = ""
	reviewerOverrides.NoSupervision = true
	reviewerOverrides.SupervisionInterval = 0
	// Keep the caller's folder scope, trusted disk binding and restricted-run
	// policy intact; the reviewer-role tool filter further narrows access.
	if cap > 0 {
		reviewerOverrides.MaxCost = remainingCost
	} else {
		reviewerOverrides.MaxCost = 0
	}
	reviewCtx, cancel := context.WithTimeout(ctx, reviewerPassTimeout)
	defer cancel()
	// This child is an internal report/review container, not a titled human
	// session. Mark it as a background notice so the agent title hook skips
	// its fast-then-smart title-generation calls.
	reviewCtx = sessionprompt.WithBackgroundJobNotice(reviewCtx)
	res, runErr := executeRun(env, reviewCtx, RunRequest{Prompt: prompt, Overrides: reviewerOverrides, Mode: RunModeJSON, ContinueSessionID: child.ID, Stdout: io.Discard, Stderr: stderr, captureResult: true, drainTurn: true, deferReviewer: true})
	setReviewerResumeSession(res, runErr, parentID, runRoleName(overrides))
	if runErr != nil {
		if res == nil {
			res = &RunResult{SessionID: parentID}
		}
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) && ctx.Err() == nil {
			recordReviewFailure(res, stderr, runErr)
		} else {
			res.ReviewVerdict = "error"
			warning := fmt.Sprintf("reviewer pass failed: %v", runErr)
			if !slices.Contains(res.Warnings, warning) {
				res.Warnings = append(res.Warnings, warning)
			}
		}
	}
	if res == nil {
		res = &RunResult{SessionID: parentID}
	}
	res.ReviewerSessionID, res.SessionID = child.ID, parentID
	res.Warnings = append(res.Warnings, warnings...)
	return child.ID, res, runErr
}

type reviewerPackageParts struct {
	instructions, originalPrompt, ledger, report, diff, facts string
	cuts                                                      []string
}

func boundedReviewerPackage(basis *reviewBasis, report string, msgs []message.Message, todos []session.Todo) (string, []string) {
	if basis == nil {
		basis = &reviewBasis{Prompt: "(original prompt unavailable)"}
	}
	diff := reviewerDiffEvidence(basis)
	parts := reviewerPackageParts{instructions: reviewerInstructions(), originalPrompt: basis.Prompt, ledger: renderReviewerTodos(todos), report: report, diff: diff, facts: reviewerRunFacts(basis, msgs, todos)}
	if strings.Contains(diff, "evidence unavailable") || strings.Contains(diff, "contents unavailable") {
		parts.cuts = append(parts.cuts, "changed-file/diff evidence unavailable")
	}
	if strings.Contains(diff, reviewerDiffFallbackCut) {
		parts.cuts = append(parts.cuts, reviewerDiffFallbackCut)
	}
	if strings.Contains(diff, "changed paths list truncated") {
		parts.cuts = append(parts.cuts, "changed paths list truncated")
	}
	if strings.Contains(diff, "diff contents truncated at size cap") {
		parts.cuts = append(parts.cuts, "diff contents truncated at size cap")
	}
	render := func() string {
		return strings.Join([]string{parts.instructions, "original_prompt:\n" + parts.originalPrompt, "current_todos_and_acceptance:\n" + parts.ledger, "Orchestrator final report (claims, not verified facts):\n" + parts.report, "computed changes (before/after snapshots and diff):\n" + parts.diff, "run facts:\n" + parts.facts}, "\n\n")
	}
	factsOmitted := false
	if provauth.ApproxTokenCount(render()) > reviewerPackageTokenTarget && !factsOmitted {
		parts.facts = "[run facts and verbose outputs omitted]"
		parts.cuts = append(parts.cuts, "run facts/verbose outputs")
		factsOmitted = true
	}
	if provauth.ApproxTokenCount(render()) > reviewerPackageTokenTarget && !strings.Contains(parts.diff, "[diff details omitted") {
		parts.diff = reviewerChangedPathSummary(basis)
		parts.cuts = append(parts.cuts, "diff details/hunks; changed paths retained")
	}
	if provauth.ApproxTokenCount(render()) > reviewerPackageTokenTarget && len(parts.report) > 512 {
		parts.report = edgeTrim(parts.report, 512, "[report middle omitted; claim/conclusion edges retained]")
		parts.cuts = append(parts.cuts, "final report middle")
	}
	if provauth.ApproxTokenCount(render()) > reviewerPackageTokenTarget && len(parts.ledger) > 512 {
		parts.ledger = edgeTrim(parts.ledger, 512, "[ledger middle omitted; acceptance/todo edges retained]")
		parts.cuts = append(parts.cuts, "todo ledger middle")
	}
	if provauth.ApproxTokenCount(render()) > reviewerPackageTokenTarget && len(parts.originalPrompt) > 1024 {
		parts.originalPrompt = edgeTrim(parts.originalPrompt, 1024, "[original prompt middle omitted]")
		parts.cuts = append(parts.cuts, "original prompt middle")
	}
	prompt := render()
	warnings := []string{}
	if len(parts.cuts) > 0 || provauth.ApproxTokenCount(prompt) > reviewerPackageTokenTarget {
		warning := "reviewer package truncated; cuts: " + strings.Join(parts.cuts, ", ")
		if n := provauth.ApproxTokenCount(prompt); n > reviewerPackageTokenTarget {
			warning += fmt.Sprintf("; assembled estimate remains %d tokens (target %d)", n, reviewerPackageTokenTarget)
		}
		warnings = append(warnings, warning)
		// The truncation notice is appended after estimating package content; its small fixed overhead is intentionally outside the payload target.
		prompt += "\n\n[" + warning + "]"
	}
	return prompt, warnings
}

func reviewerInstructions() string {
	return reviewerPassMarker + ".\nROLE SWITCH: independently review this fresh-session package; no prior conversation or parent transcript is available. The labeled final report below is the orchestrator's claims, not verified facts.\nUse read-only view, grep, glob, ls, git_read, and read_delegation_transcript only. Do not edit, run commands/tests, delegate, or ask questions. Check request and acceptance/todo ledger against changed-file evidence and run facts; return the required first-line verdict."
}

func reviewerRunFacts(basis *reviewBasis, msgs []message.Message, todos []session.Todo) string {
	var b strings.Builder
	// Only summarize command exit states. Tool arguments/results and assistant
	// final text can quote the user prompt or transcript; the dedicated package
	// fields already include the original request and final report explicitly.
	writeEvidenceCommandSection(&b, msgs, basis.Start.Unix())
	writeEvidenceTodoSection(&b, todos)
	return b.String()
}

const (
	reviewerDiffPathLimit   = 40
	reviewerDiffRuneLimit   = 100000
	reviewerDiffFallbackCut = "base commit no longer an ancestor; status-based change selection used"
)

func reviewerDiffEvidence(basis *reviewBasis) string {
	if basis == nil || basis.WorkingDir == "" {
		return "changed-file/diff evidence unavailable: captured working directory unavailable"
	}
	after := readGitSnapshot(context.Background(), basis.WorkingDir)
	if !basis.Before.OK || !after.OK {
		return "changed-file/diff evidence unavailable: before/after snapshot unavailable"
	}
	paths, useBase := reviewerDiffPaths(basis, after)
	fallback := !useBase
	var diff string
	var err error
	if useBase {
		diff, err = evidenceGit(context.Background(), basis.WorkingDir, "-c", "core.quotepath=off", "-c", "diff.renames=true", "diff", basis.BaseHead)
	} else {
		args := append([]string{"-c", "core.quotepath=off", "diff", "HEAD", "--"}, paths...)
		diff, err = evidenceGit(context.Background(), basis.WorkingDir, args...)
	}
	shown, omitted := boundedReviewerDiffPaths(paths)
	var b strings.Builder
	fmt.Fprintf(&b, "changed paths: %s\n", strings.Join(shown, ", "))
	if omitted > 0 {
		fmt.Fprintf(&b, "... and %d more paths omitted\n[reviewer diff cut: changed paths list truncated]\n", omitted)
	}
	if fallback {
		fmt.Fprintf(&b, "[reviewer diff cut: %s]\n", reviewerDiffFallbackCut)
	}
	if err != nil {
		fmt.Fprintf(&b, "diff contents unavailable: %v", err)
		return b.String()
	}
	boundedDiff, truncated := truncateReviewerDiff(diff)
	b.WriteString(boundedDiff)
	if truncated {
		b.WriteString("\n[reviewer diff cut: diff contents truncated at size cap]")
	}
	return b.String()
}

func reviewerDiffPaths(basis *reviewBasis, after gitSnapshot) ([]string, bool) {
	fallback := changedDuringRun(basis.Before, after)
	if basis.BaseHead == "" {
		return fallback, false
	}
	if _, err := evidenceGit(context.Background(), basis.WorkingDir, "rev-parse", "HEAD"); err != nil {
		return fallback, false
	}
	if _, err := evidenceGit(context.Background(), basis.WorkingDir, "merge-base", "--is-ancestor", basis.BaseHead, "HEAD"); err != nil {
		return fallback, false
	}
	diff, err := evidenceGit(context.Background(), basis.WorkingDir, "-c", "core.quotepath=off", "-c", "diff.renames=true", "diff", basis.BaseHead)
	if err != nil {
		return fallback, false
	}
	return reviewerDiffHeaderPaths(diff), true
}

func reviewerDiffHeaderPaths(diff string) []string {
	seen := make(map[string]struct{})
	var paths []string
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "diff --git a/") {
			continue
		}
		rest := strings.TrimPrefix(line, "diff --git a/")
		sep := strings.LastIndex(rest, " b/")
		if sep < 0 {
			continue
		}
		for _, path := range []string{rest[:sep], rest[sep+3:]} {
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	return paths
}

func boundedReviewerDiffPaths(paths []string) ([]string, int) {
	if len(paths) <= reviewerDiffPathLimit {
		return paths, 0
	}
	return paths[:reviewerDiffPathLimit], len(paths) - reviewerDiffPathLimit
}

func truncateReviewerDiff(diff string) (string, bool) {
	runes := []rune(diff)
	if len(runes) <= reviewerDiffRuneLimit {
		return diff, false
	}
	return string(runes[:reviewerDiffRuneLimit]), true
}

func reviewerChangedPathSummary(basis *reviewBasis) string {
	if basis == nil || !basis.Before.OK {
		return "changed paths unavailable: before snapshot unavailable"
	}
	after := readGitSnapshot(context.Background(), basis.WorkingDir)
	if !after.OK {
		return "changed paths unavailable: after snapshot unavailable"
	}
	paths, useBase := reviewerDiffPaths(basis, after)
	shown, omitted := boundedReviewerDiffPaths(paths)
	var b strings.Builder
	fmt.Fprintf(&b, "changed paths: %s\n[diff details omitted; paths retained]", strings.Join(shown, ", "))
	if omitted > 0 {
		fmt.Fprintf(&b, "\n... and %d more paths omitted\n[reviewer diff cut: changed paths list truncated]", omitted)
	}
	if !useBase {
		fmt.Fprintf(&b, "\n[reviewer diff cut: %s]", reviewerDiffFallbackCut)
	}
	return b.String()
}

func edgeTrim(s string, keep int, marker string) string {
	if len(s) <= keep {
		return s
	}
	half := keep / 2
	return s[:half] + "\n" + marker + "\n" + s[len(s)-half:]
}

func renderReviewerTodos(todos []session.Todo) string {
	var b strings.Builder
	for _, t := range todos {
		fmt.Fprintf(&b, "- [%s] %s\n", t.Status, t.Content)
	}
	if len(todos) == 0 {
		return "(none recorded)"
	}
	return b.String()
}

func reviewerPackagePrompt(basis *reviewBasis, report string, msgs []message.Message, todos []session.Todo) string {
	return strings.Join([]string{reviewerInstructions(), "original_prompt:\n" + basis.Prompt, "current_todos_and_acceptance:\n" + renderReviewerTodos(todos), "Orchestrator final report (claims, not verified facts):\n" + report, "computed changes (before/after snapshots and diff):\n" + reviewerDiffEvidence(basis), "run facts:\n" + reviewerRunFacts(basis, msgs, todos)}, "\n\n")
}

// attachReview folds a completed review turn into the run's envelope: the
// review text itself, the verdict parsed out of it, and — for every verdict
// that is not a plain verified "pass" — one warning and one stderr line. It
// touches neither the run's exit code nor final_text (A10).
func attachReview(env RunEnv, ctx context.Context, final *RunResult, sessionID, reviewText string, stderr io.Writer) {
	if final == nil {
		return
	}
	final.Review = reviewText
	switch verdict := parseReviewVerdict(reviewText); verdict {
	case "":
		// No verdict line at all: record that the reviewer never answered
		// the question instead of inventing a pass.
		final.ReviewVerdict = "unparsed"
		final.Warnings = append(final.Warnings, "reviewer verdict line missing")
		reviewVerdictStderr(stderr, "unparsed")
	case "fail":
		final.ReviewVerdict = "fail"
		final.Warnings = append(final.Warnings, "reviewer verdict: FAIL - see review")
		reviewVerdictStderr(stderr, "fail")
	default: // "pass" | "pass_with_notes"
		verified := verdict
		// A pass without the reviewer's own read-tool check is the exact
		// failure mode this design exists for (#1165): it read the
		// orchestrator's claims and retold them. Downgrade it to
		// "unverified" rather than repeating the review turn.
		calls, known := reviewerToolCallCount(env, ctx, sessionID)
		switch {
		case known && calls == 0:
			verified = "unverified"
			final.Warnings = append(final.Warnings,
				"reviewer passed the run without any read-tool check; treat the review as unverified")
			reviewVerdictStderr(stderr, "unverified")
		}
		final.ReviewVerdict = verified
	}
}

// reviewerToolCallCount is the transcript read behind a pass verdict.
// known is false when the transcript could not be read — including when this
// App has no message store at all — because an unreadable transcript is not
// a reason to fail a run that already succeeded: the caller keeps the
// reviewer's claimed verdict.
func reviewerToolCallCount(env RunEnv, ctx context.Context, sessionID string) (calls int, known bool) {
	if env.Messages() == nil {
		return 0, false
	}
	// Verification of an already-completed review is a bounded local read independent of run cancellation.
	ctx2, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	msgs, err := env.Messages().List(ctx2, sessionID)
	if err != nil {
		slog.Warn("run: failed to read the session transcript to verify the reviewer verdict", "session", sessionID, "err", err)
		return 0, false
	}
	return reviewerToolCalls(msgs), true
}

// reviewFailureKeepsPrimary reports that a FAILED review turn must not
// replace the run's own answer: the executor already completed its turn
// cleanly, so a review that died for a reason of its own — a provider error,
// the reviewer turn's own timeout — leaves that answer as the run's outcome
// with ReviewVerdict="error" and a warning (§7). A run the caller canceled,
// a fail-fast busy refusal (R2-3) and a review that queued behind another
// owner (it never ran at all) are NOT this case: each is handled by its own
// branch before the failure fallback.
func reviewFailureKeepsPrimary(runCtx context.Context, err error) bool {
	return err != nil && runCtx.Err() == nil &&
		!errors.Is(err, ErrRunQueued) &&
		!errors.Is(err, agent.ErrSessionBusy) &&
		!turnRefusedByOwner(err)
}

// recordReviewFailure keeps a primary answer after its review turn failed:
// the verdict becomes "error", the run carries a warning, and the operator
// gets one stderr line. Used by both failure paths (ExecuteRun and the
// loop's closePhase) so they stay identical.
func recordReviewFailure(final *RunResult, stderr io.Writer, err error) {
	if final != nil {
		final.ReviewVerdict = "error"
		warning := fmt.Sprintf("reviewer pass failed: %v", err)
		if !slices.Contains(final.Warnings, warning) {
			final.Warnings = append(final.Warnings, warning)
		}
	}
	fmt.Fprintf(stderr, "rush run: reviewer pass failed: %v\n", err)
}

// reviewVerdictStderr is the ONE line the operator sees for a verdict that
// needs attention. A verified pass prints nothing.
func reviewVerdictStderr(stderr io.Writer, verdict string) {
	fmt.Fprintf(stderr, "rush run: reviewer verdict: %s (see the review field)\n", verdict)
}

// messageText is a message's plain text — used to recognise the review
// prompt marker inside a user row without pulling in the whole part zoo.
func messageText(msg message.Message) string {
	var b strings.Builder
	for _, p := range msg.Parts {
		if t, ok := p.(message.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// Durable-drain assistant identity reconciliation helpers, split out of
// app_run_reviewer.go (2026-09-23, file-size ratchet) as a pure move -- no
// behavior change, same package.
type drainCompletion struct {
	result              runq.DrainResult
	err                 error
	recorder            *agent.CallResultRecorder
	originalOwner       *agent.CallResultRecorder
	assistantIDs        map[string]struct{}
	terminalAssistantID string
}

func cloneAssistantIDSet(ids map[string]struct{}) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	clone := make(map[string]struct{}, len(ids))
	for id := range ids {
		clone[id] = struct{}{}
	}
	return clone
}

func recordDrainAssistantIDs(
	mu *sync.Mutex,
	recorder *agent.CallResultRecorder,
	confirmed map[string]struct{},
	baseline map[string]struct{},
	ids map[string]struct{},
	terminalID string,
) string {
	mu.Lock()
	defer mu.Unlock()
	for id := range ids {
		if id == "" {
			continue
		}
		if _, existed := baseline[id]; existed {
			continue
		}
		confirmed[id] = struct{}{}
		recorder.RecordConfirmed(id)
	}
	if terminalID == "" {
		return ""
	}
	if _, exists := ids[terminalID]; !exists {
		return ""
	}
	if _, existed := baseline[terminalID]; existed {
		return ""
	}
	if _, confirmedTerminal := confirmed[terminalID]; !confirmedTerminal {
		return ""
	}
	recorder.RecordConfirmed(terminalID)
	return terminalID
}

func (s *executeRunLoop) adoptDrainIdentity(completion drainCompletion) bool {
	if completion.result != runq.DrainComplete || completion.recorder == nil {
		s.eventOwner = completion.originalOwner
		return false
	}
	identity := completion.recorder.Resolve()
	if identity == "" || len(completion.assistantIDs) == 0 {
		s.eventOwner = completion.originalOwner
		return false
	}
	if _, confirmedTerminal := completion.assistantIDs[completion.terminalAssistantID]; !confirmedTerminal {
		s.eventOwner = completion.originalOwner
		return false
	}
	if identity != completion.terminalAssistantID || !completion.recorder.Owns(identity) {
		s.eventOwner = completion.originalOwner
		return false
	}
	recordedIDs := completion.recorder.IDs()
	if len(recordedIDs) != len(completion.assistantIDs) {
		s.eventOwner = completion.originalOwner
		return false
	}
	for id := range completion.assistantIDs {
		if id == "" || !completion.recorder.Owns(id) {
			s.eventOwner = completion.originalOwner
			return false
		}
	}
	for id := range recordedIDs {
		if _, confirmed := completion.assistantIDs[id]; !confirmed {
			s.eventOwner = completion.originalOwner
			return false
		}
	}
	s.callResultRec = completion.recorder
	s.eventOwner = completion.recorder
	return true
}

// Fork-only file (orchestrator UX): the sub-agent fan-out aggregation
// helper used by --aggregation=attach and by the reduction-loss
// warning. Inert to upstream — see CHANGELOG.fork.md (Section 4.K).
//
// Background: when a `rush run` model uses the `agent` tool to fan
// out, each dispatch lands a sub-session whose parent_session_id
// points at the calling session. After Run() returns, the parent
// composes a final assistant message that often *summarises* the
// sub-agent outputs instead of concatenating them verbatim. The
// orchestrator on top sees just final_text and never gets the lost
// detail. session-#3 (2026-05-17) audit feedback measured a 7×
// reduction in extreme cases (parent 1.4 KB vs sub-agent combined
// ~10 KB).
//
// Two consumers of the helper below:
//
//   1. collectSubAgentOutputs builds the SubAgentOutputs envelope
//      field when --aggregation=attach is on.
//   2. subAgentSummaryStats computes (count, totalChars) for the
//      "final_text is X% of combined sub-agent output" warning that
//      ALWAYS fires (no opt-in) when reduction looks dramatic.

// maxSubAgentTextChars caps how much of each sub-agent's final text
// we surface into the envelope. Anything bigger gets a trailing
// "[…truncated, see session <id> for full text]" so the envelope
// stays bounded. 64 KB per sub-agent × ~10 sub-agents = ~640 KB
// envelope worst case; orchestrators can paginate by querying the
// sub-sessions directly if they want more.
const maxSubAgentTextChars = 64 * 1024

// collectSubAgentOutputs queries the session DB for every sub-session
// whose parent is parentSessionID, then fetches each sub-session's
// last assistant FullText via the messages service. Truncates long
// outputs to maxSubAgentTextChars. Empty slice (not nil) is returned
// when there are no sub-sessions — keeps the envelope JSON shape
// consistent for orchestrators that always expect the array key.
//
// Failures to list messages for a single sub-session are logged at
// WARN and the entry is skipped — partial output is better than
// dropping the whole field on one bad row.
func collectSubAgentOutputs(env RunEnv, ctx context.Context, parentSessionID string) []SubAgentOutput {
	subs, err := env.Sessions().ListSubSessions(ctx, parentSessionID)
	if err != nil {
		slog.Warn("collectSubAgentOutputs: list sub-sessions failed",
			"parent", parentSessionID, "err", err)
		return []SubAgentOutput{}
	}
	out := make([]SubAgentOutput, 0, len(subs))
	for _, sub := range subs {
		msgs, mErr := env.Messages().List(ctx, sub.ID)
		if mErr != nil {
			slog.Warn("collectSubAgentOutputs: list messages failed",
				"sub_session", sub.ID, "err", mErr)
			continue
		}
		text := lastAssistantText(msgs)
		truncated := text
		if len(truncated) > maxSubAgentTextChars {
			truncated = truncated[:maxSubAgentTextChars] +
				"\n\n[…truncated, see session " + sub.ID + " for full text]"
		}
		out = append(out, SubAgentOutput{
			SessionID: sub.ID,
			Title:     sub.Title,
			FinalText: truncated,
			CharCount: len(text),
		})
	}
	return out
}

// lastAssistantText walks msgs newest-first (the messages slice is
// chronological, so we iterate from the end) and returns the FullText
// of the first finished assistant message it finds. Empty string when
// the sub-session never produced one (e.g. crashed before finishing).
func lastAssistantText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant {
			continue
		}
		if !m.IsFinished() {
			continue
		}
		return m.FullText()
	}
	return ""
}

// subAgentSummaryStats is the lightweight version: same query, but
// returns only (count, totalChars) and skips the per-row text copy
// + truncation work. Used by the always-on reduction-loss warning,
// which only needs to compare a ratio.
func subAgentSummaryStats(env RunEnv, ctx context.Context, parentSessionID string) (count int, totalChars int) {
	subs, err := env.Sessions().ListSubSessions(ctx, parentSessionID)
	if err != nil {
		return 0, 0
	}
	for _, sub := range subs {
		msgs, mErr := env.Messages().List(ctx, sub.ID)
		if mErr != nil {
			continue
		}
		text := lastAssistantText(msgs)
		if text == "" {
			continue
		}
		count++
		totalChars += len(text)
	}
	return count, totalChars
}
