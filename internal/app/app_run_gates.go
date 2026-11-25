// `rush run` gating applied before the agent starts: the default
// sub-agent tool ban and its smart+worker bypass, plus the
// restricted-run allowlist spec derived from config permissions.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/app/appbatch"
	"github.com/PHPCraftdream/rush/internal/config"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// subAgentToolNames lists every tool name that the default `rush run`
// sub-agent ban strips from the coder agent's AllowedTools. Split out so
// callers that want to restore a subset (see the smart+worker bypass in
// RunNonInteractive) can strip everything EXCEPT that subset instead of
// duplicating the loop in disableSubAgentToolsInConfig.
var subAgentToolNames = []string{"agent", "agentic_fetch"}

// disableSubAgentToolsInConfig drops the given tool names from the coder
// agent's AllowedTools list in the in-memory config. Used by
// RunNonInteractive when overrides.DisableSubAgents (`rush run --agents
// single`, or the implicit default when --agents is unset) is set.
// Mutation does not touch the on-disk config and only outlives this
// process if a future caller reloads the in-memory config from disk —
// `rush run` exits immediately after the agent turn so this is moot in
// practice.
//
// Fork patch (orchestrator UX): see CHANGELOG.fork.md (Section 4.J).
func disableSubAgentToolsInConfig(env RunEnv) {
	disableToolsInConfig(env, subAgentToolNames)
}

// disableToolsInConfig drops exactly the named tools from the coder
// agent's AllowedTools list in the in-memory config. Extracted so the
// smart+worker bypass (see shouldBypassSubAgentBan) can restore just the
// `agent` tool while keeping `agentic_fetch` stripped, without
// duplicating the filter loop.
//
// Reads the currently-published config via Config() (read-only, per its
// documented contract) to compute the filtered tool list, then applies the
// change through ConfigStore.UpdateAgentAllowedTools, which publishes it as
// a new generation via the copy-on-write path instead of writing directly
// into cfg.Agents[...] on the already-published *Config — see
// UpdateAgentAllowedTools's doc comment for why that in-place write was a
// contract violation (a concurrent reader holding the old *Config would see
// the mutation retroactively).
//
// Fork patch (orchestrator UX, plan phase 2): bypass restores `agent` only.
func disableToolsInConfig(env RunEnv, toolNames []string) {
	cfg := env.Config.Config()
	if cfg == nil {
		return
	}
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return
	}
	filtered := coder.AllowedTools[:0:0]
	for _, t := range coder.AllowedTools {
		if slices.Contains(toolNames, t) {
			continue
		}
		filtered = append(filtered, t)
	}
	env.Config.UpdateAgentAllowedTools(config.AgentCoder, filtered)
}

