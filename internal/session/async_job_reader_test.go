package session

// Real-SQLite, real-lock-file coverage for LiveJobs/LiveDescendantJobs (doc
// sec.5 step 7, sec.3.8's "Читатели между процессами"): a row is LIVE iff
// state='running' AND its host is alive per the SHARED probe; the walk
// follows child_session_id, never parent_session_id; a dead host's running
// row is dropped; an unknown host's row stays visible; this process's own
// host rows are alive without ever probing.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
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

// TestLiveOwnJobs pins the own-plain-job reader: only the session's OWN
// running non-delegation rows on a host not provably dead count. A delegation
// row (LiveDescendantJobs' business), a terminal row, a row on a dead host and
// another owner's row do not.
//
// Revert-check performed: made LiveOwnJobs return nil -- the "own running job"
// subtest FAILED (len 0).
func TestLiveOwnJobs(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	require.NoError(t, seedSession(ctx, q, "owner-2"))
	require.NoError(t, seedSession(ctx, q, "child-1"))

	t.Run("nothing", func(t *testing.T) {
		live, incomplete := store.LiveOwnJobs(ctx, "owner-1")
		require.False(t, incomplete)
		require.Empty(t, live)
	})

	t.Run("own running job on this host", func(t *testing.T) {
		_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "own-1", Kind: JobKindCommand, Input: "sleep"})
		require.NoError(t, err)
		live, incomplete := store.LiveOwnJobs(ctx, "owner-1")
		require.False(t, incomplete)
		require.Len(t, live, 1)
		assert.Equal(t, "own-1", live[0].ToolCallID)
		assert.Equal(t, HostStatusAlive, live[0].HostStatus)
		assert.Equal(t, 0, live[0].Depth)
	})

	t.Run("delegation row and another owner's row are not own plain jobs", func(t *testing.T) {
		_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "deleg-1", Kind: JobKindAgent, Input: "x", ChildSessionID: "child-1"})
		require.NoError(t, err)
		_, err = store.Claim(ctx, ClaimParams{Owner: "owner-2", ToolCallID: "other-1", Kind: JobKindCommand, Input: "x"})
		require.NoError(t, err)
		live, _ := store.LiveOwnJobs(ctx, "owner-1")
		require.Len(t, live, 1, "only the plain own-1 row")
		assert.Equal(t, "own-1", live[0].ToolCallID)
	})

	t.Run("terminal and dead-host rows are not live", func(t *testing.T) {
		_, err := store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "own-1", State: "completed", NoticeKind: "completed", Wake: true})
		require.NoError(t, err)
		seed, err := TryAcquireFileLock(HostLockPath(store.dataDir, "own-dead-host"))
		require.NoError(t, err)
		require.NoError(t, seed.Release())
		require.NoError(t, claimRunning(ctx, q, "owner-1", "dead-1", "command", "own-dead-host", ""))
		live, incomplete := store.LiveOwnJobs(ctx, "owner-1")
		require.False(t, incomplete)
		require.Empty(t, live)
	})

	t.Run("nil store and empty id", func(t *testing.T) {
		var nilStore *AsyncJobStore
		live, incomplete := nilStore.LiveOwnJobs(ctx, "owner-1")
		require.Empty(t, live)
		require.False(t, incomplete)
		live, incomplete = store.LiveOwnJobs(ctx, "")
		require.Empty(t, live)
		require.False(t, incomplete)
	})
}

// TestJobsInTree_RunsOnTheReadPool is R2A-12's first half: JobsInTree still
// read on the single writer connection, contrary to SetReadConn's doc, so a
// `sessions jobs` walk could stall behind a write transaction. Wiring a read
// pool that cannot answer (an empty DB without the schema) must make the walk
// fail into walkIncomplete instead of silently using the writer.
//
// REVERT CHECK: JobsInTree changed back to s.q.ListAsyncJobsForOwner -- the
// walk kept returning the writer's row with walkIncomplete=false and both
// assertions below failed. Restored; re-ran, passed.
func TestJobsInTree_RunsOnTheReadPool(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	jobs, incomplete := store.JobsInTree(ctx, "owner-1")
	require.Len(t, jobs, 1, "sanity: with no read pool wired the walk uses the writer")
	require.False(t, incomplete)

	other, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "other.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	store.SetReadConn(other)

	jobs, incomplete = store.JobsInTree(ctx, "owner-1")
	require.Empty(t, jobs, "the walk must read from the wired read pool, not the writer")
	require.True(t, incomplete, "a failing read pool must surface as walkIncomplete, never as 'nothing running'")
}

// countingDBTX counts the reader queries a store issues.
type countingDBTX struct {
	db.DBTX
	n *atomic.Int64
}

func (c countingDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	c.n.Add(1)
	return c.DBTX.QueryContext(ctx, query, args...)
}

func (c countingDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	c.n.Add(1)
	return c.DBTX.QueryRowContext(ctx, query, args...)
}

