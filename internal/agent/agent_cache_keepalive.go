// Cache keep-alive: after a turn that wrote to the provider's ephemeral
// prompt cache, schedule a lightweight detached "replay" request shortly
// before the cache TTL expires, to extend it. Bounded by
// cacheKeepAliveMaxExtensions so a session cannot keep paying for this
// forever.
package agent

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"charm.land/fantasy"
)

// cacheKeepAliveDefaultInterval is the fallback used when a keep-alive-
// eligible provider's profile has no TTL (ttl == 0) — unreachable today
// since every keepAliveEligible profile sets ttl=5m, kept as a safety net.
const cacheKeepAliveDefaultInterval = 4*time.Minute + 45*time.Second

// cacheKeepAliveTTLMargin is how long before a provider's cache TTL expires
// we fire the replay.
const cacheKeepAliveTTLMargin = 15 * time.Second

// cacheKeepAliveInterval, when nonzero, overrides the per-provider TTL-derived
// interval — the package's test-seam idiom, so tests can shrink it instead of
// sleeping through the real delay. Zero (the default) means "derive from the
// triggering provider's cache profile TTL", via cacheKeepAliveIntervalFor.
var cacheKeepAliveInterval time.Duration

// cacheKeepAliveIntervalFor returns how long after a cache-writing turn to
// fire a keep-alive replay for provider. Respects the cacheKeepAliveInterval
// test-seam override when set; otherwise derives from the provider's cache
// profile TTL (falling back to cacheKeepAliveDefaultInterval if the profile
// has no TTL).
func cacheKeepAliveIntervalFor(provider string) time.Duration {
	if cacheKeepAliveInterval != 0 {
		return cacheKeepAliveInterval
	}
	ttl := cacheProfileFor(provider).ttl
	if ttl == 0 {
		return cacheKeepAliveDefaultInterval
	}
	return ttl - cacheKeepAliveTTLMargin
}

// cacheKeepAliveMaxExtensions caps how many times a single idle period can
// be extended (~15 minutes total) so a forgotten session cannot keep firing
// paid replay requests indefinitely.
var cacheKeepAliveMaxExtensions = 3

// cacheKeepAliveCallTimeout bounds each detached replay call. Independent of
// any turn's context — it must survive the triggering turn having long
// since ended.
var cacheKeepAliveCallTimeout = 30 * time.Second

// cacheKeepAliveFireSeam is a test-only hook — see its call site in
// fireCacheKeepAlive. nil (a no-op) in every production path.
var cacheKeepAliveFireSeam func()

// cacheKeepAliveEntry is the pending-timer state for one session. It holds
// only what the generation-guard compare-and-act sequences actually read
// back (timer, generation) plus the extension counter — messages, tools,
// providerOptions, and maxCost are NOT duplicated here: the armed
// time.AfterFunc closure already captures everything fireCacheKeepAlive
// needs to run, so storing them a second time on this struct would just be
// redundant memory held per idle session (messages in particular can be a
// full conversation history).
type cacheKeepAliveEntry struct {
	timer      *time.Timer
	extension  int
	generation int64
}

// cacheKeepAliveExplicitCacheProvider reports whether provider is one of the
// explicit prompt-cache providers getCacheControlOptions marks up. Mirrors
// that switch exactly — an implicit-cache provider gets zero benefit from a
// replay, so scheduling one would just be a wasted paid request.
func cacheKeepAliveExplicitCacheProvider(provider string) bool {
	return cacheProfileFor(provider).keepAliveEligible
}