// shouldBypassSubAgentBan decides whether the `rush run` default
// sub-agent ban (DisableSubAgents) should be bypassed for the `agent`
// tool specifically, restoring it to the coder's AllowedTools even
// though the ban is otherwise in effect.
//
// Fork patch (orchestrator UX, plan phase 2): the orchestrator design (a smart
// parent delegating hands-on work to cheap worker sub-agents) depends on
// the `agent` tool being available. The ban exists to stop an
// unsupervised `rush run` from silently fanning out — but when a Worker
// model is configured, that fan-out IS the point, so the ban would
// otherwise block the very feature it was configured for. This applies
// regardless of whether `--agents single` was passed explicitly or left
// unset: a configured worker means the operator's intent is delegation,
// and `--agents single` alone (with no worker unconfigured to back it)
// is not a strong enough signal to override that.
//
// Bypasses only when ALL of:
//   - role == SelectedModelTypeLarge: the run declared --role smart.
//     --role fast (or worker/reviewer) never bypasses; we don't
//     second-guess an explicit non-smart role choice.
//   - a Worker model slot is configured with a non-empty Model. No
//     worker means there is nothing productive for the `agent` tool to
//     delegate to, so the historical (safe) default — sub-agents off —
//     stands. This is the single most important case: worker NOT
//     configured + role smart + flags unset must keep sub-agents
//     disabled exactly as today.
func shouldBypassSubAgentBan(role config.SelectedModelType, cfg *config.Config) bool {
	if role != config.SelectedModelTypeSmart {
		return false
	}
	if cfg == nil {
		return false
	}
	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

// shouldRunReviewerPass decides whether a finished `rush run` invocation
// should be followed by the automatic reviewer pass: one extra agent turn
// on the same session, driven by the Reviewer model slot, whose own
// response becomes the run's final output (the functional equivalent of
// the operator manually running `rush run --role reviewer --session
// <same-id>` right after the smart run finished).
//
// Fork patch (reviewer pass): fires only when ALL of:
//   - role == SelectedModelTypeSmart: the run declared --role smart.
//     An explicit --role fast/worker/reviewer invocation is never
//     auto-followed by a review — the operator already chose a specific
//     slot on purpose.
//   - a Reviewer model slot is configured with a non-empty Model. No
//     reviewer means the run must behave byte-identically to before the
//     feature existed; unconfiguring the reviewer is the off switch (the
//     same design as the worker bypass above — no flag of its own).
func shouldRunReviewerPass(role config.SelectedModelType, cfg *config.Config) bool {
	if role != config.SelectedModelTypeSmart {
		return false
	}
	if cfg == nil {
		return false
	}
	reviewerModelCfg, ok := cfg.Models[config.SelectedModelTypeReviewer]
	return ok && reviewerModelCfg.Model != ""
}

// reviewerPassScopeOpenRetries bounds how many times reviewerPassBlocked
// retries a failed CLIScope read before giving up and defaulting to
// "blocked" (skip the reviewer pass) -- C16 fix: doc sec.3.5 requires a DB
// read error to retry with a pause, never a silent skip on the very first
// error.
const reviewerPassScopeOpenRetries = 3

// reviewerPassBlocked reports whether the automatic reviewer pass must not
// open a new phase on sessionID: it has running work, or owes (Owed), is
// retrying (Paced) or gave up on (Stuck) a reaction turn. Debt the policy
// defers (DrainDeferred: nothing will ever act on it) does not block. Every
// skip is one line on stderr -- the run's own writer (RunRequest.Stderr), never
// the process's -- so a silently missing review is never a mystery.
// A DB read error retries reviewerPassScopeOpenRetries times (paced by
// cliDBRetryPause); persistent failure blocks -- running a possibly-conflicting
// reviewer phase while the scope is unknown is the riskier guess.
//
// Only called when every cheap, in-memory gate (ExecuteRun's own error,
// credentials, cancellation, shouldRunReviewerPass) already passed.
func reviewerPassBlocked(env RunEnv, ctx context.Context, source agent.ReactionDebtSource, sessionID string, stderr io.Writer) bool {
	var lastErr error
	for attempt := 1; attempt <= reviewerPassScopeOpenRetries; attempt++ {
		state, err := source.CLIScope(ctx, sessionID)
		if err == nil {
			why := ""
			switch {
			case state.WorkOpen:
				why = "it still has running work"
			case state.Drain == agent.DrainOwed:
				why = "it owes a reaction turn"
			case state.Drain == agent.DrainPaced:
				why = "a failed reaction turn is being retried"
			case state.Drain == agent.DrainStuck:
				why = "a notice it stopped reacting to is still pending"
			}
			if why != "" {
				fmt.Fprintf(stderr, "rush run: reviewer pass skipped for session %q: %s\n", sessionID, why)
				return true
			}
			return false
		}
		lastErr = err
		if errors.Is(err, agent.ErrBatchFactsSourceUnavailable) {
			break
		}
		slog.Warn("reviewer pass: scope check failed; retrying", "session_id", sessionID, "attempt", attempt, "err", err)
		if attempt == reviewerPassScopeOpenRetries || !sleepOrCtxDone(ctx, cliDBRetryPause) {
			break
		}
	}
	fmt.Fprintf(stderr, "rush run: reviewer pass skipped for session %q: its scope could not be read (%v)\n", sessionID, lastErr)
	return true
}

// runAllowlistSpecFromConfig reads the config-derived restricted-run
// allowlist spec (pre-compilation). Returns an inert spec (Restrict =
// false) when permissions.run is absent or disabled, preserving the
// legacy `rush run` auto-approve-everything behaviour.
func runAllowlistSpecFromConfig(p *config.Permissions) permission.RunAllowlistSpec {
	spec := permission.RunAllowlistSpec{}
	if p == nil || p.Run == nil {
		return spec
	}
	spec.Restrict = p.Run.Restrict
	// Defensive copies so a later config reload can't mutate the spec
	// we hand to the compiler.
	spec.AllowTools = append(spec.AllowTools, p.Run.AllowTools...)
	spec.AllowBash = append(spec.AllowBash, p.Run.AllowBash...)
	return spec
}

// fsToolsForScope returns the fs_* tool names the compiled scope grants,
// for appending to the restricted-run AllowTools table so a
// "scoped + restricted" run's fs_* calls clear the run gate. The
// presence rules mirror the coordinator's applyCallFolderScope exactly
// (one operation per tool; fs_write appears when create OR overwrite is
// granted, because its per-item check picks the operation by path
// existence), so the permission gate and the actual scoped toolset can
// never disagree. A nil scope grants nothing.
func fsToolsForScope(scope *permission.FolderScope) []string {
	if scope == nil {
		return nil
	}
	return tools.FolderScopeToolNames(*scope)
}

// retryTransientRead paces a transient store-read failure of the CLI loop: nil
// after a pause means retry; the error ends the run once the failures outlast
// cliDBErrorRetryOverallLimit (or the run's context is done).
func (l *cliLoop) retryTransientRead(start *time.Time, what string, err error) error {
	if start.IsZero() {
		*start = time.Now()
	}
	if time.Since(*start) > cliDBErrorRetryOverallLimit {
		fmt.Fprintf(l.errOut(), "rush run: session %q's %s been unreadable for %s (%s); giving up\n",
			l.sessionID, what, cliDBErrorRetryOverallLimit, err)
		return err
	}
	if !sleepOrCtxDone(l.ctx, cliDBRetryPause) {
		return l.ctx.Err()
	}
	return nil
}

// usageBySession reads the session's per-message usage for the JSON
// envelope on its own cleanup budget: the finish phase's earlier reads share
// finalCtx and can spend it on a busy store, which left the envelope
// without usage.session.
func (s *executeRunLoop) usageBySession() (message.UsageReport, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), cleanupTimeout)
	defer cancel()
	return s.env.Messages().UsageBySession(ctx, s.sess.ID)
}

