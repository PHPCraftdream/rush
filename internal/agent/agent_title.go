// Session title generation: generateTitle (the background goroutine body),
// the title-related duration bounds, and the embedded prompt.
package agent

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
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
	ctx = callbeat.WithHeartbeat(ctx, sessionID, heartbeat.PurposeTitle, "fast", callbeat.HbFastSource(ctx, hb))

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
		MaxRetries: &agentcall.ProviderMaxRetries,
		Prompt:     fmt.Sprintf("Generate a concise title for the following content:\n\n%s\n <think>\n\n</think>", userPrompt),
		Headers:    sessionHeaders(sessionID),
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
			return context.WithValue(callCtx, cliprovider.TitleGenerationContextKey, true), prepared, nil
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
		if opts := turnstep.TitleProviderOptions(attempt.model); opts != nil {
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
		candidate := turnstep.CleanTitle(resp.Response.Content.Text())
		// A length-truncated reasoning stream ends mid-thought: its last line
		// is a fragment, not an answer, so the reasoning fallback is used ONLY
		// when reasoning actually completed. Length-truncated-but-textful
		// responses above are still accepted — text truncation keeps the
		// title readable, a truncated reasoning stream does not.
		if candidate == "" && reasoningLen > 0 && finishReason != fantasy.FinishReasonLength {
			// Thinking models can stream the whole answer as
			// reasoning_content with no text part at all; recover the title
			// from the reasoning stream (see turnstep.TitleFromReasoning).
			slog.Warn("Title model produced reasoning-only response; extracting title from reasoning",
				"model", attempt.name, "finish_reason", finishReason,
				"text_len", textLen, "reasoning_len", reasoningLen)
			candidate = turnstep.SanitizeReasoningTitle(turnstep.TitleFromReasoning(reasoning))
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
	usage := provauth.NormalizeProviderUsage(model.Model.Provider(), resp.TotalUsage)
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
	callbeat.AddHeartbeatUsage(ctx, model.Model.Provider(), model.Model.Model(), usage, cost)

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

// titleGenRegistry tracks the cancel funcs of in-flight title-generation
// goroutines.
//
// It exists because those goroutines' contexts are deliberately detached
// from the turn that spawned them (see startTitleGeneration) — nothing in
// the turn's own cancellation reaches them any more. CancelAll therefore
// needs its own hook, or a title provider that ignores cancellation would
// hold runWg (and with it App.Shutdown) for CancelAll's entire grace on
// every shutdown. register returns the id the goroutine must unregister on
// exit; cancelAll fires every live cancel and is safe to call on a nil
// registry (bare test fixtures that never construct one).
type titleGenRegistry struct {
	mu      sync.Mutex
	next    int64
	cancels map[int64]context.CancelFunc
}

func newTitleGenRegistry() *titleGenRegistry {
	return &titleGenRegistry{cancels: make(map[int64]context.CancelFunc)}
}

func (r *titleGenRegistry) register(cancel context.CancelFunc) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := r.next
	r.cancels[id] = cancel
	return id
}

func (r *titleGenRegistry) unregister(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, id)
}

// cancelAll fires every registered cancel. Invoked once by CancelAll, which
// is terminal for the agent, so the map is simply drained.
func (r *titleGenRegistry) cancelAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.cancels))
	for _, c := range r.cancels {
		cancels = append(cancels, c)
	}
	clear(r.cancels)
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// startTitleGeneration launches runTurn's title-generation goroutine when
// needsTitle is true and returns the channel it closes on exit (nil when no
// title was requested, matching the zero value of the local runTurn used to
// declare for this). Split out of agent_turn.go's runTurn when the
// 1000-line limit landed.
//
// Deliberately a free function rather than a method on turnTitleJoiner
// below: runTurn calls this BEFORE its stream watchdog exists (the
// watchdog needs no part of title generation to start), and only the JOIN
// needs the watchdog, to disarm it before waiting. Bundling launch and join
// into one constructor would force moving this call to after the watchdog
// is built, changing the relative order of two independent setup steps
// that this file's own history says not to move without a specific reason
// -- see turnTitleJoiner's doc.
//
// The title's context is deliberately detached from the turn's own
// cancellation: context.WithoutCancel(genCtx) keeps every value genCtx
// carries while making the context immune to the cancel() runTurn fires the
// instant it stops waiting for the title.
//
// Note WHY WithoutCancel and not "the turn's parent context": every context
// in this chain is turn-scoped. runTurn receives turnCtx, which runOwned's
// loop cancels the moment runTurn returns (agent_run.go's `turnCancel()`),
// and runCtx above it is cancelled by runOwned's own deferred runCancel.
// Deriving from either reproduces the bug on a shorter fuse — the title
// request is killed by the turn unwinding rather than by anything the user
// did.
//
// What detachment costs, and how it is paid for: a per-session interrupt
// (Ctrl-C, the web stop button, `sessions kill`) no longer aborts an
// in-flight title request either — it now stops on its own budget
// (effectiveTitleGenerationMaxDuration) instead. Agent SHUTDOWN still stops
// it, via titleGenRegistry below: CancelAll fires every registered cancel,
// so a title provider that ignores cancellation cannot hold App.Shutdown
// open for CancelAll's whole grace. The goroutine is also registered in
// runWg, which is what lets CancelAll join it at all (P0-4).
//
// This is the root-cause fix for "the session title never appears". The old
// code derived titleCtx from genCtx, so the moment runTurn's bounded join
// gave up (titleJoinGrace) and fell through to its explicit cancel(), the
// still-in-flight title request was killed mid-flight. generateTitle then
// logged "Error generating title with fast model; trying next
// err=context canceled", tried the smart model, got the same error, and gave
// up — and for a web session (born titled "New Session", see
// internal/server/handlers_sessions.go) the deferred "Untitled Session"
// fallback then declined to write, because it only stamps a slot it can
// VERIFY is still empty. Net result: no generated title at all, ever, on
// every session whose title provider took longer than the grace.
//
// Detaching the context turns that cliff into a delay: the title goroutine
// keeps its own budget (effectiveTitleGenerationMaxDuration) and its own
// result, and survives the turn that spawned it. The join below bounds only
// how long runTurn WAITS, never whether the title eventually lands.
func startTitleGeneration(a *sessionAgent, genCtx context.Context, needsTitle bool, sessionID, prompt string, cfg turnConfig) chan struct{} {
	if !needsTitle {
		return nil
	}
	titleCtx, titleCancel := context.WithTimeout(context.WithoutCancel(genCtx), a.effectiveTitleGenerationMaxDuration())
	done := make(chan struct{})
	// Safe without admission gate: runWg.Add(1) here is always called from
	// inside an already-admitted Run() call, so runWg counter is guaranteed
	// >= 1 at this point. Per sync.WaitGroup contract, Add(1) when counter
	// is > 0 may happen at any time, including concurrently with Wait. The
	// real race P1-1 closes is Add starting when counter is zero and Wait
	// starting between the check and the Add.
	titleGenID := a.titleGens.register(titleCancel)
	a.runWg.Add(1)
	go func() {
		defer close(done)
		defer a.runWg.Done()
		defer a.titleGens.unregister(titleGenID)
		defer titleCancel()
		a.generateTitle(titleCtx, sessionID, prompt, cfg)
	}()
	return done
}

