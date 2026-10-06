// Session title generation: generateTitle (the background goroutine body),
// cleanTitle, the think-tag regexes, and the title-related duration bounds
// and embedded prompt.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

// titleGenerationMaxDurationDefault bounds how long the background
// title-generation goroutine (launched from runTurn, awaited by its `defer
// wg.Wait()`) is allowed to run. Title generation is a best-effort cosmetic
// side call (a.generateTitle tries up to two models, each a blocking
// agent.Stream with no timeout of its own) and must never be able to hold
// runTurn — and therefore Run() — open past its own turn. Its context is
// derived from genCtx so the stream watchdog's cancellation already covers
// it; this timer is the independent backstop for the case where genCtx's
// cancellation, for whatever reason, doesn't propagate (e.g. a provider
// stuck outside of context-aware I/O). Generous relative to a title's actual
// cost (a handful of tokens) so it only ever trips when something is
// genuinely wedged. Overridden per-agent via
// SessionAgentOptions.TitleGenerationMaxDuration, resolved at runTurn-time
// via effectiveTitleGenerationMaxDuration below.
const titleGenerationMaxDurationDefault = 2 * time.Minute

// titleJoinGrace bounds how long runTurn's join waits for the title
// goroutine AFTER that goroutine's own deadline should already have fired
// (P1-B).
//
// The timer above only bounds a provider that honours context
// cancellation. One that does not — a hung connection, a transport stuck
// outside context-aware I/O — never returns, and the bare `defer
// wg.Wait()` this replaces then held runTurn (and with it Run, the
// session's mailbox ownership and its OS lock) open indefinitely, for a
// turn whose real work had already completed.
//
// Abandoning the goroutine does not leak it: it exits whenever its provider
// finally unblocks, or when effectiveTitleGenerationMaxDuration expires.
//
// MEASURED (2026-08-19, task #546): with a wedged title provider, a turn
// whose MAIN provider returns HTTP 500 takes 6.26s, of which this grace is
// 5s. Note where that time is spent -- a provider error is recorded as a
// finish part and the turn completes NORMALLY, so it pays the full grace on
// the ordinary path. It does not take an early return, which is why
// splitting the budget by return path was tried and discarded: it would
// have bounded the rare paths and left the common one untouched.
//
// RE-EVALUATED (2026-09-25, session-title bug): the measurement above
// treated the grace as the whole cost, because back then hitting it LOST
// the title — the title's context was derived from the turn's genCtx, so
// the cancel() that follows a timed-out join killed the in-flight request
// and the session stayed untitled. That is what made shortening this
// constant look dangerous and kept it at 5s.
//
// startTitleGeneration now derives the title's context with
// context.WithoutCancel, so the grace no longer decides whether the title
// lands — only how long runTurn is held open past its own work, and how
// promptly the tab updates. 5s was still demonstrably too short for the
// common case: connection setup + time-to-first-token + a ~96-token title
// on a busy provider routinely exceeds it, and every one of those turns
// paid the grace in full for a title it then threw away. Raised to 10s,
// which covers the ordinary round-trip twice over while keeping the
// pathological wait bounded. If this needs to move again, move it here —
// SessionAgentOptions.TitleJoinGrace is the per-agent override.
const titleJoinGrace = 10 * time.Second

//go:embed templates/title.md
var titlePrompt []byte

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

