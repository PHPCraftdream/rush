package session

// Real-SQLite, real-lock-file coverage for LiveJobs/LiveDescendantJobs (doc
// sec.5 step 7, sec.3.8's "Читатели между процессами"): a row is LIVE iff
// state='running' AND its host is alive per the SHARED probe; the walk
// follows child_session_id, never parent_session_id; a dead host's running
// row is dropped; an unknown host's row stays visible; this process's own
// host rows are alive without ever probing.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimRunning is a small helper: claims a running async_jobs row for
// owner/toolCallID on hostID directly via db.Queries, bypassing
// AsyncJobStore.Claim's own host registration -- so a test can attribute a
// row to an arbitrary FOREIGN host id (never registered by this process)
// instead of always landing on the store's own lazily-registered host.
func claimRunning(ctx context.Context, q *db.Queries, owner, toolCallID, kind, hostID, childSessionID string) error {
	params := db.ClaimAsyncJobParams{
		OwnerSessionID: owner,
		ToolCallID:     toolCallID,
		Kind:           kind,
		InputHash:      "h",
		HostID:         hostID,
		CreatedAt:      1700000000,
		UpdatedAt:      1700000000,
	}
	if childSessionID != "" {
		params.ChildSessionID.String = childSessionID
		params.ChildSessionID.Valid = true
	}
	_, err := q.ClaimAsyncJob(ctx, params)
	return err
}

func TestAsyncJobStore_HostLiveness_OwnHostAliveWithoutProbing(t *testing.T) {
	store, q, ctx := newTestStore(t)

	// A SEPARATE host identity in the same process/dataDir, with its OS lock
	// released directly (bypassing Close/SimulateCrashForTest, both of which
	// would also un-mark it) -- so the ONLY thing that can report it alive
	// is the own-host short-circuit, never a genuinely held lock.
	h, err := RegisterHost(ctx, store.dataDir, 1, "other", q)
	require.NoError(t, err)
	require.NoError(t, h.lock.Release())
	t.Cleanup(func() { unmarkOwnHostID(h.ID) })

	// Sanity: a real external probe of this exact lock file would say dead.
	status, err := ProbeHostLockShared(HostLockPath(store.dataDir, h.ID))
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, status, "sanity: the OS lock is genuinely free")

	// But HostLiveness must never reach the probe for this process's own id.
	assert.Equal(t, HostStatusAlive, store.HostLiveness(h.ID))
}

func TestLiveJobs_OwnRunningRowIsLive(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	require.Len(t, live, 1)
	assert.Equal(t, "owner-1", live[0].SessionID)
	assert.Equal(t, 0, live[0].Depth)
	assert.Equal(t, "call-1", live[0].ToolCallID)
	assert.Equal(t, HostStatusAlive, live[0].HostStatus)
}

