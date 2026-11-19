// Activity-notify and watchdog-bump context plumbing, moved verbatim from
// internal/agent/agent_ownership.go (target_owner=provauth). Package agent
// keeps thin delegates to these so existing callers are untouched.
package provauth

import (
	"context"

	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
)

// activityNotifyContextKey is the context key under which runTurn stores a
// composed "activity happened" callback (see WithActivityNotify). It is
// unexported: nothing outside this package needs to read or set it directly —
// sub-agent delegation (the "agent" tool, agentic_fetch_tool) just forwards
// ctx unmodified, which is enough for the value to keep flowing.
type activityNotifyContextKey struct{}

// watchdogBumpContextKey is the context key for storing the stream watchdog's
// bump function. Used by runSummarizeBody and runSummarizeSilent to report
// LLM streaming progress to the watchdog so healthy long-running compaction
// doesn't get falsely killed as "no provider activity" (task #310).
type watchdogBumpContextKey struct{}

// WithActivityNotify returns a child of ctx carrying a composed
// activity-notify callback: calling the returned callback records activity
// on lk (nil-receiver-safe, see SessionLock.RecordActivity) AND forwards to
// any ancestor notify callback already present on ctx.
//
// The "read the ancestor before overwriting" composition is what makes
// multi-level delegation chains (grandparent -> parent -> child) work: each
// level's runTurn calls WithActivityNotify once on its own ctx before
// deriving genCtx from it, so a grandchild session's activity notify walks
// all the way back up through every ancestor session's lock, not just its
// immediate parent's. This is how a parent session's heartbeat stays alive
// purely from a delegated sub-agent's progress while the parent itself is
// blocked inside the "agent"/agentic_fetch tool call, producing no stream
// callbacks of its own.
func WithActivityNotify(ctx context.Context, lk *sessionlock.SessionLock) context.Context {
	ancestor, _ := ctx.Value(activityNotifyContextKey{}).(func())
	self := func() {
		lk.RecordActivity() // nil-receiver-safe: no-op when lk is nil (a.dataDir == "")
		if ancestor != nil {
			ancestor()
		}
	}
	return context.WithValue(ctx, activityNotifyContextKey{}, self)
}

// NotifyActivity invokes the composed activity-notify callback stored on ctx
// by WithActivityNotify, if any. Safe to call on a ctx that never had
// WithActivityNotify applied (e.g. in tests, or any ctx not descended from a
// runTurn call) — it is then a harmless no-op.
func NotifyActivity(ctx context.Context) {
	if notify, ok := ctx.Value(activityNotifyContextKey{}).(func()); ok && notify != nil {
		notify()
	}
}

// WithWatchdogBump stores the stream watchdog's bump function in ctx.
// Used by runSummarizeBody and runSummarizeSilent to report LLM streaming
// progress during compaction (task #310).
func WithWatchdogBump(ctx context.Context, bump func()) context.Context {
	return context.WithValue(ctx, watchdogBumpContextKey{}, bump)
}

// NotifyWatchdog invokes the watchdog bump function stored on ctx by
// WithWatchdogBump, if any. Safe to call on a ctx that never had
// WithWatchdogBump applied — it is then a harmless no-op.
func NotifyWatchdog(ctx context.Context) {
	if bump, ok := ctx.Value(watchdogBumpContextKey{}).(func()); ok && bump != nil {
		bump()
	}
}
