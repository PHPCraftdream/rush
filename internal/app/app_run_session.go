package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
)

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	origin := agent.CallOriginFrom(ctx)
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err == nil {
			if sess.ParentSessionID != "" {
				return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
			}
			return sess, nil
		}
		// Get-or-create semantics: --session <id> with an unknown id creates
		// a brand-new top-level session with that exact id. Lets CI / scripts
		// pick a deterministic key (e.g. an issue number) and re-run idempotently.
		var created session.Session
		var createErr error
		if oc, ok := app.Sessions.(session.OriginCreator); ok {
			created, createErr = oc.CreateWithIDAndOrigin(ctx, continueSessionID, continueSessionID, origin)
		} else {
			// Test fakes implementing session.Service without the
			// OriginCreator seam keep the legacy behaviour.
			created, createErr = app.Sessions.CreateWithID(ctx, continueSessionID, continueSessionID)
		}
		if createErr == nil {
			slog.Info("Created session on demand from --session id", "session_id", created.ID)
			return created, nil
		}
		// Session-creation race (task #605): several `rush run --session
		// <id>` processes can all miss the Get above for an id that has
		// NEVER existed before (first use of that id) and then all race
		// CreateWithID's INSERT. SQLite's own single-writer serialization
		// still guarantees exactly one INSERT wins — the losers get back a
		// PRIMARY KEY/UNIQUE constraint violation on sessions.id, e.g.
		// "constraint failed: UNIQUE constraint failed: sessions.id
		// (1555)". Before this fix that raw driver error propagated
		// straight to the operator/orchestrator instead of the friendly
		// "session busy, use `sessions inject`" message the already-exists
		// race already gets (see sessionBusyGuidance in app_run_errors.go)
		// — an orchestrator parsing stderr would misclassify a transient,
		// retryable race as a permanent failure.
		//
		// Fix: re-Get once. By the time our own INSERT has been rejected
		// by the UNIQUE/PRIMARY KEY index, the winner's INSERT has
		// necessarily already committed (SQLite's single-writer model
		// serializes the two transactions; ours only sees the conflict
		// because theirs finished first) — so the row is guaranteed to be
		// there for a losing process to attach to. Re-Get is the shape
		// that reuses ALL of the existing, tested busy-rejection path:
		// once attached, this losing process proceeds exactly like the
		// already-exists case above, and the pre-existing OS-level
		// session lock (internal/agent/agent_run.go, checked once this
		// function returns and AgentCoordinator.Run acquires it) is what
		// actually produces the clean "session busy" guidance — no new
		// error-classification branch or wrapping needed here.
		//
		// This intentionally only swallows the constraint violation for
		// THIS table+column (sessions.id) via isSessionsIDConstraintError
		// below, not any database error: a genuinely broken DB (disk
		// full, corruption, permission denied) must still surface as-is
		// rather than being silently retried into a confusing "not
		// found" on the follow-up Get.
		if isSessionsIDConstraintError(createErr) {
			if sess, getErr := app.Sessions.Get(ctx, continueSessionID); getErr == nil {
				if sess.ParentSessionID != "" {
					return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
				}
				slog.Info("Session creation raced another process; attached to the winner's row",
					"session_id", continueSessionID)
				return sess, nil
			}
			// The re-Get failed too (extremely unlikely — the row that
			// caused our constraint violation vanished again, e.g. a
			// concurrent `sessions kill`/delete). Fall through to the
			// original wrapped error below rather than hiding the
			// surprise.
		}
		return session.Session{}, fmt.Errorf("session %q not found and could not be created: %w", continueSessionID, createErr)

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		if oc, ok := app.Sessions.(session.OriginCreator); ok {
			return oc.CreateWithOrigin(ctx, agent.DefaultSessionName, origin)
		}
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}
