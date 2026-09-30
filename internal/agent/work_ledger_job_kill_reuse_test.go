// R3A-2 (docs/reviews/2026-09-30-async-phase4-round3.md), through the real
// job_kill result path: a provider that numbers calls per response issues a new
// "call_0" while the previous "call_0" job's job_kill is still running. The new
// claim archives the just-killed row, so anything keyed by tool_call_id lands on
// the wrong row.
package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// reusedKillFixture stops J1 ("call_0") with job_kill, then starts J2 under the
// same id before job_kill's own result is persisted.
type reusedKillFixture struct {
	*jobKillResultFixture
	q        *db.Queries
	archived db.AsyncJob // J1 after the new claim archived it
	second   *asyncJob   // J2
}

func newReusedKillFixture(t *testing.T) *reusedKillFixture {
	t.Helper()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	q := db.New(conn)
	base := &jobKillResultFixture{messages: &failingCreateTxMessages{Service: message.NewService(q)}, rep: &fakeRepender{}}
	base.l = newWorkLedger(nil)
	base.l.store = store
	base.l.jobKillRepend = base.rep

	_, _, err := base.l.Start("owner-1", "call_0", "sleep 9", tools.BashToolName, "", false, false, nil, func() {})
	require.NoError(t, err)
	base.l.acknowledged(jobOf(base.l, "owner-1", "call_0"))
	text, claim, verdict := base.l.MarkJobStopped("owner-1", "call_0")
	require.Equal(t, tools.JobStopStopped, verdict)
	base.claim = claim
	meta, err := json.Marshal(tools.JobKillResponseMetadata{JobID: "call_0", ShellID: "s1", KilledClaimID: claim})
	require.NoError(t, err)
	base.result = message.ToolResult{ToolCallID: "kill-1", Name: tools.JobKillToolName, Content: text, Metadata: string(meta)}

	// The model's next response reuses "call_0" while job_kill is still running.
	second, _, err := base.l.Start("owner-1", "call_0", "sleep 10", tools.BashToolName, "", false, false, nil, func() {})
	require.NoError(t, err)
	require.NotEqual(t, claim, second.claimID)

	f := &reusedKillFixture{jobKillResultFixture: base, q: q, second: second}
	for _, r := range f.rows(t) {
		if r.ClaimID == claim {
			f.archived = r
		}
	}
	require.Contains(t, f.archived.ToolCallID, "#reused#", "the new claim must have archived the killed row")
	return f
}

func (f *reusedKillFixture) rows(t *testing.T) []db.AsyncJob {
	t.Helper()
	rows, err := f.q.ListAsyncJobsForOwner(context.Background(), "owner-1")
	require.NoError(t, err)
	return rows
}

func (f *reusedKillFixture) row(t *testing.T, claimID string) db.AsyncJob {
	t.Helper()
	for _, r := range f.rows(t) {
		if r.ClaimID == claimID {
			return r
		}
	}
	t.Fatalf("no row with claim %s", claimID)
	return db.AsyncJob{}
}

// TestPersistToolResult_JobKillWithReusedToolCallID_NamesTheKilledRow: the
// fused write names the ARCHIVED killed row, not the new job that took the id.
//
// Revert-check: key the fused write by tool_call_id (owner + the reused id)
// -> the archived row keeps a NULL notice_message_id.
func TestPersistToolResult_JobKillWithReusedToolCallID_NamesTheKilledRow(t *testing.T) {
	t.Parallel()
	f := newReusedKillFixture(t)
	require.False(t, f.archived.NoticeMessageID.Valid)

	require.NoError(t, f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params()))

	msgs, err := f.messages.List(context.Background(), "owner-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	killed := f.row(t, f.claim)
	require.True(t, killed.NoticeMessageID.Valid, "DUR-11: the killed row must name the job_kill result message")
	require.Equal(t, msgs[0].ID, killed.NoticeMessageID.String)
	fresh := f.row(t, f.second.claimID)
	require.False(t, fresh.NoticeMessageID.Valid, "the new job under the reused id must not be named")
	require.Empty(t, f.rep.snapshot(), "a successful fused write re-pends nothing")
}

// TestPersistToolResult_JobKillErrorResultWithReusedToolCallID_RependsTheKilledRow:
// the result is an error (no fused write): the REAL store's re-pend still
// finds the archived row by claim and puts it back to pending; the new job is
// untouched.
//
// Revert-check: add `AND tool_call_id = <reused id>` to the re-pend query.
func TestPersistToolResult_JobKillErrorResultWithReusedToolCallID_RependsTheKilledRow(t *testing.T) {
	t.Parallel()
	f := newReusedKillFixture(t)
	f.l.jobKillRepend = nil // the real store
	f.result.IsError = true

	require.NoError(t, f.l.persistToolResult(context.Background(), "owner-1", f.result, f.messages, f.params()))

	killed := f.row(t, f.claim)
	require.Equal(t, "pending", killed.Delivery, "the unnamed job_kill row must be deliverable by the ordinary pull")
	require.EqualValues(t, 0, killed.Wake)
	fresh := f.row(t, f.second.claimID)
	require.Equal(t, "running", fresh.State)
}