// scheduleCacheKeepAlive arms (or re-arms) a keep-alive timer for sessionID
// after a turn observed CacheCreationTokens > 0. A fresh cache write resets
// any existing timer and its extension counter — a genuine new turn already
// re-warmed the cache on its own.
//
// tools and providerOptions are the exact values the triggering turn passed
// to fantasy.NewAgent/AgentStreamCall, so the replay can reproduce the same
// request shape. systemPrompt is NOT re-applied via fantasy.WithSystemPrompt
// on the replay: fantasy's createPrompt already folds WithSystemPrompt into
// the messages it hands PrepareStep as a MessageRoleSystem entry, and
// stepMessages (what scheduleCacheKeepAlive is called with) is cloned from
// exactly those already-prepared messages — re-adding WithSystemPrompt would
// duplicate the system message in the replay's prompt.
func (a *sessionAgent) scheduleCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, maxCost float64) {
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return
	}
	// Opt-in: the replay must reproduce the exact cached request shape (task
	// #761) — off by default until that is verified in production.
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_CACHE_KEEPALIVE")); !t {
		return
	}
	if !cacheKeepAliveExplicitCacheProvider(model.Model.Provider()) {
		return
	}

	a.cacheKeepAliveMu.Lock()
	defer a.cacheKeepAliveMu.Unlock()

	if old, ok := a.cacheKeepAlive.Take(sessionID); ok {
		old.timer.Stop()
	}

	gen := a.cacheKeepAliveGen.Add(1)
	entry := &cacheKeepAliveEntry{generation: gen}
	entry.timer = time.AfterFunc(cacheKeepAliveIntervalFor(model.Model.Provider()), func() {
		a.fireCacheKeepAlive(sessionID, model, messages, tools, providerOptions, 0, gen, maxCost)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// fireCacheKeepAlive runs one detached replay call and, on success, reschedules
// itself until cacheKeepAliveMaxExtensions is reached. gen is the generation
// captured when this fire's timer was armed: if a newer schedule has since
// replaced the map entry (time.Timer.Stop cannot cancel an already-fired or
// already-running callback), this fire is a lost-race survivor and must not
// touch the map or send a replay — doing so could delete/clobber the newer
// entry with stale data from this, older, turn.
//
// maxCost mirrors the triggering turn's SessionAgentCall.MaxCost (agent_turn.go's
// own abort check): checked FIRST, before the tryAdmitRunWg gate, since it is
// the cheapest possible way to skip a replay that must not happen — no point
// admitting into runWg or building a replay agent for a session already at
// its cap. Unlike a failed replay, a cost-capped skip does not reschedule,
// matching a real turn's max-cost abort being terminal rather than retried.
func (a *sessionAgent) fireCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message, tools []fantasy.AgentTool, providerOptions fantasy.ProviderOptions, extension int, gen int64, maxCost float64) {
	// Built before the lock so it is ready to register atomically with the
	// pending-entry removal below — see the in-flight-registration comment.
	ctx, cancel := context.WithTimeout(context.Background(), cacheKeepAliveCallTimeout)

	a.cacheKeepAliveMu.Lock()
	current, ok := a.cacheKeepAlive.Get(sessionID)
	if !ok || current.generation != gen {
		a.cacheKeepAliveMu.Unlock()
		cancel()
		return
	}
	a.cacheKeepAlive.Del(sessionID)
	// Registered in the SAME critical section as the Del above — not after
	// the maxCost check/tryAdmitRunWg/replay setup that used to follow it.
	// That gap let cancelCacheKeepAlive/CancelAll observe a window where
	// NEITHER the pending entry NOR the in-flight cancel existed for a fire
	// that had already committed to running: a cancel landing there was
	// silently lost, letting a stale replay run concurrently with (and later
	// re-arm on top of) the new turn it should have yielded to. Atomic
	// registration closes that gap — any observer taking cacheKeepAliveMu
	// after this point sees either the pending entry (not yet fired) or the
	// in-flight cancel (already committed), never neither.
	a.cacheKeepAliveInFlight.Set(sessionID, cancel)
	a.cacheKeepAliveMu.Unlock()

	// Test-only seam: fires right after the atomic registration above, before
	// the maxCost check/tryAdmitRunWg/replay call below — lets a test land a
	// concurrent cancelCacheKeepAlive deterministically inside the exact span
	// that used to be an unregistered gap. nil (a no-op) in every production
	// path.
	if cacheKeepAliveFireSeam != nil {
		cacheKeepAliveFireSeam()
	}

	defer func() {
		a.cacheKeepAliveMu.Lock()
		a.cacheKeepAliveInFlight.Del(sessionID)
		a.cacheKeepAliveMu.Unlock()
		cancel()
	}()

	if maxCost > 0 {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sess, err := a.sessions.Get(checkCtx, sessionID)
		checkCancel()
		if err != nil {
			slog.Debug("cache keep-alive: failed to load session for max-cost check", "session_id", sessionID, "err", err)
			return
		}
		if sess.Cost >= maxCost {
			slog.Debug("cache keep-alive: skipping replay, session at or over max cost", "session_id", sessionID, "cost", sess.Cost, "max_cost", maxCost)
			return
		}
	}

	if !a.tryAdmitRunWg() {
		return
	}
	defer a.runWg.Done()

	replayAgent := fantasy.NewAgent(
		model.Model,
		fantasy.WithTools(tools...),
		fantasy.WithMaxOutputTokens(1),
		fantasy.WithUserAgent(userAgent),
	)
	result, err := replayAgent.Stream(ctx, fantasy.AgentStreamCall{
		Messages:        messages,
		Headers:         sessionHeaders(sessionID),
		ProviderOptions: providerOptions,
	})
	if err != nil {
		slog.Debug("cache keep-alive replay failed", "session_id", sessionID, "err", err)
		return
	}

	if !a.recordCacheKeepAliveCost(sessionID, model, result, maxCost) {
		// The session crossed maxCost while this replay was in flight (or
		// the re-check itself couldn't be verified) — treated the same as
		// the up-front max-cost skip: no charge, no rearm.
		return
	}

	if extension+1 >= cacheKeepAliveMaxExtensions {
		return
	}
	// A cancelCacheKeepAlive call landing AFTER Stream already returned
	// successfully but BEFORE this rearm decision would find the in-flight
	// entry still registered (its own defer hasn't run yet) and cancel this
	// fire's ctx — a no-op on the already-finished call, but ctx.Err() still
	// reports it, which the generation check below cannot: a new turn hasn't
	// necessarily scheduled its own keep-alive yet, so "no entry present" is
	// indistinguishable from "genuinely idle" without this explicit signal.
	if ctx.Err() != nil {
		slog.Debug("cache keep-alive: cancelled after replay completed, not rearming", "session_id", sessionID)
		return
	}

	a.cacheKeepAliveMu.Lock()
	defer a.cacheKeepAliveMu.Unlock()
	// A concurrent schedule/cancel may have landed while the replay call was
	// in flight — only re-arm if this fire's generation is still the map's
	// current occupant (i.e. nothing newer has since claimed the session).
	if current, ok := a.cacheKeepAlive.Get(sessionID); ok && current.generation != gen {
		return
	}
	newGen := a.cacheKeepAliveGen.Add(1)
	entry := &cacheKeepAliveEntry{extension: extension + 1, generation: newGen}
	entry.timer = time.AfterFunc(cacheKeepAliveIntervalFor(model.Model.Provider()), func() {
		a.fireCacheKeepAlive(sessionID, model, messages, tools, providerOptions, extension+1, newGen, maxCost)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// recordCacheKeepAliveCost bills a successful replay's usage to the session,
// mirroring agent_title.go's generateTitle cost accounting exactly. Only
// IncrementCost is called — NOT sessions.SetUsage/PromptTokens/CompletionTokens
// — because those columns are a snapshot of the main conversation's current
// context-window size (see updateSessionTokenCounters' doc) that the next
// real turn overwrites, not a cumulative counter. A background replay adding
// to them here would corrupt that snapshot: whichever of the replay and the
// next real turn's own usage update lands last would silently win.
//
// Returns false when maxCost blocked the charge — the session crossed its
// cap sometime between fireCacheKeepAlive's up-front check and the replay
// actually completing (a real, if narrow, TOCTOU window: the up-front check
// only bounds the wait BEFORE the up-to-30s replay call, not the charge
// after it). The caller must treat false the same as the up-front skip: no
// charge, and no rearm — a session already over its cap must not keep
// spending on future keep-alive extensions either.
func (a *sessionAgent) recordCacheKeepAliveCost(sessionID string, model Model, result *fantasy.AgentResult, maxCost float64) bool {
	if result == nil {
		return true
	}
	usage := normalizeProviderUsage(model.Model.Provider(), result.TotalUsage)

	var openrouterCost *float64
	for _, step := range result.Steps {
		stepCost := a.openrouterCost(step.ProviderMetadata)
		if stepCost != nil {
			newCost := *stepCost
			if openrouterCost != nil {
				newCost += *openrouterCost
			}
			openrouterCost = &newCost
		}
	}

	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	if openrouterCost != nil {
		cost = *openrouterCost
	}
	if model.FlatRate {
		cost = 0
	}
	if cost == 0 {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if maxCost > 0 {
		sess, err := a.sessions.Get(ctx, sessionID)
		if err != nil {
			slog.Debug("cache keep-alive: failed to re-verify max cost before charging; skipping charge", "session_id", sessionID, "err", err)
			return false
		}
		if sess.Cost >= maxCost {
			slog.Debug("cache keep-alive: session crossed max cost while the replay was in flight; skipping charge", "session_id", sessionID, "cost", sess.Cost, "max_cost", maxCost)
			return false
		}
	}

	if _, err := a.sessions.IncrementCost(ctx, sessionID, cost); err != nil {
		slog.Error("cache keep-alive: failed to accrue replay cost", "session_id", sessionID, "err", err)
		return true
	}
	slog.Debug("cache keep-alive: replay cost recorded", "session_id", sessionID, "cost", cost)
	return true
}

// cancelCacheKeepAlive stops and removes any pending keep-alive timer for
// sessionID, AND cancels any replay call already in flight for it. Called at
// the start of a genuine new turn: a real request already means the cache is
// about to be refreshed naturally, so any stale scheduled or in-flight
// keep-alive is moot and must not race the real turn's own request — without
// this, an in-flight replay (bounded only by cacheKeepAliveCallTimeout, 30s)
// would otherwise keep running underneath the new turn.
func (a *sessionAgent) cancelCacheKeepAlive(sessionID string) {
	// Both maps checked under ONE lock hold, not two separate cycles: since
	// fireCacheKeepAlive now moves an entry from "pending" to "in-flight"
	// atomically (see its own comment), a single hold here is guaranteed to
	// see a consistent snapshot — the pending entry, the in-flight cancel, or
	// neither (genuinely idle / already fully finished), never a torn state
	// in between.
	a.cacheKeepAliveMu.Lock()
	entry, hasPending := a.cacheKeepAlive.Take(sessionID)
	cancel, hasInFlight := a.cacheKeepAliveInFlight.Take(sessionID)
	a.cacheKeepAliveMu.Unlock()

	if hasPending {
		entry.timer.Stop()
	}
	if hasInFlight {
		cancel()
	}
}