// turnTitleJoiner owns the bounded join that waits for the title-generation
// goroutine startTitleGeneration launched. Unlike checkpoint/peak-hours/
// UI-notify this one carries a real historical fragility, so its
// extraction preserves the exact defer-registration SITE in runTurn, not
// just its behaviour: only the deferred call's target changed (a closure
// became a method value); the `defer` statement itself stays at the
// identical point in runTurn's sequence of defers, which is what the
// ordering below depends on.
//
// join() is a named function called from TWO places (task #525): once
// deferred, once explicit right before runTurn's own final cancel(). The
// join used to exist only as a defer, and runTurn calls cancel() EXPLICITLY
// near its end -- before any defer runs -- so on the success path titleCtx
// (derived from genCtx) was already cancelled by the time anything waited
// for it. The observed failure: a session left "Untitled Session" with
// "Error generating title with fast model; trying next err=context
// canceled" for both models. It surfaced as a ~1-in-27 flake only because
// the mock in the older test answers instantly and usually wins that race.
//
// So the success path joins BEFORE that explicit cancel, and the defer
// remains for every early return that never reaches it. sync.Once keeps a
// turn from paying the grace period twice.
//
// It stays bounded, which is what the original P1-B fix added:
// generateTitle's attempts are blocking agent.Stream calls with no timeout
// of their own, so a provider that ignores context cancellation never
// returns, and waiting unconditionally once held runTurn -- and with it
// Run, the session's mailbox ownership and its OS lock -- open forever on a
// turn whose work had finished.
//
// Abandoning the goroutine no longer loses the title. That used to be the
// trade (and why shortening this constant looked expensive): the title's
// context was derived from genCtx, so runTurn's cancel() -- which fires the
// instant the join gives up -- killed the in-flight request and the title
// with it. startTitleGeneration now derives the title's context with
// context.WithoutCancel, so the goroutine keeps running after the join
// gives up, finishes on its own budget, and still persists (and publishes)
// its result. What the grace now costs is only how long runTurn is held
// open past its own work, never whether the session ends up titled.
//
// disarm() is called FIRST inside the Once body, on every path that reaches
// it -- not just the success path -- because join() is called both
// explicitly (success path) and via the deferred call (every early return).
// The turn's real work is finished by the time ANY caller of join() runs;
// all that remains is this bounded wait and the eventual cancel().
// --timeout-hard-cap is absolute from turn start rather than idle-based, so
// without disarming first, the wait alone could push a turn that finished
// just inside the cap over it -- firing a stall dump for a turn that had
// already finished, and cancelling the very title being waited for. The
// goroutine still exits on genCtx and is still joined by the caller's own
// deferred `<-wd.Done` regardless of disarm.
type turnTitleJoiner struct {
	wd            watchdog.StreamWatchdog
	done          chan struct{}
	overrideGrace time.Duration
	sessionID     string

	once sync.Once
}

// Test-only seam; nil in production.
var turnTitleJoinAfterDisarmSeam func()

func newTurnTitleJoiner(wd watchdog.StreamWatchdog, done chan struct{}, overrideGrace time.Duration, sessionID string) *turnTitleJoiner {
	return &turnTitleJoiner{wd: wd, done: done, overrideGrace: overrideGrace, sessionID: sessionID}
}

func (j *turnTitleJoiner) join() {
	j.once.Do(func() {
		j.wd.Disarm()
		if turnTitleJoinAfterDisarmSeam != nil {
			turnTitleJoinAfterDisarmSeam()
		}
		if j.done == nil {
			return
		}
		grace := titleJoinGrace
		if j.overrideGrace > 0 {
			grace = j.overrideGrace
		}
		select {
		case <-j.done:
		case <-time.After(grace):
			// Not an error and not a loss: the goroutine keeps running on
			// its own budget and still persists/publishes the title when it
			// lands. Only THIS turn stops waiting for it.
			slog.Warn(
				"agent: title generation outlived the turn's grace — the turn is not held open for it, the title still lands on its own",
				"session_id", j.sessionID,
				"grace", grace,
			)
		}
	})
}