func TestLiveJobs_ForeignAliveHostIsLive(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	holder, err := TryAcquireFileLock(HostLockPath(store.dataDir, "foreign-alive"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	require.NoError(t, claimRunning(ctx, q, "owner-1", "call-1", "command", "foreign-alive", ""))

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	require.Len(t, live, 1)
	assert.Equal(t, HostStatusAlive, live[0].HostStatus)
}

func TestLiveJobs_DeadHostRowIsNotLive(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	// Lock file exists on disk but nobody holds it -- a confirmed-dead host.
	seed, err := TryAcquireFileLock(HostLockPath(store.dataDir, "foreign-dead"))
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	require.NoError(t, claimRunning(ctx, q, "owner-1", "call-1", "command", "foreign-dead", ""))

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	assert.Empty(t, live, "a running row on a confirmed-dead host must never be reported live")
}

// TestLiveJobs_DeadHostProbeDoesNotBlockLaterRecovery is an A-b test fix:
// TestLiveJobs_DeadHostRowIsNotLive alone cannot tell whether LiveJobs used
// the non-acquiring SHARED probe (HostLiveness/ProbeHostShared, correct) or
// the ACQUIRING exclusive one (ProbeHost, wrong for a read-only reader) --
// both report the same "not live" verdict for a dead host. The two diverge
// on what they leave behind: an exclusive probe that won the lock and forgot
// to release it would make a LATER, REAL recoverer see this same host as
// still alive. This proves LiveJobs leaves nothing behind: a genuine
// recoverer can still win and interrupt the row right after LiveJobs probed
// it.
func TestLiveJobs_DeadHostProbeDoesNotBlockLaterRecovery(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	seed, err := TryAcquireFileLock(HostLockPath(store.dataDir, "foreign-dead-2"))
	require.NoError(t, err)
	require.NoError(t, seed.Release())
	require.NoError(t, claimRunning(ctx, q, "owner-1", "call-1", "command", "foreign-dead-2", ""))
	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{UpdatedAt: 1, OwnerSessionID: "owner-1", ToolCallID: "call-1"})
	require.NoError(t, err)

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	require.Empty(t, live)

	outcome, err := store.RecoverDeadHost(ctx, "foreign-dead-2", nil)
	require.NoError(t, err)
	require.Equal(t, 1, outcome.Interrupted, "a real recoverer must still win this host's lock right after LiveJobs probed it")
}

// TestLiveJobs_DepthCapSetsWalkIncomplete is A13's fix: the walk exiting on
// maxDescendantWalkDepth (16), not on an exhausted frontier, must report
// walkIncomplete=true -- a truncated result must never be read as "nothing
// deeper is running". Builds a delegation chain 17 levels deep (depths
// 0..16 are fully explored; the row at depth 16 still names a live child one
// level further out, which the walk never reaches).
func TestLiveJobs_DepthCapSetsWalkIncomplete(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)

	const chainLen = maxDescendantWalkDepth + 2 // sessions s0..s(chainLen-1)
	sessions := make([]string, chainLen)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("depth-chain-s%d", i)
		require.NoError(t, seedSession(ctx, q, sessions[i]))
	}
	// One initial real Claim so the store has a registered (alive) host id.
	_, err := store.Claim(ctx, ClaimParams{Owner: sessions[0], ToolCallID: "seed", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	hostID := store.HostID()
	require.NotEmpty(t, hostID)

	// Delegation rows: sessions[i] -> sessions[i+1], for i = 0..maxDescendantWalkDepth
	// (maxDescendantWalkDepth+1 rows, depths 0..maxDescendantWalkDepth).
	for i := 0; i <= maxDescendantWalkDepth; i++ {
		require.NoError(t, claimRunning(ctx, q, sessions[i], fmt.Sprintf("call-%d", i), "agent", hostID, sessions[i+1]))
	}

	live, incomplete := store.LiveJobs(ctx, sessions[0])
	require.True(t, incomplete, "a walk truncated by the depth cap, not an exhausted frontier, must report walkIncomplete")
	// Every row 0..maxDescendantWalkDepth was still explored and reported.
	require.Len(t, live, maxDescendantWalkDepth+2) // the seed row (depth 0, no child) + the chain rows
}

// TestJobsInTree_DepthCapSetsWalkIncomplete is JobsInTree's twin of the
// LiveJobs test above.
func TestJobsInTree_DepthCapSetsWalkIncomplete(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)

	const chainLen = maxDescendantWalkDepth + 2
	sessions := make([]string, chainLen)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("depth-chain-jt-s%d", i)
		require.NoError(t, seedSession(ctx, q, sessions[i]))
	}
	_, err := store.Claim(ctx, ClaimParams{Owner: sessions[0], ToolCallID: "seed", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	hostID := store.HostID()
	require.NotEmpty(t, hostID)

	for i := 0; i <= maxDescendantWalkDepth; i++ {
		require.NoError(t, claimRunning(ctx, q, sessions[i], fmt.Sprintf("call-%d", i), "agent", hostID, sessions[i+1]))
	}

	_, incomplete := store.JobsInTree(ctx, sessions[0])
	require.True(t, incomplete, "JobsInTree must also report walkIncomplete when truncated by the depth cap")
}

func TestLiveJobs_UnknownHostRowStaysVisible(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	// A directory at the lock path makes the probe fail with a non-ENOENT,
	// non-contention error on every supported platform -- "unknown", never
	// "dead" (doc sec.3.6/DUR-5).
	lockPath := HostLockPath(store.dataDir, "foreign-unknown")
	require.NoError(t, os.MkdirAll(lockPath, 0o755))

	require.NoError(t, claimRunning(ctx, q, "owner-1", "call-1", "command", "foreign-unknown", ""))

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	require.Len(t, live, 1, "an unknown-liveness host's row must stay visible, never silently dropped")
	assert.Equal(t, HostStatusUnknown, live[0].HostStatus)
}

func TestLiveJobs_TerminalRowIsNeverLive(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", NoticeKind: "completed", Wake: true})
	require.NoError(t, err)

	live, incomplete := store.LiveJobs(ctx, "owner-1")
	require.False(t, incomplete)
	assert.Empty(t, live, "a terminal row must never be reported live regardless of host liveness")
}

// TestLiveJobs_DelegationTreeTransitivity is the doc sec.5 step 7 scenario:
// a parent shows work via a CHILD's live rows through delegation rows
// (child_session_id), not parent_session_id, and the result -- via
// LiveDescendantJobs -- never names the root's own id.
func TestLiveJobs_DelegationTreeTransitivity(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "root"))
	require.NoError(t, seedSession(ctx, q, "child"))
	require.NoError(t, seedSession(ctx, q, "grandchild"))

	_, err := store.Claim(ctx, ClaimParams{
		Owner: "root", ToolCallID: "delegate-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child",
	})
	require.NoError(t, err)
	_, err = store.Claim(ctx, ClaimParams{
		Owner: "child", ToolCallID: "delegate-2", Kind: JobKindAgent, Input: "do more work", ChildSessionID: "grandchild",
	})
	require.NoError(t, err)

	live, incomplete := store.LiveJobs(ctx, "root")
	require.False(t, incomplete)
	require.Len(t, live, 2)
	assert.Equal(t, "root", live[0].SessionID)
	assert.Equal(t, 0, live[0].Depth)
	assert.Equal(t, "child", live[0].ChildSessionID)
	assert.Equal(t, "child", live[1].SessionID)
	assert.Equal(t, 1, live[1].Depth)
	assert.Equal(t, "grandchild", live[1].ChildSessionID)

	// LiveDescendantJobs reports every DELEGATION link, at any depth: the
	// root's own row already names "child" as a live descendant (that row
	// IS the evidence, even though root itself owns it, Depth 0), and the
	// child's own row names "grandchild" transitively.
	descendants, incomplete2 := store.LiveDescendantJobs(ctx, "root")
	require.False(t, incomplete2)
	require.Len(t, descendants, 2)
	gotChildren := []string{descendants[0].ChildSessionID, descendants[1].ChildSessionID}
	assert.ElementsMatch(t, []string{"child", "grandchild"}, gotChildren)
	for _, d := range descendants {
		assert.NotEqual(t, "root", d.ChildSessionID, "LiveDescendantJobs must never name the root's own id")
	}
}