// cleanTitle normalises a raw model title response: collapse newlines, strip
// any (orphan) think tags, and trim. Returns "" when nothing usable remains
// (e.g. a pure-reasoning response with no actual title text).
func cleanTitle(raw string) string {
	t := strings.ReplaceAll(raw, "\n", " ")
	t = thinkTagRegex.ReplaceAllString(t, "")
	t = orphanThinkTagRegex.ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

// titleFromReasoning extracts a title candidate from a reasoning-only
// response. Thinking models (z.ai/glm via the openai-compat provider) can
// stream their whole answer as `reasoning_content`, leaving
// ResponseContent.Text() empty; the final answer typically sits on the last
// non-empty line of the reasoning stream, so that line is the best title
// candidate.
func titleFromReasoning(reasoning string) string {
	lines := strings.Split(reasoning, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// titleMaxReasoningLen caps a title recovered from a reasoning line.
const titleMaxReasoningLen = 80

// sanitizeReasoningTitle cleans a title candidate recovered from a reasoning
// stream: strip answer prefixes ("Title:"/"Заголовок:" and friends) and
// wrapping quotes, then cap the length — reasoning lines can run long and a
// title must stay short. A cut longer than titleMaxReasoningLen falls back
// to the last space inside the window so no word is split mid-token.
func sanitizeReasoningTitle(raw string) string {
	t := cleanTitle(raw)
	for _, p := range []string{"Title:", "Заголовок:", "Title -", "Заголовок -"} {
		if len(t) >= len(p) && strings.EqualFold(t[:len(p)], p) {
			t = strings.TrimSpace(t[len(p):])
		}
	}
	t = strings.Trim(t, "\"'«»„“”")
	runes := []rune(t)
	if len(runes) <= titleMaxReasoningLen {
		return t
	}
	cut := titleMaxReasoningLen
	if sp := strings.LastIndex(string(runes[:titleMaxReasoningLen]), " "); sp > titleMaxReasoningLen/2 {
		cut = sp
	}
	return strings.TrimSpace(string(runes[:cut]))
}

// titleProviderOptions returns the request-level options that stop a
// thinking-by-default openai-compat provider (classified by the CONFIGURED
// provider ID, the same domain getProviderOptions switches on) from thinking
// on the title call. The prompt-level "/no_think" and "<think></think>"
// tricks are ignored by z.ai's GLM models, so without this the whole title
// answer streams as reasoning_content and Response.Content.Text() is empty.
// GLM-5.3 model IDs cannot disable thinking at all (see zai53ModelIDs), so
// they get their minimal level instead — mirroring getProviderOptions'
// "off" → "low" degradation. Any other provider keeps its default behavior.
func titleProviderOptions(model Model) *fantasy.ProviderOptions {
	if model.ModelCfg.Provider != string(catwalk.InferenceProviderZAI) {
		return nil
	}
	extraBody := map[string]any{}
	if zai53ModelIDs[strings.ToLower(model.ModelCfg.Model)] {
		extraBody["thinking"] = map[string]any{"type": "enabled"}
		extraBody["reasoning_effort"] = "low"
	} else {
		extraBody["thinking"] = map[string]any{"type": "disabled"}
	}
	parsed, err := openaicompat.ParseOptions(map[string]any{"extra_body": extraBody})
	if err != nil {
		return nil
	}
	return &fantasy.ProviderOptions{openaicompat.Name: parsed}
}

// generateTitle generates a session title based on the initial prompt.
//
// It runs detached from the turn that launched it: ctx is the title
// goroutine's own context (see startTitleGeneration — immune to the turn's
// cancel(), bounded by effectiveTitleGenerationMaxDuration), so this
// function may outlive runTurn and still persist its result. Every write it
// performs is a plain session-service call (Rename/IncrementCost/Get) — no
// mailbox, no OS lock, no turn state — which is what makes that safe and is
// why a title failure can never fail or delay the user's turn.
func (a *sessionAgent) generateTitle(ctx context.Context, sessionID string, userPrompt string, cfg turnConfig) {
	if userPrompt == "" {
		return
	}
	// Heartbeat attribution: the title slot is the fast model (design pt. 1).
	hb := heartbeat.FromContext(ctx)
	ctx = withHeartbeat(ctx, sessionID, heartbeat.PurposeTitle, "fast", hbFastSource(ctx, hb))

	// Ensure the session always gets a title even if every path below
	// fails or the context is cancelled before we finish. WithoutCancel so
	// the fallback still lands when the caller's ctx is already done.
	var titleSaved bool
	defer func() {
		if titleSaved {
			return
		}
		fallbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Only stamp the default when the session STILL has no real title.
		//
		// This used to be unconditional, which was safe only because runTurn
		// waited unconditionally for this goroutine: the write always landed
		// before the turn returned, so no later turn could have set a title
		// yet. Bounding that wait (P1-B) removed the ordering — a goroutine
		// whose provider ignores cancellation can now outlive its turn by
		// minutes. Unconditional, it would then overwrite a perfectly good
		// title a LATER turn had since generated; and because runTurn's
		// needsTitle test counts DefaultSessionName as "still needs one",
		// that becomes a loop, with every following turn spawning another
		// attempt for the abandoned one to clobber again.
		//
		// The re-read is what makes abandonment safe: a late finisher either
		// finds the slot still empty and fills it (the original intent) or
		// finds it taken and leaves it alone.
		// Fail CLOSED: if the slot cannot be VERIFIED empty, do not stamp it.
		// An earlier draft fell through to the rename whenever this read
		// errored, which reinstates the very clobber-and-loop being fixed —
		// and the read is most likely to fail exactly when it matters (the
		// 5s budget expiring, or the DB closing during shutdown, both of
		// which mean this goroutine has been abandoned and has no business
		// writing).
		sess, err := a.sessions.Get(fallbackCtx, sessionID)
		if err != nil {
			slog.Debug("agent: skipping fallback session title — could not confirm the title is still unset",
				"session_id", sessionID, "err", err)
			return
		}
		if sess.Title != "" && sess.Title != DefaultSessionName {
			return
		}
		if err := a.sessions.Rename(fallbackCtx, sessionID, DefaultSessionName); err != nil {
			slog.Error("Failed to save fallback session title", "error", err)
		}
	}()

	// From the turn's snapshot, not a fresh read of the shared fields — a
	// title generated for session A must not pick up session B's model just
	// because B applied an override while this goroutine was starting
	// (task #265).
	fastModel := cfg.fastModel
	smartModel := cfg.smartModel
	systemPromptPrefix := cfg.promptPrefix

	newAgent := func(m fantasy.LanguageModel, p []byte, tok int64) fantasy.Agent {
		return fantasy.NewAgent(
			m,
			fantasy.WithSystemPrompt(string(p)+"\n /no_think"),
			fantasy.WithMaxOutputTokens(tok),
			fantasy.WithUserAgent(userAgent),
		)
	}

	var reasoningBuf strings.Builder
	streamCall := fantasy.AgentStreamCall{
		Prompt:  fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		Headers: sessionHeaders(sessionID),
		// A reasoning-only stream (the whole answer as `reasoning_content`, no
		// text part) never flushes fantasy's ReasoningContent — the openaicompat
		// hook only emits ReasoningEnd when a content delta follows the reasoning
		// — so Response.Content would carry neither text nor reasoning. Capture
		// the reasoning deltas here instead so the fallback below can use them.
		OnReasoningDelta: func(_, text string) error {
			reasoningBuf.WriteString(text)
			return nil
		},
		PrepareStep: func(callCtx context.Context, opts fantasy.PrepareStepFunctionOptions) (_ context.Context, prepared fantasy.PrepareStepResult, err error) {
			prepared.Messages = opts.Messages
			if systemPromptPrefix != "" {
				prepared.Messages = append([]fantasy.Message{
					fantasy.NewSystemMessage(systemPromptPrefix),
				}, prepared.Messages...)
			}
			return callCtx, prepared, nil
		},
	}

	// Try the fast model first, then fall back to the smart one. A
	// response that hit the token limit (FinishReasonLength) is treated as
	// a failure so we retry rather than save a truncated title.
	type modelAttempt struct {
		name  string
		model Model
	}
	attempts := []modelAttempt{
		{"fast", fastModel},
		{"smart", smartModel},
	}

	var resp *fantasy.AgentResult
	var err error
	var model Model
	var title string
	var success bool
	for _, attempt := range attempts {
		// Non-reasoning models: a title is a handful of tokens, but GLM-style
		// models don't always honour /no_think and leak a short preamble — 40
		// tokens then truncates the title itself. 96 gives headroom while
		// staying cheap. Reasoning models get their full budget (the think
		// block is suppressed but still counts against the cap).
		tok := int64(96)
		if attempt.model.CatwalkCfg.CanReason {
			tok = attempt.model.CatwalkCfg.DefaultMaxTokens
		}
		agent := newAgent(attempt.model.Model, titlePrompt, tok)
		reasoningBuf.Reset()
		if opts := titleProviderOptions(attempt.model); opts != nil {
			streamCall.ProviderOptions = *opts
		} else {
			streamCall.ProviderOptions = nil
		}
		resp, err = agent.Stream(ctx, streamCall)
		if err != nil {
			slog.Error("Error generating title with "+attempt.name+" model; trying next", "err", err)
			continue
		}
		if resp == nil {
			slog.Error("Title generation returned nil response with " + attempt.name + " model; trying next")
			continue
		}
		// A length-truncated response usually still carries a usable title —
		// only a genuinely empty one (pure reasoning, no text) is a real miss.
		// Discarding a truncated-but-good title just retries the same tiny
		// budget on the next model, typically fails the same way, and leaves
		// the session "Untitled" for a transient reason.
		textLen := len(resp.Response.Content.Text())
		reasoning := cmp.Or(resp.Response.Content.ReasoningText(), reasoningBuf.String())
		reasoningLen := len(reasoning)
		finishReason := resp.Response.FinishReason
		candidate := cleanTitle(resp.Response.Content.Text())
		// A length-truncated reasoning stream ends mid-thought: its last line
		// is a fragment, not an answer, so the reasoning fallback is used ONLY
		// when reasoning actually completed. Length-truncated-but-textful
		// responses above are still accepted — text truncation keeps the
		// title readable, a truncated reasoning stream does not.
		if candidate == "" && reasoningLen > 0 && finishReason != fantasy.FinishReasonLength {
			// Thinking models can stream the whole answer as
			// reasoning_content with no text part at all; recover the title
			// from the reasoning stream (see titleFromReasoning).
			slog.Warn("Title model produced reasoning-only response; extracting title from reasoning",
				"model", attempt.name, "finish_reason", finishReason,
				"text_len", textLen, "reasoning_len", reasoningLen)
			candidate = sanitizeReasoningTitle(titleFromReasoning(reasoning))
		}
		if candidate == "" {
			slog.Warn("Title generation produced no usable text with "+attempt.name+" model; trying next",
				"finish_reason", finishReason,
				"text_len", textLen, "reasoning_len", reasoningLen)
			continue
		}
		if finishReason == fantasy.FinishReasonLength {
			slog.Debug("Title truncated (FinishReasonLength) but usable with " + attempt.name + " model")
		} else {
			slog.Debug("Generated title with " + attempt.name + " model")
		}
		title = candidate
		model = attempt.model
		success = true
		break
	}
	if !success {
		// The deferred fallback will save the default session name.
		return
	}

	title = cmp.Or(title, DefaultSessionName)

	// Calculate usage and cost.
	var openrouterCost *float64
	for _, step := range resp.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	// Google/OpenRouter/Vercel report InputTokens inclusive of cached tokens;
	// normalize before costing, same as the main turn (agent_turn.go) and the
	// keep-alive replay (recordCacheKeepAliveCost) already do.
	usage := normalizeProviderUsage(model.Model.Provider(), resp.TotalUsage)
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	// Use override cost if available (e.g., from OpenRouter).
	if openrouterCost != nil {
		cost = *openrouterCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}
	// Heartbeat usage: the model that actually answered (design pt. 4).
	addHeartbeatUsage(ctx, model.Model.Provider(), model.Model.Model(), usage, cost)

	// Rename and cost-accrual are intentionally two separate atomic calls,
	// not a single combined update. Title generation is a small side LLM
	// call that runs concurrently with the main turn (see the wg.Go call
	// site in Run): it must NOT add to prompt_tokens/completion_tokens,
	// since those columns are a snapshot of the main conversation's current
	// context-window size (see Service.SetUsage's doc comment) that the
	// main turn overwrites, not a cumulative counter. If title generation
	// added to them here, whichever of the two goroutines finishes last
	// would nondeterministically win: SetUsage's overwrite would erase this
	// addition, or this addition would land on top of a stale snapshot.
	// Cost, on the other hand, IS real money spent on a real API call, and
	// already has a dedicated atomic-additive path (IncrementCost) built
	// exactly for this class of "charge the session from a concurrent
	// goroutine" problem — so only cost is added here.
	//
	// Rename runs first: a title is the primary purpose of this function,
	// and the deferred fallback above only fires when titleSaved is still
	// false, so a Rename failure must leave titleSaved false to trigger the
	// "Untitled Session" fallback. IncrementCost runs regardless of the
	// Rename outcome so a title-generation API call is never left unbilled
	// just because the rename itself failed for an unrelated reason (e.g. a
	// transient DB error on that specific statement).
	renameErr := a.sessions.Rename(ctx, sessionID, title)
	if renameErr != nil {
		slog.Error("Failed to save session title", "error", renameErr)
	} else {
		titleSaved = true
	}

	if _, err := a.sessions.IncrementCost(ctx, sessionID, cost); err != nil {
		slog.Error("Failed to accrue title generation cost", "error", err)
	}
}
