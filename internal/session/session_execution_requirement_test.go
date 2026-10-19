package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedFence stores a fully populated fence on sessID and returns it.
func seedFence(t *testing.T, svc *service, sessID string) SessionExecutionRequirement {
	t.Helper()
	req := SessionExecutionRequirement{
		BindingID: "binding-1",
		Owner:     "owner-1",
		Epoch:     7,
		FolderScopeSpec: &FolderScopeSpec{
			WorkingDir: "/tmp/w",
			Entries:    []FolderScopeEntry{{Dir: ".", Ops: []FileOp{"read"}}},
		},
		RunAllowlistSpec: &RunAllowlistSpec{Restrict: true, AllowTools: []string{"read"}},
		CallPolicy:       &CallOptionsSpec{Version: CallOptionsSpecVersion, DisableSubAgents: true},
	}
	require.NoError(t, svc.StoreSessionExecutionRequirement(t.Context(), sessID, req))
	return req
}

// TestSessionExecutionRequirementRoundTrip catches a lost or degraded
// field in the store/load envelope round-trip, and a fence row written
// with any status other than 'acked', terminal_failure other than 1, or
// lease columns set — any of which would make the metadata row visible to
// the pump or retryable.
func TestSessionExecutionRequirementRoundTrip(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-roundtrip")
	require.NoError(t, err)
	want := seedFence(t, svc, sess.ID)

	got, err := svc.LoadSessionExecutionRequirement(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)

	entry, err := svc.GetRunQueueEntry(ctx, SessionExecutionRequirementRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, "acked", entry.Status)
	assert.True(t, entry.TerminalFailure)
	assert.Empty(t, entry.LeasedBy)
	assert.Zero(t, entry.LeasedAt)
	assert.Zero(t, entry.LeaseExpiresAt)
}

// TestSessionExecutionRequirementAbsent catches a loader that reports an
// error (or invents a zero-value requirement) for a session that never
// had a fence — the absent state must read as (nil, nil).
func TestSessionExecutionRequirementAbsent(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-absent")
	require.NoError(t, err)

	got, err := svc.LoadSessionExecutionRequirement(ctx, sess.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestSessionExecutionRequirementMalformed catches a loader that fails
// open: a wrong kind, a wrong version, an empty binding id, or truncated
// JSON in the stored payload must all surface
// ErrSessionExecutionRequirementMalformed rather than decode to something
// executable.
func TestSessionExecutionRequirementMalformed(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-malformed")
	require.NoError(t, err)
	require.NoError(t, svc.StoreSessionExecutionRequirement(ctx, sess.ID, SessionExecutionRequirement{BindingID: "b"}))
	rowID := SessionExecutionRequirementRowID(sess.ID)

	setCallData := func(payload string) {
		t.Helper()
		_, err := svc.db.ExecContext(ctx,
			`UPDATE session_run_queue SET call_data = ? WHERE id = ?`, payload, rowID)
		require.NoError(t, err)
	}

	cases := map[string]string{
		"wrong kind":     `{"kind":"something_else","version":1,"requirement":{"binding_id":"b"}}`,
		"wrong version":  `{"kind":"session_execution_requirement","version":99,"requirement":{"binding_id":"b"}}`,
		"empty binding":  `{"kind":"session_execution_requirement","version":1,"requirement":{"binding_id":""}}`,
		"truncated JSON": `{"kind":"session_execution_requirement","versi`,
	}
	for name, payload := range cases {
		setCallData(payload)
		got, err := svc.LoadSessionExecutionRequirement(ctx, sess.ID)
		require.ErrorIs(t, err, ErrSessionExecutionRequirementMalformed, name)
		assert.Nil(t, got, name)
	}
}

// TestSessionExecutionRequirementUpsert catches a store that appends a
// second fence row or fails to overwrite — loading after a re-store must
// return exactly the latest envelope.
func TestSessionExecutionRequirementUpsert(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-upsert")
	require.NoError(t, err)
	seedFence(t, svc, sess.ID)

	next := SessionExecutionRequirement{BindingID: "binding-2", Owner: "owner-2", Epoch: 9}
	require.NoError(t, svc.StoreSessionExecutionRequirement(ctx, sess.ID, next))

	got, err := svc.LoadSessionExecutionRequirement(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, next, *got)

	entry, err := svc.GetRunQueueEntry(ctx, SessionExecutionRequirementRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, entry, "upsert must keep exactly one fence row")
}

// TestPurgePreservesExecutionRequirementFence catches a purge that
// deletes the fence row (breaking H4's continuation of a surviving
// session) or, conversely, leaves a pending call row alive — the purge
// must still clear real queued work around the reserved row.
func TestPurgePreservesExecutionRequirementFence(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-purge")
	require.NoError(t, err)
	seedFence(t, svc, sess.ID)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "fence-purge-call", sess.ID, []byte(`{}`)))

	purged, err := svc.PurgeQueuedWorkForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged.RunQueue)

	callEntry, err := svc.GetRunQueueEntry(ctx, "fence-purge-call")
	require.NoError(t, err)
	assert.Nil(t, callEntry, "pending call row must be purged")

	fenceEntry, err := svc.GetRunQueueEntry(ctx, SessionExecutionRequirementRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, fenceEntry, "fence row must survive the purge")
	assert.Equal(t, "acked", fenceEntry.Status)

	got, err := svc.LoadSessionExecutionRequirement(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "binding-1", got.BindingID)
}

// TestLeaseNeverTakesExecutionRequirementFence catches a pending
// selection that ignores the status filter and hands the 'acked'
// metadata row to the pump as a runnable call.
func TestLeaseNeverTakesExecutionRequirementFence(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-lease")
	require.NoError(t, err)
	seedFence(t, svc, sess.ID)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "fence-lease-call", sess.ID, []byte(`{}`)))

	first, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "tester", time.Second)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "fence-lease-call", first.ID)

	second, err := svc.LeaseRunQueueEntry(ctx, sess.ID, "tester", time.Second)
	require.NoError(t, err)
	assert.Nil(t, second, "the fence row must never be leased")
}

// TestHasOutstandingIgnoresExecutionRequirementFence catches an
// outstanding-work check that counts the 'acked' fence row, which would
// make every fenced session look permanently busy to DrainSessionNow's
// terminal check.
func TestHasOutstandingIgnoresExecutionRequirementFence(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-outstanding")
	require.NoError(t, err)
	seedFence(t, svc, sess.ID)

	has, err := svc.HasOutstandingRunQueueEntriesForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.False(t, has)
}

// TestIsSessionExecutionRequirementCallData catches a pump guard that
// either misses a real fence envelope (executing it as a call) or
// false-positives on serialized SessionAgentCallData / garbage, which
// would wrongly terminal-fail genuine queued work.
func TestIsSessionExecutionRequirementCallData(t *testing.T) {
	sqlDB, q := newTestDB(t)
	svc := NewService(q, sqlDB).(*service)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "fence-probe")
	require.NoError(t, err)
	seedFence(t, svc, sess.ID)

	row, err := svc.GetRunQueueEntry(ctx, SessionExecutionRequirementRowID(sess.ID))
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.True(t, IsSessionExecutionRequirementCallData(row.CallData))

	callJSON, err := json.Marshal(SessionAgentCallData{Prompt: "hello"})
	require.NoError(t, err)
	assert.False(t, IsSessionExecutionRequirementCallData(string(callJSON)))
	assert.False(t, IsSessionExecutionRequirementCallData(`{not json`))
}
