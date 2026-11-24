package app

// The reviewer pass is an ADDITIVE observation about a run that already
// succeeded (A10): the executor's own answer stays the run's final_text and
// what the second model concluded lands beside it in the review field plus
// the parsed verdict. Nothing here may change the run's exit code — a
// wrapper branches on the exit code, and the verdict is one model's opinion
// of the work (docs/plans/2026-10-02-reviewer-pass-verification.md §5, §9).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
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
	msgs, err := env.Messages().List(ctx, sessionID)
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
		final.Warnings = append(final.Warnings, fmt.Sprintf("reviewer pass failed: %v", err))
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
