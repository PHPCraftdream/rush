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
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/vercel"
)

// cacheKeepAliveInterval is how long after a cache-writing turn we fire a
// replay to extend the TTL. Anthropic's ephemeral cache TTL is 5 minutes;
// this fires a bit early. A var (not const), per this codebase's test-seam
// idiom, so tests can shrink it instead of sleeping through the real delay.
var cacheKeepAliveInterval = 4*time.Minute + 45*time.Second

// cacheKeepAliveMaxExtensions caps how many times a single idle period can
// be extended (~15 minutes total) so a forgotten session cannot keep firing
// paid replay requests indefinitely.
var cacheKeepAliveMaxExtensions = 3

// cacheKeepAliveCallTimeout bounds each detached replay call. Independent of
// any turn's context — it must survive the triggering turn having long
// since ended.
var cacheKeepAliveCallTimeout = 30 * time.Second

// cacheKeepAliveEntry is the pending-timer state for one session.
type cacheKeepAliveEntry struct {
	timer     *time.Timer
	extension int
}

// cacheKeepAliveExplicitCacheProvider reports whether provider is one of the
// explicit prompt-cache providers getCacheControlOptions marks up. Mirrors
// that switch exactly — an implicit-cache provider gets zero benefit from a
// replay, so scheduling one would just be a wasted paid request.
func cacheKeepAliveExplicitCacheProvider(provider string) bool {
	switch provider {
	case anthropic.Name, bedrock.Name, vercel.Name:
		return true
	default:
		return false
	}
}

// scheduleCacheKeepAlive arms (or re-arms) a keep-alive timer for sessionID
// after a turn observed CacheCreationTokens > 0. A fresh cache write resets
// any existing timer and its extension counter — a genuine new turn already
// re-warmed the cache on its own.
func (a *sessionAgent) scheduleCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message) {
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return
	}
	if !cacheKeepAliveExplicitCacheProvider(model.Model.Provider()) {
		return
	}

	if old, ok := a.cacheKeepAlive.Take(sessionID); ok {
		old.timer.Stop()
	}

	entry := &cacheKeepAliveEntry{}
	entry.timer = time.AfterFunc(cacheKeepAliveInterval, func() {
		a.fireCacheKeepAlive(sessionID, model, messages, 0)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// fireCacheKeepAlive runs one detached replay call and, on success, reschedules
// itself until cacheKeepAliveMaxExtensions is reached.
func (a *sessionAgent) fireCacheKeepAlive(sessionID string, model Model, messages []fantasy.Message, extension int) {
	a.cacheKeepAlive.Del(sessionID)

	if !a.tryAdmitRunWg() {
		return
	}
	defer a.runWg.Done()

	ctx, cancel := context.WithTimeout(context.Background(), cacheKeepAliveCallTimeout)
	defer cancel()

	replayAgent := fantasy.NewAgent(
		model.Model,
		fantasy.WithMaxOutputTokens(1),
		fantasy.WithUserAgent(userAgent),
	)
	_, err := replayAgent.Stream(ctx, fantasy.AgentStreamCall{
		Messages: messages,
		Headers:  sessionHeaders(sessionID),
	})
	if err != nil {
		slog.Debug("cache keep-alive replay failed", "session_id", sessionID, "err", err)
		return
	}

	if extension+1 >= cacheKeepAliveMaxExtensions {
		return
	}
	entry := &cacheKeepAliveEntry{extension: extension + 1}
	entry.timer = time.AfterFunc(cacheKeepAliveInterval, func() {
		a.fireCacheKeepAlive(sessionID, model, messages, extension+1)
	})
	a.cacheKeepAlive.Set(sessionID, entry)
}

// cancelCacheKeepAlive stops and removes any pending keep-alive timer for
// sessionID. Called at the start of a genuine new turn: a real request
// already means the cache is about to be refreshed naturally, so any stale
// scheduled keep-alive is moot and must not race the real turn's own request.
func (a *sessionAgent) cancelCacheKeepAlive(sessionID string) {
	if entry, ok := a.cacheKeepAlive.Take(sessionID); ok {
		entry.timer.Stop()
	}
}
