// The WS-1 ownership refusal (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1, invariant WS-1): a session may
// only be driven by the process whose workspace it was created in, or by the
// home process when the row is a legacy unbound one.
//
// THREE callers must refuse, and all three must say the same thing:
//   - app.resolveSession, for an operator's `rush run --session <id>` (before
//     any write and before the OS session lock, so a foreign id never reads
//     as "already in use");
//   - agent.sessionAgent.runOwned, the backstop that catches every other
//     entry point into a turn (the run-queue pump, a drain, a wake, a web
//     inject) -- it runs once this process owns the mailbox AND the
//     inter-process session lock, and before the admission-time setup and the
//     turn preamble, so nothing has been written or streamed when it fires;
//   - the turn arbiter, which classifies the same refusal when it arrives as
//     a leg's error (see turn_arbiter.go's ForeignWorkspace fact).
//
// The text is built here, once, so those three cannot drift into three
// different stories about the same condition. It wraps
// session.ErrForeignWorkspace (the typed sentinel the run-queue pump reads
// to nack a foreign row without penalising it) and names the owning root, the
// branch that checkout was on, and the two ways forward.
//
// The sentinel lives in internal/session (the storage layer classifies the
// same condition without importing agent), and the sentence lives here in the
// agent package because all three refusal sites already reach for the agent
// package's vocabulary.
package agent

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/session"
)

// ForeignWorkspaceError is the typed refusal for a session this process does
// not own: sess belongs to a workspace other than this process's, so this
// process must not drive it. The returned error satisfies
// errors.Is(err, session.ErrForeignWorkspace) and its text is the contract's
// sentence from docs/plans/2026-10-01-shared-data-dir.md §1.
//
// It deliberately does NOT decide anything: the caller has already established
// that the row is foreign (app.owns / session.Owns in runOwned), and the
// message only explains what to do about it. Re-pointing the row is not
// offered on purpose -- `rush sessions fork` is the one sanctioned way to
// continue another checkout's history here, and it copies rather than moves
// the session, so two processes can never write one session's messages.
func ForeignWorkspaceError(sess session.Session) error {
	msg := fmt.Sprintf(
		"session %s belongs to %s (branch %s); run it there or \"rush sessions fork %s\" here",
		sess.ID, sess.WorkspaceRoot, sess.GitBranch, sess.ID,
	)
	if sess.WorkspaceRoot != "" {
		if _, err := os.Stat(sess.WorkspaceRoot); err != nil {
			// The owning checkout is gone (`git worktree remove`): say so, or
			// the operator goes hunting for a directory that no longer
			// exists. A legacy row's empty root is never stat-ed -- it names no
			// directory to be missing.
			msg += " (workspace no longer exists)"
		}
	}
	return fmt.Errorf("%s: %w", msg, session.ErrForeignWorkspace)
}
