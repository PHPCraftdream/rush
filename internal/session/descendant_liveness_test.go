package session

// Cross-process descendant-liveness walk tests. These exercise the shared
// helper every status surface consumes (`sessions list`,
// markDelegatingLiveDescendants; `sessions why`, explainSessionStatus; the
// web session list, annotateLiveDescendantWork), so the transitivity,
// cycle-guard and depth-bound rules are proven once, at the source.
//
// The production bug these guard: a parent/root session whose per-turn lock
// was released reported "done" while an implementation sub-agent session
// still held a LIVE lock and the outer `rush run` was still waiting on it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newDescendantTestService builds the real session Service over the
// package's file-backed test DB so the parent→child linkage is genuine
// durable state, not an in-memory fake.
func newDescendantTestService(t *testing.T) Service {
	t.Helper()
	conn, q := newTestDB(t)
	return NewService(q, conn)
}

// TestLiveDescendants_DirectChildHoldsLiveLock is the core regression: the
// child session holds a REAL exclusive lock (acquired in-process, so the
// lock file, its PID sidecar and the heartbeat are all genuine), the parent
// has nothing of its own — the walk must report the child as a live
// descendant at depth 1.
func TestLiveDescendants_DirectChildHoldsLiveLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	parent, err := s.Create(ctx, "live-descendants parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "live-descendants child", parent.ID, "child")
	require.NoError(t, err)
	require.Equal(t, parent.ID, child.ParentSessionID,
		"test setup assumption: the child row must carry the parent linkage")

	dataDir := t.TempDir()
	lock, err := TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()

	live, incomplete := LiveDescendants(ctx, s, dataDir, parent.ID)
	require.False(t, incomplete, "a healthy DB must enumerate the whole tree")
	require.Len(t, live, 1, "the live child must be reported")
	require.Equal(t, child.ID, live[0].ID)
	require.Equal(t, 1, live[0].Depth, "a direct child is depth 1")
	require.True(t, live[0].Lock.Live, "the observed lock must be live")
	require.True(t, live[0].Lock.Exists, "the child's lock file must exist")
}

// TestLiveDescendants_ChildReleasedThenAgedIsNotLive is the companion: once
// the child's lock is released the parent is no longer waiting on
// anything, so the walk must report no live descendants and the parent can
// go back to done/at rest.
//
// Note the mtime back-dating: clearHolderMetadata truncates the lock file
// on release rather than unlinking it, which leaves a fresh mtime behind,
// so a lock released moments ago still reads live for up to
// LockStaleDuration — the same post-release window the web server's
// external-ownership annotation already tolerates. Back-dating simulates a
// release that happened longer ago than that window.
func TestLiveDescendants_ChildReleasedThenAgedIsNotLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	parent, err := s.Create(ctx, "released child parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "released child", parent.ID, "child")
	require.NoError(t, err)

	dataDir := t.TempDir()
	lock, err := TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	require.NoError(t, lock.Release())

	// While held the child was live — prove the fixture was real.
	live, _ := LiveDescendants(ctx, s, dataDir, parent.ID)
	require.Len(t, live, 1, "sanity: before the back-date the just-released lock is still inside the post-release window")

	releasedAgo := time.Now().Add(-(LockStaleDuration + 5*time.Second))
	lockPath := SessionLockPath(dataDir, child.ID)
	require.NoError(t, os.Chtimes(lockPath, releasedAgo, releasedAgo))

	live, incomplete := LiveDescendants(ctx, s, dataDir, parent.ID)
	require.False(t, incomplete)
	require.Empty(t, live, "a child whose lock was released longer ago than LockStaleDuration is not live")
}

// TestLiveDescendants_GrandchildHoldsLiveLock proves transitivity: an
// intermediate child may be entirely at rest (no lock of its own) while a
// GRANDCHILD is still working. The root must still be reported as having
// live descendant work — the walk visits every level, not just the direct
// children.
func TestLiveDescendants_GrandchildHoldsLiveLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	root, err := s.Create(ctx, "transitive root")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "transitive child", root.ID, "child")
	require.NoError(t, err)
	grandchild, err := s.CreateTaskSession(ctx, "transitive grandchild", child.ID, "grandchild")
	require.NoError(t, err)

	dataDir := t.TempDir()
	lock, err := TryAcquireSessionLock(dataDir, grandchild.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()

	live, incomplete := LiveDescendants(ctx, s, dataDir, root.ID)
	require.False(t, incomplete)
	require.Len(t, live, 1, "the live grandchild must reach the root")
	require.Equal(t, grandchild.ID, live[0].ID)
	require.Equal(t, 2, live[0].Depth, "a grandchild is depth 2")
}