// TestLiveWorkForRoots_MatchesSingleRootReadersInBatchedQueries is R2A-12's
// batched reader: for a forest of sessions -- a delegation chain, a session
// that is both a root and somebody's child, a dead host, an own plain job, a
// shared host, idle sessions -- LiveWorkForRoots must return for every root
// exactly what LiveDescendantJobs and LiveOwnJobs return for it alone, while
// issuing a number of queries that depends on the tree depth, not on the number
// of roots, and probing each distinct host once.
//
// REVERT CHECK: LiveWorkForRoots reimplemented as a per-root loop over
// LiveDescendantJobs/LiveOwnJobs -- the equivalence assertions still passed
// but the query-count assertion failed (2 queries per root instead of one per
// level) and the probe-count assertion failed (one probe per root per call).
// Restored; re-ran, passed.
func TestLiveWorkForRoots_MatchesSingleRootReadersInBatchedQueries(t *testing.T) {
	store, q, ctx := newTestStore(t)
	// Register this store's host so "own" rows are alive without probing.
	ownHost, err := store.ensureHost(ctx)
	require.NoError(t, err)
	for _, id := range []string{"root-a", "mid-a", "leaf-a", "root-b", "solo", "idle-1", "idle-2", "root-dead", "mid-b"} {
		require.NoError(t, seedSession(ctx, q, id))
	}
	// A foreign host that is alive (lock held by this test) and one that is dead.
	aliveLock, err := TryAcquireFileLock(HostLockPath(store.dataDir, "foreign-alive"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aliveLock.Release() })
	fabricateDeadHost(t, ctx, q, store.dataDir, "foreign-dead")

	// root-a -> (delegation) mid-a -> (delegation) leaf-a which runs a plain job
	seedRunningJob(t, ctx, q, "root-a", "d1", ownHost, "mid-a", true)
	seedRunningJob(t, ctx, q, "mid-a", "d2", "foreign-alive", "leaf-a", true)
	seedRunningJob(t, ctx, q, "leaf-a", "p1", "foreign-alive", "", true)
	// mid-a is itself a root of the list (a session that is somebody's child).
	// root-b runs an own plain job and a delegation whose child delegates back to it: a cycle.
	seedRunningJob(t, ctx, q, "root-b", "p2", ownHost, "", true)
	seedRunningJob(t, ctx, q, "root-b", "d3", ownHost, "mid-b", true)
	seedRunningJob(t, ctx, q, "mid-b", "d5", ownHost, "root-b", true)
	// solo: own plain job on the alive foreign host.
	seedRunningJob(t, ctx, q, "solo", "p3", "foreign-alive", "", true)
	// root-dead: delegation on a dead host is not live, and neither is its child.
	seedRunningJob(t, ctx, q, "root-dead", "d4", "foreign-dead", "idle-1", true)
	seedRunningJob(t, ctx, q, "root-dead", "p4", "foreign-dead", "", true)

	roots := []string{"root-a", "mid-a", "leaf-a", "root-b", "solo", "idle-1", "idle-2", "root-dead", "", "root-a", "never-heard-of"}

	var queries atomic.Int64
	store.readQ.Store(db.New(countingDBTX{DBTX: store.sqlDB, n: &queries}))
	var probes atomic.Int64
	origProbe := probeHostSharedFn
	probeHostSharedFn = func(dataDir, hostID string) (HostLockStatus, error) {
		probes.Add(1)
		return origProbe(dataDir, hostID)
	}
	t.Cleanup(func() { probeHostSharedFn = origProbe })

	got := store.LiveWorkForRoots(ctx, roots)
	batchedQueries, batchedProbes := queries.Load(), probes.Load()

	distinct := map[string]struct{}{}
	for _, r := range roots {
		if r != "" {
			distinct[r] = struct{}{}
		}
	}
	require.Len(t, got, len(distinct), "every distinct non-empty root has an entry")
	for root := range distinct {
		wantDesc, wantDescIncomplete := store.LiveDescendantJobs(ctx, root)
		wantOwn, wantOwnIncomplete := store.LiveOwnJobs(ctx, root)
		gotWork := got[root]
		require.ElementsMatchf(t, wantDesc, gotWork.Descendants, "descendants of %s", root)
		require.ElementsMatchf(t, wantOwn, gotWork.Own, "own jobs of %s", root)
		require.Equalf(t, wantDescIncomplete, gotWork.DescendantsIncomplete, "descendants incomplete flag of %s", root)
		require.Equalf(t, wantOwnIncomplete, gotWork.OwnIncomplete, "own incomplete flag of %s", root)
	}
	// Spot-check the shapes the equivalence loop above compares against.
	require.NotEmpty(t, got["root-a"].Descendants, "root-a has a live delegation chain")
	require.Empty(t, got["root-dead"].Descendants, "a delegation on a dead host is not live")
	require.Empty(t, got["root-dead"].Own, "an own job on a dead host is not live")
	require.NotEmpty(t, got["solo"].Own)
	require.Empty(t, got["idle-2"].Descendants)

	// Depth of the tree is 3 levels (root-a -> mid-a -> leaf-a); distinct roots
	// are 9. A per-root walk needs well over 2 queries per root.
	require.LessOrEqual(t, batchedQueries, int64(4), "queries must depend on tree depth, not on the number of roots")
	require.LessOrEqual(t, batchedProbes, int64(2), "one liveness probe per distinct foreign host (alive + dead)")
}

// TestLiveWorkForRoots_ChunksLargeRootSets pins the chunking: an IN (...) over
// every listed session must not exceed SQLite's bound-variable limit, so the
// root set is queried liveWorkQueryChunk owners at a time.
//
// REVERT CHECK: the chunk loop replaced by one query over all owners -- the
// query count assertion failed (1 instead of 3).
func TestLiveWorkForRoots_ChunksLargeRootSets(t *testing.T) {
	store, _, ctx := newTestStore(t)
	roots := make([]string, 0, 2*liveWorkQueryChunk+100)
	for i := 0; i < 2*liveWorkQueryChunk+100; i++ {
		roots = append(roots, fmt.Sprintf("session-%d", i))
	}
	var queries atomic.Int64
	store.readQ.Store(db.New(countingDBTX{DBTX: store.sqlDB, n: &queries}))

	got := store.LiveWorkForRoots(ctx, roots)
	require.Len(t, got, len(roots))
	require.EqualValues(t, 3, queries.Load(), "600+... roots must be queried in ceil(n/%d) chunks", liveWorkQueryChunk)
}