// processHome is ProcessHome's free helper. It takes the *App itself
// (not a RunEnv) because it must also answer for a nil App.
func processHome(app *App) bool {
	if app == nil {
		return true
	}
	return app.home
}

// refuseForeign reports the WS-1 refusal for a session this process does not
// own (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1 table).
//
// resolveSession consults this after every successful Get -- before any write,
// and well before the OS-level session lock, so a foreign `--session <id>`
// produces this refusal rather than an "already in use" raised by another
// checkout's running agent.
//
// The wording (owning root, branch, and the two ways forward) and the typed
// runq.ErrForeignWorkspace wrapper live in agent.ForeignWorkspaceError:
// runOwned's guard refuses with the same sentence for every non-CLI entry
// point, and one builder keeps those two unable to drift.
func refuseForeign(sess core.Session) error {
	return agent.ForeignWorkspaceError(sess)
}

// owns reports whether this process may drive sess (WS-1, #1142 step C).
// Legacy unbound (”) rows are drivable exactly when the App is a home
// process -- the explicit home flag from config.WorkspaceHome(), never a
// derivation from the workspace root (which is non-empty for every git
// checkout and would otherwise refuse the whole pre-WS-1 history).
func owns(sess core.Session, workspaceRoot string, home bool) bool {
	return core.Owns(sess.WorkspaceRoot, workspaceRoot, home)
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func resolveSession(env RunEnv, ctx context.Context, continueSessionID string, useLast bool) (core.Session, error) {
	origin := sessionprompt.CallOriginFrom(ctx)
	switch {
	case continueSessionID != "":
		if env.Sessions().IsAgentToolSession(continueSessionID) {
			return core.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := env.Sessions().Get(ctx, continueSessionID)
		if err == nil {
			return appbatch.ValidateResolvedSession(sess, continueSessionID, env.WorkspaceRoot, env.ProcessHome())
		}
		// Get-or-create semantics: --session <id> with an unknown id creates
		// a brand-new top-level session with that exact id. Lets CI / scripts
		// pick a deterministic key (e.g. an issue number) and re-run idempotently.
		var created core.Session
		var createErr error
		if oc, ok := env.Sessions().(core.OriginCreator); ok {
			created, createErr = oc.CreateWithIDAndOrigin(ctx, continueSessionID, continueSessionID, origin)
		} else {
			// Test fakes implementing core.Service without the
			// OriginCreator seam keep the legacy behaviour.
			created, createErr = env.Sessions().CreateWithID(ctx, continueSessionID, continueSessionID)
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
			if sess, getErr := env.Sessions().Get(ctx, continueSessionID); getErr == nil {
				// The winner of the create race may have bound the row to its
				// own workspace: same ownership check as the first Get.
				sess, err := appbatch.ValidateResolvedSession(sess, continueSessionID, env.WorkspaceRoot, env.ProcessHome())
				if err == nil {
					slog.Info("Session creation raced another process; attached to the winner's row",
						"session_id", continueSessionID)
				}
				return sess, err
			}
			// The re-Get failed too (extremely unlikely — the row that
			// caused our constraint violation vanished again, e.g. a
			// concurrent `sessions kill`/delete). Fall through to the
			// original wrapped error below rather than hiding the
			// surprise.
		}
		return core.Session{}, fmt.Errorf("session %q not found and could not be created: %w", continueSessionID, createErr)

	case useLast:
		sess, err := env.Sessions().GetLast(ctx)
		if err != nil {
			return core.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		if oc, ok := env.Sessions().(core.OriginCreator); ok {
			return oc.CreateWithOrigin(ctx, agent.DefaultSessionName, origin)
		}
		return env.Sessions().Create(ctx, agent.DefaultSessionName)
	}
}

// resolveRunSession resolves the session of one ExecuteRun. A `rush run` loop's
// Drain iteration (drainTurn) continues the session its first turn already
// resolved and claimed: it is read back as is, without the "cannot continue a
// child session" refusal, which guards only what an operator may start.
func resolveRunSession(env RunEnv, ctx context.Context, drainTurn bool, continueSessionID string, useLast bool) (core.Session, error) {
	if drainTurn && continueSessionID != "" {
		return env.Sessions().Get(ctx, continueSessionID)
	}
	return resolveSession(env, ctx, continueSessionID, useLast)
}

const drainPendingMaxAttempts = 3

var drainPendingAfterAttemptSeam func(string)

// drainPendingBeforeRun executes outstanding durable rows before the new turn.
func drainPendingBeforeRun(env RunEnv, ctx context.Context, sessionID string) error {
	if env.Pump() == nil || env.Sessions() == nil {
		return nil
	}

	for attempt := 1; attempt <= drainPendingMaxAttempts; attempt++ {
		outstanding, err := env.Sessions().HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("run: failed to check pending durable work before the turn: %w", err)
		}
		if !outstanding {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("run: canceled before pending durable work drained: %w", err)
		}

		slog.Info("run: executing pending durable work for this session before the new turn", "session_id", sessionID)
		result, drainErr := env.Pump().DrainSessionNow(ctx, sessionID)
		if drainPendingAfterAttemptSeam != nil {
			drainPendingAfterAttemptSeam(sessionID)
		}

		localBusy := env.Coord() != nil && env.Coord().IsSessionBusy(sessionID)
		if drainErr != nil {
			contention := errors.Is(drainErr, core.ErrCallQueuedNotExecuted) || errors.Is(drainErr, runq.ErrDrainIncomplete)
			if !contention {
				return fmt.Errorf("run: pending durable work drain incomplete (%s): %w", result, drainErr)
			}
		}

		if localBusy {
			if err := waitLocalOwnerReleased(env, ctx, sessionID); err != nil {
				return fmt.Errorf("run: canceled while waiting for the current session owner: %w", err)
			}
		}

		outstanding, err = env.Sessions().HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("run: failed to recheck pending durable work after drain: %w", err)
		}
		if !outstanding {
			if drainErr != nil {
				return fmt.Errorf("run: pending durable work drain incomplete (%s): %w", result, drainErr)
			}
			slog.Info("run: pending durable work drained", "session_id", sessionID, "result", result)
			return nil
		}
		if attempt == drainPendingMaxAttempts {
			break
		}
		if !localBusy {
			timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("run: canceled while waiting for pending durable work: %w", ctx.Err())
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("run: pending durable work remains after %d drain attempts", drainPendingMaxAttempts)
}

// waitLocalOwnerReleased waits until this process no longer owns the session.
func waitLocalOwnerReleased(env RunEnv, ctx context.Context, sessionID string) error {
	if env.Coord() == nil || !env.Coord().IsSessionBusy(sessionID) {
		return nil
	}
	slog.Info("run: waiting for this process's current turn on the session to finish", "session_id", sessionID)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for env.Coord().IsSessionBusy(sessionID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
