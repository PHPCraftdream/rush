package cmd

// Regression coverage for the STATUS-column classification of a session that
// is AT REST but still owns an unreleased sub-agent delegation.
//
// Before this step existed, such a session was reported blank ("-", at rest)
// for the entire duration of the delegation: computeSessionStatuses only
// ever sees a lock file, and the parent's lock is released the moment the
// parent's turn yields — which for an async (`agent` / `agentic_fetch`)
// delegation is before the delegation is anywhere near done.
// markDelegatingSessions could not cover it either, because it only probes
// sessions already flagged "running" and requires a descendant message newer
// than the session's own updated_at, which is false for a parent whose only
// output was the tool call.
//
// The consequence an operator sees is a session sitting at "done"/"-" while a
// sub-agent is still running its own async work — the same misleading
// "finished" signal as the parent-facing notice, one layer up.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parkedDelegationReporter wraps the app's real coordinator with a canned
// answer for the optional ParkedSubAgentWorkReporter interface, so the
// classifier can be driven deterministically without standing up a live
// delegation. Embedding agent.Coordinator satisfies the whole interface; only
// the optional method is overridden.
type parkedDelegationReporter struct {
	agent.Coordinator
	parents []string
}

func (r parkedDelegationReporter) ParkedSubAgentParents() []string { return r.parents }

// parkedStatusEnv is the minimal shape markParkedDelegationSessions needs,
// plus the app it reads the optional reporter off.
func parkedStatusEnv(t *testing.T) (*app.App, session.Session) {
	t.Helper()
	a, _, _ := isolatedListEnvWithConfiguredDataDir(t)
	created, err := a.Sessions.Create(context.Background(), "parked-delegation-status")
	require.NoError(t, err)
	return a, created
}

// TestMarkParkedDelegationSessions_AtRestParentIsDelegating is the
// regression: an at-rest (blank-status) session that still owns a parked
// delegation must be reported "delegating", never "-" and never "done".
func TestMarkParkedDelegationSessions_AtRestParentIsDelegating(t *testing.T) {
	a, sess := parkedStatusEnv(t)

	// Precondition from computeSessionStatuses: no lock file, so the
	// session's raw classification is blank (at rest).
	statusByID := computeSessionStatuses(a)
	require.Empty(t, statusByID[sess.ID],
		"precondition: a session with no lock file must classify as at rest")

	a.AgentCoordinator = parkedDelegationReporter{Coordinator: a.AgentCoordinator, parents: []string{sess.ID}}
	got := markParkedDelegationSessions([]session.Session{sess}, statusByID, []string{sess.ID})

	assert.Equal(t, "delegating", got[sess.ID],
		"an at-rest session with a parked delegation is mid-workflow, not done")
}

// TestMarkParkedDelegationSessions_NoParkedWorkLeavesStatusAlone is the
// no-regression companion: a session with nothing parked must keep exactly
// the status computeSessionStatuses gave it, and a coordinator that does not
// implement the optional reporter must be a no-op rather than an error.
func TestMarkParkedDelegationSessions_NoParkedWorkLeavesStatusAlone(t *testing.T) {
	a, sess := parkedStatusEnv(t)

	statusByID := computeSessionStatuses(a)
	before := statusByID[sess.ID]
	got := markParkedDelegationSessions([]session.Session{sess}, statusByID, nil)
	assert.Equal(t, before, got[sess.ID], "no parked work must not change the status")

	// A coordinator that does not implement ParkedSubAgentWorkReporter is
	// simply not consulted (the call site type-asserts). The real one DOES
	// implement it — that is the point of the optional interface.
}

// TestMarkParkedDelegationSessions_NilStatusMapIsPromoted pins the nil-map
// path: computeSessionStatuses returns nil when the locks directory cannot
// be read at all (no session has ever held a lock), so every session reads
// as at rest — exactly the shape this promotion exists to correct. A naive
// nil guard would silently skip the promotion on every such machine.
func TestMarkParkedDelegationSessions_NilStatusMapIsPromoted(t *testing.T) {
	_, sess := parkedStatusEnv(t)

	var nilStatus map[string]string
	require.Nil(t, nilStatus)
	got := markParkedDelegationSessions([]session.Session{sess}, nilStatus, []string{sess.ID})
	assert.Equal(t, "delegating", got[sess.ID],
		"a nil status map must not silently disable the promotion")
}

// TestMarkParkedDelegationSessions_NeverDowngradesRunningOrCrashed pins that
// the promotion is additive only for the two signals that are genuinely
// independent of it: a session holding a live lock, and a session whose
// holder died without a clean finish. "delegating" is a refinement of
// "running" and must never mask either.
//
// "done" is deliberately NOT protected. A session promoted to "done" by
// reclassifyCrashedAsDone is exactly the shape this step corrects: its last
// end_turn turn can be nothing more than the parent's own yield before the
// delegation, which is why it reads as finished at all.
func TestMarkParkedDelegationSessions_NeverDowngradesRunningOrCrashed(t *testing.T) {
	_, sess := parkedStatusEnv(t)

	for _, stronger := range []string{"running", "crashed"} {
		statusByID := map[string]string{sess.ID: stronger}
		got := markParkedDelegationSessions([]session.Session{sess}, statusByID, []string{sess.ID})
		assert.Equal(t, stronger, got[sess.ID],
			"a %q session must keep its status when a delegation is parked", stronger)
	}

	statusByID := map[string]string{sess.ID: "done"}
	got := markParkedDelegationSessions([]session.Session{sess}, statusByID, []string{sess.ID})
	assert.Equal(t, "delegating", got[sess.ID],
		"a misclassified done must be corrected while the delegation is parked")
}
