package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// The Drain session policy: the implementation moved to
// drain_policy_component.go (the drainComponent host pattern); this file
// keeps the thin coordinator delegates.

func (c *coordinator) arbiterVerdict(ctx context.Context, sessionID string, site LaunchSite) (Verdict, TurnFacts, error) {
	return newDrainComponent(c).arbiterVerdict(ctx, sessionID, site)
}

func (c *coordinator) drainPolicy(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return newDrainComponent(c).drainPolicy(ctx, sessionID, spent)
}

func (c *coordinator) drainPermitted(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return newDrainComponent(c).drainPermitted(ctx, sessionID, spent)
}

func (c *coordinator) drainDecision(ctx context.Context, sessionID string, spent bool) (debt bool, v drainVerdict, err error) {
	return newDrainComponent(c).drainDecision(ctx, sessionID, spent)
}

func (c *coordinator) isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error) {
	return newDrainComponent(c).isDurableDelegationChild(ctx, sessionID)
}

func (c *coordinator) sessionDebtIsBGShellOnly(ctx context.Context, sessionID string) (bgOnly bool, err error) {
	return newDrainComponent(c).sessionDebtIsBGShellOnly(ctx, sessionID)
}

// Rate-limit (429 non-quota) wait budget for the coordinator retry loop.

// Exponential wait schedule: 10s x 3^n, n = prior rate-limit waits in this
// run. Vars, not consts, so tests can shrink them (see
// streamStallRetryBaseBackoff).
var (
	rateLimitBaseWait = 10 * time.Second
	rateLimitMaxWait  = 5 * time.Minute
	rateLimitBudget   = 30 * time.Minute
	rateLimitWaitTick = 500 * time.Millisecond
)

var errRateLimitBudgetExhausted = errors.New("coordinator: rate limit retry budget exhausted")

type rateLimitWaitObservationKey struct{}

type rateLimitWaitObservation func()

func withRateLimitWaitObservation(ctx context.Context, observe rateLimitWaitObservation) context.Context {
	return context.WithValue(ctx, rateLimitWaitObservationKey{}, observe)
}

// isRateLimitError reports whether err is a 429 that is NOT a quota wall —
// the overload class that deserves its own wait budget. fantasy.RetryError
// unwraps to its last error, so errors.As reaches the ProviderError through
// it.
func isRateLimitError(err error) bool {
	var pe *fantasy.ProviderError
	return errors.As(err, &pe) &&
		pe.StatusCode == http.StatusTooManyRequests &&
		!isQuotaLimit(pe)
}

// retryAfter reads the provider's Retry-After hint off the ProviderError's
// captured headers: "retry-after-ms" (integer milliseconds) first, then
// "retry-after" (float seconds, or an HTTP-date). Any positive result is
// capped at rateLimitMaxWait.
func retryAfter(err error) (time.Duration, bool) {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe.ResponseHeaders == nil {
		return 0, false
	}
	// Headers arrive canonical ("Retry-After") from net/http; match any case.
	get := func(name string) string {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, name) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	// Two passes so both keys present is deterministic.
	if ms := get("retry-after-ms"); ms != "" {
		if n, e := strconv.ParseInt(ms, 10, 64); e == nil {
			if n > 0 {
				capMS := int64(rateLimitMaxWait / time.Millisecond)
				if n > capMS {
					return rateLimitMaxWait, true
				}
				return capRateLimitWait(time.Duration(n) * time.Millisecond), true
			}
		} else if isPositiveDecimalInteger(ms) {
			return rateLimitMaxWait, true
		}
	}
	if ra := get("retry-after"); ra != "" {
		if f, e := strconv.ParseFloat(ra, 64); e == nil {
			if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
				return 0, false
			}
			maxSeconds := float64(rateLimitMaxWait) / float64(time.Second)
			if f >= maxSeconds || f > float64(math.MaxInt64)/float64(time.Second) {
				return rateLimitMaxWait, true
			}
			return time.Duration(f * float64(time.Second)), true
		}
		if t, e := http.ParseTime(ra); e == nil {
			if d := time.Until(t); d > 0 {
				return capRateLimitWait(d), true
			}
			return 0, false
		}
	}
	return 0, false
}