// TestLiveDescendants_DeadHolderStaleHeartbeatIsNotLive: a descendant whose
// lock records a dead PID and whose heartbeat is stale is crashed work,
// not live work, so it must not hold the parent non-terminal.
func TestLiveDescendants_DeadHolderStaleHeartbeatIsNotLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	parent, err := s.Create(ctx, "crashed child parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "crashed child", parent.ID, "child")
	require.NoError(t, err)

	dataDir := t.TempDir()
	lockPath := SessionLockPath(dataDir, child.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	// PID 999999 is guaranteed not to be a live process on any platform.
	require.NoError(t, os.WriteFile(lockPath, []byte("999999\n"), 0o644))
	stale := time.Now().Add(-(LockStaleDuration + 30*time.Second))
	require.NoError(t, os.Chtimes(lockPath, stale, stale))

	live, incomplete := LiveDescendants(ctx, s, dataDir, parent.ID)
	require.False(t, incomplete)
	require.Empty(t, live, "a dead holder with a stale heartbeat is not live descendant work")
}

// TestLiveDescendants_CyclicLinkageTerminates guards against linkage data
// that points back at an already-visited session: a→b→a must terminate
// rather than spin, and report only what it actually saw.
func TestLiveDescendants_CyclicLinkageTerminates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	// Each row names the other as its parent — the cycle.
	_, err := s.CreateTaskSession(ctx, "cycle-a", "cycle-b", "cycle a")
	require.NoError(t, err)
	_, err = s.CreateTaskSession(ctx, "cycle-b", "cycle-a", "cycle b")
	require.NoError(t, err)

	dataDir := t.TempDir()
	live, incomplete := LiveDescendants(ctx, s, dataDir, "cycle-a")
	require.False(t, incomplete)
	require.Empty(t, live, "a cycle with no live locks must simply report nothing")
}

// TestLiveDescendants_DepthBound caps how deep the walk goes. A chain
// deeper than maxDescendantWalkDepth is corrupt data, not a real call
// tree; the bound must keep the walk finite instead of trusting the data.
func TestLiveDescendants_DepthBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)

	// A chain of exactly maxDescendantWalkDepth descendants under one root.
	const depth = maxDescendantWalkDepth
	rootID := "depth-root"
	parent := rootID
	for i := 1; i <= depth; i++ {
		id := "depth-" + string(rune('a'+i))
		_, err := s.CreateTaskSession(ctx, id, parent, "chain node")
		require.NoError(t, err)
		parent = id
	}

	dataDir := t.TempDir()
	lock, err := TryAcquireSessionLock(dataDir, parent)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()

	live, incomplete := LiveDescendants(ctx, s, dataDir, rootID)
	require.False(t, incomplete)
	require.Len(t, live, 1, "a live lock at exactly the depth bound must still be found")
	require.Equal(t, depth, live[0].Depth)

	// One level deeper than the bound: the deepest lock must NOT be seen.
	deeper := "depth-beyond"
	_, err = s.CreateTaskSession(ctx, deeper, parent, "beyond the bound")
	require.NoError(t, err)
	deeperLock, err := TryAcquireSessionLock(dataDir, deeper)
	require.NoError(t, err)
	defer func() { require.NoError(t, deeperLock.Release()) }()

	live, incomplete = LiveDescendants(ctx, s, dataDir, rootID)
	require.False(t, incomplete)
	require.Len(t, live, 1, "the walk must stop at the depth bound and not report the lock below it")
	require.Equal(t, parent, live[0].ID, "only the in-bound live lock may be reported")
}

// failingLister is a SubSessionLister whose child listing always fails, to
// prove the walk reports walkIncomplete instead of quietly answering "no
// live descendants" for a tree it could not enumerate.
type failingLister struct{}

func (failingLister) ListSubSessions(context.Context, string) ([]Session, error) {
	return nil, os.ErrPermission
}

func TestLiveDescendants_ListingFailureReportsIncomplete(t *testing.T) {
	t.Parallel()

	live, incomplete := LiveDescendants(context.Background(), failingLister{}, t.TempDir(), "any-session")
	require.True(t, incomplete, "a failed child listing must not look like an empty tree")
	require.Empty(t, live)
}

// TestLiveDescendants_NilAndEmptyInputs: the helper must stay safe for the
// degenerate inputs the display surfaces hand it (no app, no data dir, no
// session id) rather than panicking a status command.
func TestLiveDescendants_NilAndEmptyInputs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	dataDir := t.TempDir()

	live, incomplete := LiveDescendants(ctx, nil, dataDir, "x")
	require.False(t, incomplete)
	require.Empty(t, live)

	live, incomplete = LiveDescendants(ctx, s, "", "x")
	require.False(t, incomplete)
	require.Empty(t, live)

	live, incomplete = LiveDescendants(ctx, s, dataDir, "")
	require.False(t, incomplete)
	require.Empty(t, live)
}

// TestLiveDescendants_UnreadableLockFileCountsAsLive: a descendant whose
// lock could not be inspected at all (StatErr) is reported as live. A
// status surface must not answer "done" for work it failed to verify —
// the same fail-closed posture the startup recovery sweep takes.
//
// The stat failure is forged with a NUL byte inside the data directory:
// Go's os package rejects NUL bytes in a path before any syscall, on every
// platform, so this can never be misclassified as ENOENT (the same
// technique explainSessionStatus's stat-failure test uses).
func TestLiveDescendants_UnreadableLockFileCountsAsLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newDescendantTestService(t)
	parent, err := s.Create(ctx, "unreadable child parent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "unreadable child", parent.ID, "child")
	require.NoError(t, err)

	dataDir := t.TempDir() + string([]byte{0}) + "bad"

	live, incomplete := LiveDescendants(ctx, s, dataDir, parent.ID)
	require.False(t, incomplete)
	require.Len(t, live, 1, "an unverifiable descendant lock must not clear the parent")
	require.Equal(t, child.ID, live[0].ID, "the reported live descendant must be the created child")
	require.Error(t, live[0].Lock.StatErr)
	require.False(t, live[0].Lock.Live)
}