func TestLiveJobs_NilAndEmptyInputs(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestStore(t)

	live, incomplete := store.LiveJobs(ctx, "")
	assert.Nil(t, live)
	assert.False(t, incomplete)

	var nilStore *AsyncJobStore
	live, incomplete = nilStore.LiveJobs(ctx, "owner-1")
	assert.Nil(t, live)
	assert.False(t, incomplete)
}

func TestJobsInTree_IncludesTerminalAndFollowsAllDelegations(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "root"))
	require.NoError(t, seedSession(ctx, q, "child"))

	_, err := store.Claim(ctx, ClaimParams{
		Owner: "root", ToolCallID: "delegate-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child",
	})
	require.NoError(t, err)
	_, err = store.Transition(ctx, TransitionParams{Owner: "root", ToolCallID: "delegate-1", State: "completed", NoticeKind: "completed", Wake: true})
	require.NoError(t, err)

	_, err = store.Claim(ctx, ClaimParams{Owner: "child", ToolCallID: "call-2", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	jobs, incomplete := store.JobsInTree(ctx, "root")
	require.False(t, incomplete)
	require.Len(t, jobs, 2, "JobsInTree must include the TERMINAL delegation row and follow into its child")

	var sawTerminal, sawChildJob bool
	for _, j := range jobs {
		if j.OwnerSessionID == "root" && j.State == "completed" {
			sawTerminal = true
		}
		if j.OwnerSessionID == "child" && j.ToolCallID == "call-2" {
			sawChildJob = true
		}
	}
	assert.True(t, sawTerminal, "the finished delegation row itself must be included")
	assert.True(t, sawChildJob, "JobsInTree must follow into the child's own jobs even though the delegation is terminal")
}