func isPositiveDecimalInteger(s string) bool {
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// capRateLimitWait clamps any wait to the per-wait cap.
func capRateLimitWait(d time.Duration) time.Duration {
	if d > rateLimitMaxWait {
		return rateLimitMaxWait
	}
	return d
}

// rateLimitWaitFor picks the wait before the next attempt: the provider's
// Retry-After hint if present, else exponential (10s x 3^priorWaits),
// always capped.
func rateLimitWaitFor(err error, priorWaits int) time.Duration {
	if wait, ok := retryAfter(err); ok {
		return wait
	}
	wait := rateLimitBaseWait
	if wait >= rateLimitMaxWait || priorWaits <= 0 {
		return capRateLimitWait(wait)
	}
	for i := 0; i < priorWaits && wait < rateLimitMaxWait; i++ {
		if wait > rateLimitMaxWait/3 {
			return rateLimitMaxWait
		}
		wait *= 3
	}
	return capRateLimitWait(wait)
}

// hasLiveTreeWork reports whether sessionID (or any session in its running
// delegation tree) still has undelivered work — signal that the operator is
// watching live output and the wait budget should pause. Liveness is a batch
// live read (own roots plus live delegation descendants, the R10b/R11G
// semantics). A FAILED read answers not-live HERE, against the seam's
// conservative rule, deliberately: this answer only decides whether the
// rate-limit wait budget may consume, and "pause forever on an unreadable
// source" made the 429 retry loop unbounded (F3 — the loop's only other exit
// is ctx cancellation). Worst case the budget exhausts and the run surfaces
// the ORIGINAL provider error; the durable batch work itself is untouched.
// Durable consumers (drain policy, CLI scope, supervision) keep their own
// conservative reads.
func (c *coordinator) hasLiveTreeWork(sessionID string) bool {
	own, descendants, err := coordinatorBatchCutoverFacts(c).LiveWorkSummary(context.Background(), sessionID)
	if err != nil {
		return false
	}
	return own > 0 || descendants > 0
}

// waitRateLimit sleeps out wait in tick-sized steps, returning early on ctx
// cancellation. While live tree work exists the budget does not consume;
// once it stops, remaining budget below rateLimitBudget is all that's left,
// and exhausting it fails with errRateLimitBudgetExhausted.
func (c *coordinator) waitRateLimit(ctx context.Context, sessionID string, wait time.Duration, spent *time.Duration) error {
	remaining := wait
	for remaining > 0 {
		live := c.hasLiveTreeWork(sessionID)
		if !live && *spent >= rateLimitBudget {
			return errRateLimitBudgetExhausted
		}
		step := rateLimitWaitTick
		if step > remaining {
			step = remaining
		}
		if !live && rateLimitBudget-*spent < step {
			step = rateLimitBudget - *spent
		}
		timer := time.NewTimer(step)
		if observe, ok := ctx.Value(rateLimitWaitObservationKey{}).(rateLimitWaitObservation); ok {
			observe()
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if !live {
			*spent += step
		}
		remaining -= step
	}
	return nil
}

// RateLimitNotifier is the optional coordinator extension the app installs
// to surface each provider rate-limit wait to the operator.
type RateLimitNotifier interface {
	SetRateLimitNotify(f func(sessionID string, until time.Time, attempt int))
}

// SetRateLimitNotify installs the per-wait callback.
func (c *coordinator) SetRateLimitNotify(f func(sessionID string, until time.Time, attempt int)) {
	if f == nil {
		return
	}
	c.rateLimitNotify.Store(&f)
}

// notifyRateLimitWait invokes the installed callback, if any.
func (c *coordinator) notifyRateLimitWait(sessionID string, until time.Time, attempt int) {
	if f := c.rateLimitNotify.Load(); f != nil {
		(*f)(sessionID, until, attempt)
	}
}

// notifyRateLimitWaitContext reports one provider rate-limit wait to the
// run's own sink when it has one, falling back to the legacy app-installed
// callback otherwise. Same line format the removed app-wide callback wrote.
func (c *coordinator) notifyRateLimitWaitContext(ctx context.Context, sessionID string, until time.Time, attempt int) {
	if w := RateLimitNotifySinkFrom(ctx); w != nil {
		fmt.Fprintf(w, "rush run: session %q waiting for provider rate limit until %s (attempt %d)\n",
			sessionID, until.Format("15:04"), attempt)
		return
	}
	c.notifyRateLimitWait(sessionID, until, attempt)
}

// Retry classification and continuation prompts for coordinator_run.go's
// run loop — split out because coordinator_run.go sits just under the
// repo's 1000-line limit (CLAUDE.md). Pure code move, no behavior change.

// continuationPrompt builds the follow-up prompt sent when a turn already
// produced partial content (text/reasoning/a tool call) before a transient
// provider failure interrupted it. Unlike the blind-retry path, this never
// resends the original prompt verbatim -- that would ask the model to
// redo work it may have already done, or contradict a tool call it already
// made. Instead it names the interruption explicitly and hands back
// whatever text the model had produced, mirroring the shape of the
// shouldSummarize continuation call in agent_turn.go (same idea -- "the
// previous attempt was cut short, continue it" -- for a different cause).
func continuationPrompt(originalPrompt string, partial message.Message) string {
	partialText := strings.TrimSpace(partial.FullText())
	if partialText == "" {
		// Progress was reasoning/a tool call only, no visible text yet --
		// nothing to quote back, so just name the interruption.
		return fmt.Sprintf("Your previous response was interrupted by a transient provider error (e.g. a stream stall or rate limit) before any text was produced. Continue working on the original request: `%s`", originalPrompt)
	}
	return fmt.Sprintf(
		"Your previous response to the request `%s` was interrupted by a transient provider error (e.g. a stream stall or rate limit) partway through. Here is what you had written so far:\n\n%s\n\nContinue exactly where you left off. Do not repeat the text above and do not restart the task from scratch.",
		originalPrompt, partialText,
	)
}

// Continuation prompt openings, shared between continuationPrompt (the
// producer) and IsContinuationPrompt (the recognizer the run loop's
// terminal reconciliation uses to find continuation chain boundaries in
// the committed history).
const (
	continuationPromptPrefixPartialText = "Your previous response to the request `"
	continuationPromptPrefixNoText      = "Your previous response was interrupted"
)

// IsContinuationPrompt reports whether text is a coordinator-generated
// continuation prompt (see continuationPrompt) — the fresh user message
// the transient-retry loop sends after an attempt was interrupted
// mid-stream. The run loop's terminal reconciliation walks the committed
// rows backward across these boundaries to reassemble a continuation
// chain's full text; a genuine user turn must end that walk, so the
// recognizer has to match the producer exactly. It lives next to
// continuationPrompt so any wording change updates both in one commit.
func IsContinuationPrompt(text string) bool {
	return strings.HasPrefix(text, continuationPromptPrefixPartialText) ||
		strings.HasPrefix(text, continuationPromptPrefixNoText)
}

// retryClass partitions a turn-terminating failure into "surface it" vs
// "transparently re-run it". See shouldRetryTurn for the policy.
type retryClass = drainctl.RetryClass

const (
	// classTerminal is an operator-actionable failure (quota wall, auth,
	// context overflow, bad request, user cancel) that must surface.
	classTerminal = drainctl.ClassTerminal
	// classTransient is a provider/network hiccup worth a re-run.
	classTransient = drainctl.ClassTransient
	// classRateLimit is a 429 non-quota overload: retryable with its own
	// dedicated wait budget.
	classRateLimit = drainctl.ClassRateLimit
)

// classifyProviderError delegates to the drainctl classifier. The seams
// carry the agent-package predicates: the WS-replay refusal and the hard
// provider-limit check, whose body is the original usage call shape
// (time.Now() and usagepolicy.DisplayZoneProvider read at the original point).
func classifyProviderError(err error) retryClass {
	return drainctl.ClassifyProviderError(err, errProviderPeakHours, drainctl.Seams{
		ReplayForbidden: modelkit.ResponsesWSReplayForbidden,
		HardProviderLimit: func(err error) bool {
			return usagepolicy.ClassifyHardProviderLimit(err, "", time.Now(), usagepolicy.DisplayZoneProvider) == usagepolicy.ProviderLimitHard
		},
	})
}

func isProviderClassifiable(err error) bool {
	return drainctl.IsProviderClassifiable(err, errProviderPeakHours)
}

// turnAttemptRefused reports whether err is an admission-shaped refusal:
// the call never started a provider turn, so there is no attempt outcome
// for the retry classifiers to act on. ErrSessionBusy (mailbox busy,
// agent_run.go) and ErrAgentShuttingDown (shutdown admission gate) are
// returned before any provider request begins; the OS session-lock busy
// error (runOwned) refuses after mailbox admission but before the first
// turn. R3-1: a refusal must never be reclassified as a transient turn
// failure — the stale/foreign message rows a concurrent caller left in
// the session are not this call's evidence.
func turnAttemptRefused(err error) bool {
	if errors.Is(err, ErrSessionBusy) || errors.Is(err, ErrAgentShuttingDown) {
		return true
	}
	var lockBusy *sessionlock.SessionLockBusyError
	return errors.As(err, &lockBusy)
}

// isQuotaLimit reports whether a 429 is a hard usage/quota wall (resets on
// the order of hours) rather than a momentary overload. The two share
// status 429, so we discriminate on the provider's message text.
func isQuotaLimit(providerErr *fantasy.ProviderError) bool {
	return usagepolicy.IsQuotaLimit(providerErr)
}

// turnMadeProgress reports whether the assistant message carries any real
// output — text, reasoning, or a tool call (even a partial one). A turn
// that made progress must never be re-run: it would duplicate work the
// user already has.
func turnMadeProgress(msg message.Message) bool {
	return strings.TrimSpace(msg.FullText()) != "" ||
		strings.TrimSpace(msg.FullReasoningText()) != "" ||
		len(msg.ToolCalls()) > 0
}

// shouldRetryStalledMessage decides whether a watchdog-stalled assistant
// message warrants re-running the turn. The retry exists to recover from
// turns where the provider never delivered anything; ANY content reaching
// the assistant — text, reasoning, even a half-emitted tool call — proves
// the server received and processed the prompt, and re-running would just
// duplicate the user message in the DB and burn tokens redoing work the
// user already (partially) has.
//
// Returns false for any non-stalled finish reason (including nil), so the
// caller can pass the last assistant message unconditionally.
func shouldRetryStalledMessage(msg message.Message) bool {
	fp := msg.FinishPart()
	if fp == nil {
		return false
	}
	if fp.Reason != message.FinishReasonError || fp.Message != streamStalledFinishTitle {
		return false
	}
	return !turnMadeProgress(msg)
}

// ownAttemptAssistantMessage returns the assistant message row THIS
// call's attempt wrote, identified by the ID its turn reported through
// SessionAgentCall.OnAssistantMessageCreated. The lookup is scoped to the
// session, so an ID belonging to any other session never qualifies. ok is
// false on a DB error or when no assistant row with that ID exists here
// (deleted by a concurrent compaction, or the attempt never wrote one) --
// callers treat that as "no owned evidence, nothing to retry".
func (c *coordinator) ownAttemptAssistantMessage(ctx context.Context, sessionID, msgID string) (message.Message, bool) {
	if c.messages == nil {
		return message.Message{}, false
	}
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return message.Message{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == msgID {
			return msgs[i], msgs[i].Role == message.Assistant
		}
	}
	return message.Message{}, false
}

// shouldRetryTurn decides whether a finished turn should be transparently
// re-run. A turn qualifies ONLY if it ended in error WITHOUT producing any
// content (so a re-run cannot clobber a partial answer) AND the failure is
// a transient provider/network hiccup rather than an operator-actionable
// condition. This generalizes the original stall-only retry: a watchdog
// stall is one transient class among several.
//
// Decision order:
//   - admission refusal (ErrSessionBusy/ErrAgentShuttingDown/OS session-lock
//     busy — turnAttemptRefused) → no attempt ran, don't retry
//   - no owned evidence (the attempt wrote no assistant row) → don't retry
//   - the attempt's own row: clean finish / user-cancel finish → don't retry
//   - turn produced any content                                 → don't retry
//   - persisted "Stream stalled" title (err is context.Canceled) → retry
//   - turn returned no error (empty-stream close)                → retry
//   - otherwise classify the returned error                      → transient/rate limit?
func (c *coordinator) shouldRetryTurn(ctx context.Context, sessionID string, err error, attemptAssistantMsgID string) bool {
	if modelkit.ResponsesWSReplayForbidden(err) {
		return false
	}
	// R3-1: an admission refusal means this call never started a provider
	// request — classify nothing, not even the message lookup.
	if turnAttemptRefused(err) {
		return false
	}
	// R3-1 (round 5): evidence ownership. The only assistant message this
	// call may classify is the row its own attempt wrote, reported via
	// OnAssistantMessageCreated. An empty ID means the attempt produced no
	// assistant row (refused before the turn, or it failed before the
	// first provider step) — nothing to act on. The session's last
	// assistant row at classification time may belong to a concurrent
	// caller whose turn interleaved after ours returned: a newer ID is
	// not ownership, so the session-wide last row is never consulted.
	if attemptAssistantMsgID == "" {
		return false
	}
	msg, ok := c.ownAttemptAssistantMessage(ctx, sessionID, attemptAssistantMsgID)
	if !ok {
		return false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return false
	}
	if turnMadeProgress(msg) {
		return false
	}
	if fp.Message == streamStalledFinishTitle {
		// A call armed with a positive CallOptions.IdleTimeout (`rush run
		// --idle-timeout`) wants a stall to end the run outright, not
		// retry — see IdleTimeout's doc in call_options.go.
		if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.IdleTimeout > 0 {
			return false
		}
		return true
	}
	if err == nil {
		return true
	}
	// rate limit are both retryable; everything terminal is not.
	return classifyProviderError(err) != classTerminal
}

// shouldContinueTurn decides whether a turn that already produced partial
// content (text/reasoning/a tool call) before failing should be resumed via
// a CONTINUATION prompt, as opposed to a blind resend (shouldRetryTurn) or
// a terminal failure. This is the path shouldRetryTurn deliberately never
// takes: turnMadeProgress(msg) == true makes shouldRetryTurn bail out, on
// the theory that a blind resend of the same prompt would either duplicate
// or discard that partial work. shouldContinueTurn covers exactly that
// case for a transient failure -- the real-world shape a watchdog stall
// or rate limit actually takes, since either can fire well after the model
// started streaming a reply.
//
// Returns the partial assistant message and true only when: no admission
// refusal is present (turnAttemptRefused — no attempt ran, R3-1), the
// attempt DID write an assistant row of its own and that row (not the
// session's last row, which a concurrent caller's newer turn can own) has
// FinishReasonError, it DID make progress, and the failure reads as
// transient -- either the persisted "Stream stalled" finish title
// (matching shouldRetryTurn's stall check), or a retryable
// classifyProviderError class (transient or rate limit). A nil err paired with
// progress is left alone (returns false): that combination doesn't
// correspond to any known transient signal (the nil-err retry path exists
// only for the empty-stream-close case, which by definition has no
// content), so it's surfaced rather than guessed at.
func (c *coordinator) shouldContinueTurn(ctx context.Context, sessionID string, err error, attemptAssistantMsgID string) (message.Message, bool) {
	if modelkit.ResponsesWSReplayForbidden(err) {
		return message.Message{}, false
	}
	// R3-1: an admission refusal means this call never started a provider
	// request — classify nothing, not even the message lookup.
	if turnAttemptRefused(err) {
		return message.Message{}, false
	}
	// R3-1 (round 5): evidence ownership — see shouldRetryTurn. The
	// continuation, if any, is built from the attempt's OWN partial row
	// even when a concurrent caller's newer message is the session's last
	// assistant row by the time classification runs.
	if attemptAssistantMsgID == "" {
		return message.Message{}, false
	}
	msg, ok := c.ownAttemptAssistantMessage(ctx, sessionID, attemptAssistantMsgID)
	if !ok {
		return message.Message{}, false
	}
	fp := msg.FinishPart()
	if fp == nil || fp.Reason != message.FinishReasonError {
		return message.Message{}, false
	}
	if !turnMadeProgress(msg) {
		return message.Message{}, false
	}
	if fp.Message == streamStalledFinishTitle {
		// See the matching check in shouldRetryTurn.
		if callOpts := callOptionsFrom(ctx); callOpts != nil && callOpts.IdleTimeout > 0 {
			return message.Message{}, false
		}
		return msg, true
	}
	if err == nil {
		return message.Message{}, false
	}
	if classifyProviderError(err) != classTerminal {
		return msg, true
	}
	return message.Message{}, false
}

// SetTurnRetryBackoffForTest shrinks the coordinator's first backoff before it
// retries a transient turn failure (10s in production), for tests in other
// packages that drive a real turn through a failing provider stub
// (`internal/app` cannot reach this package's _test.go files). The returned
// func restores the previous value; call it only after the App built under the
// shrunken value has shut down.
//
// It panics outside a test binary (testing.Testing()), so production code can
// never change the backoff.
func SetTurnRetryBackoffForTest(base time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("agent.SetTurnRetryBackoffForTest called outside a test binary")
	}
	old := modelkit.StreamStallRetryBaseBackoff
	modelkit.StreamStallRetryBaseBackoff = base
	return func() { modelkit.StreamStallRetryBaseBackoff = old }
}
