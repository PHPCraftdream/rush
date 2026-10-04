// WS-1 workspace ownership (task #1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md sec.1). Every test here runs
// against a real, migrated SQLite DB (the same fixture the async-job store
// tests use: db.Connect applies every embedded migration, including the
// workspace columns). Each test carries a revert-check note describing the
// mutation that must break it.

package session

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Workspace roots are opaque strings to the storage layer; they only need to
// differ from each other and from the legacy "".
const (
	wsOwn     = `D:\ws\own`
	wsForeign = `D:\ws\foreign`
)

// wsFx is a migrated DB plus the queries/services a workspace test needs.
type wsFx struct {
	t    *testing.T
	conn *sql.DB
	q    *db.Queries
	ctx  context.Context
}

func newWsFx(t *testing.T) *wsFx {
	t.Helper()
	store, q, ctx := newTestStore(t)
	return &wsFx{t: t, conn: store.sqlDB, q: q, ctx: ctx}
}

// service builds the session service bound to workspaceRoot/workDir with
// home=true -- today's git-checkout process: it owns its own data directory
// (config.WorkspaceHome() is true until SD-D #1143), so legacy ” rows are
// its to drive even though its root is non-empty.
func (f *wsFx) service(workspaceRoot, workDir string) Service {
	return NewServiceWithWorkspace(f.q, f.conn, f.q, f.conn, workspaceRoot, workDir, true)
}

// serviceLinked is service with home=false: the shared-from-linked process
// SD-D (#1143) will introduce -- a workspace of its own, but NOT the owner
// of the data directory, so legacy ” rows are not its to drive. The
// ownership-filter tests use it to keep exercising that half of Owns.
func (f *wsFx) serviceLinked(workspaceRoot, workDir string) Service {
	return NewServiceWithWorkspace(f.q, f.conn, f.q, f.conn, workspaceRoot, workDir, false)
}

// seedSession inserts a top-level session bound to workspaceRoot with an
// explicit updated_at, so ownership-sensitive ordering is deterministic.
func (f *wsFx) seedSession(id, workspaceRoot string, updatedAt int64) db.Session {
	f.t.Helper()
	_, err := f.q.CreateSession(f.ctx, db.CreateSessionParams{
		ID:            id,
		Title:         id,
		WorkspaceRoot: workspaceRoot,
	})
	require.NoError(f.t, err)
	_, err = f.conn.ExecContext(f.ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, updatedAt, id)
	require.NoError(f.t, err)
	row, err := f.q.GetSessionByID(f.ctx, id)
	require.NoError(f.t, err)
	return row
}

func (f *wsFx) getSession(id string) db.Session {
	f.t.Helper()
	row, err := f.q.GetSessionByID(f.ctx, id)
	require.NoError(f.t, err)
	return row
}

// gitWorkDir builds a directory whose .git/HEAD names branch.
func (f *wsFx) gitWorkDir(branch string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	require.NoError(f.t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(f.t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644))
	return dir
}

// TestOwns pins the predicate the Go filter and the SQL filters implement.
func TestOwns(t *testing.T) {
	cases := []struct {
		name string
		sess string
		proc string
		home bool
		want bool
	}{
		{"same workspace", wsOwn, wsOwn, false, true},
		{"foreign workspace", wsForeign, wsOwn, false, false},
		{"legacy row, linked process", "", wsOwn, false, false},
		{"legacy row, home process with a root", "", wsOwn, true, true},
		{"legacy row, undetermined root", "", "", false, true},
		{"same empty root", "", "", false, true},
		{"nothing of ours for the foreign process", wsOwn, wsForeign, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Owns(tc.sess, tc.proc, tc.home))
		})
	}
}

// TestWorkspaceBinding_AllCreationPaths: every creation path fills
// workspace_root with the service's workspace and git_branch from
// <git-dir>/HEAD, and a fork takes the FORKING service's workspace, never the
// source's.
//
// Revert-check: neuter bindWorkspace (return p untouched) -- every
// WorkspaceRoot/GitBranch assertion below fails.
func TestWorkspaceBinding_AllCreationPaths(t *testing.T) {
	f := newWsFx(t)
	workDir := f.gitWorkDir("feature-x")
	svc := f.service(wsOwn, workDir)

	created, err := svc.Create(f.ctx, "cli")
	require.NoError(t, err)
	withID, err := svc.CreateWithID(f.ctx, "run-cli-id", "cli id")
	require.NoError(t, err)
	withOrigin, err := svc.(OriginCreator).CreateWithOrigin(f.ctx, "web", message.OriginWeb)
	require.NoError(t, err)
	withIDOrigin, err := svc.(OriginCreator).CreateWithIDAndOrigin(f.ctx, "web-id", "web id", message.OriginWeb)
	require.NoError(t, err)
	title, err := svc.CreateTitleSession(f.ctx, created.ID)
	require.NoError(t, err)
	task, err := svc.CreateTaskSession(f.ctx, "call-1", created.ID, "delegated")
	require.NoError(t, err)

	for _, id := range []string{created.ID, withID.ID, withOrigin.ID, withIDOrigin.ID, title.ID, task.ID} {
		row := f.getSession(id)
		require.Equal(t, wsOwn, row.WorkspaceRoot, "session %s must be bound to the service workspace", id)
		require.Equal(t, "feature-x", row.GitBranch, "session %s must carry the workDir branch", id)
	}

	// No git directory at all: branch is "", never an error.
	noBranch, err := f.service(wsOwn, f.t.TempDir()).Create(f.ctx, "branchless")
	require.NoError(t, err)
	require.Equal(t, wsOwn, f.getSession(noBranch.ID).WorkspaceRoot)
	require.Empty(t, f.getSession(noBranch.ID).GitBranch)

	// A gitdir: indirection (linked worktree) is followed to the real HEAD.
	linked := f.t.TempDir()
	realGit := f.t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(realGit, "HEAD"), []byte("ref: refs/heads/wt-branch\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+realGit+"\n"), 0o644))
	linkedSess, err := f.service(wsOwn, linked).Create(f.ctx, "linked")
	require.NoError(t, err)
	require.Equal(t, "wt-branch", f.getSession(linkedSess.ID).GitBranch)

	// Detached HEAD: "" (the branch is audit attribution, not ownership).
	detached := f.t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(detached, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(detached, ".git", "HEAD"), []byte("0123456789abcdef0123456789abcdef01234567\n"), 0o644))
	detachedSess, err := f.service(wsOwn, detached).Create(f.ctx, "detached")
	require.NoError(t, err)
	require.Empty(t, f.getSession(detachedSess.ID).GitBranch)

	// Fork: the fork belongs to the FORKING service's workspace even when the
	// source belongs to a different (foreign) one -- `sessions fork` is the
	// only supported way to continue a history in another checkout.
	f.seedSession("src-foreign", wsForeign, 100)
	fork, copied, err := svc.ForkSessionTx(f.ctx, "src-foreign", ForkOptions{NewID: "fork-mine", Title: "forked"})
	require.NoError(t, err)
	require.Equal(t, 0, copied, "the seeded foreign source has no messages")
	require.Equal(t, wsOwn, fork.WorkspaceRoot, "fork must take the forking service's workspace, not the source's")
	require.Equal(t, "feature-x", fork.GitBranch)
	require.Equal(t, wsOwn, f.getSession("fork-mine").WorkspaceRoot)
}

// TestGetLast_OwnershipFilter: `--continue` only ever resumes the caller's own
// sessions; a legacy ” row is only visible to a home process.
//
// Revert-check: drop the ownership predicate from getLastSessionSubtree
// (back to a bare parent_session_id IS NULL) -- the foreign, most-recent
// session is handed to both services and the ID assertions fail.
func TestGetLast_OwnershipFilter(t *testing.T) {
	f := newWsFx(t)
	svcOwn := f.serviceLinked(wsOwn, "")
	svcForeign := f.serviceLinked(wsForeign, "")
	svcHome := f.service("", "")

	// The foreign row is the MOST RECENT of all three: an unfiltered
	// GetLastSession would hand it to every process.
	f.seedSession("legacy-row", "", 100)
	f.seedSession("own-row", wsOwn, 300)
	f.seedSession("foreign-row", wsForeign, 400)

	own, err := svcOwn.GetLast(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "own-row", own.ID, "a linked process only continues its own workspace")

	foreign, err := svcForeign.GetLast(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "foreign-row", foreign.ID, "the foreign process continues its own workspace")

	home, err := svcHome.GetLast(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "legacy-row", home.ID, "a home process only sees the legacy unbound row, never a newer foreign one")
}

// TestListPendingRunQueueEntries_OwnershipFilter: the pump scan only lists
// rows of sessions this process owns; a foreign row stays pending with its
// attempts untouched.
//
// Revert-check: drop the JOIN + ownership predicate from
// ListPendingRunQueueEntries in run_queue.sql and regenerate -- the foreign
// entry shows up in the linked scan and the length/ID assertions fail.
func TestListPendingRunQueueEntries_OwnershipFilter(t *testing.T) {
	f := newWsFx(t)
	svcOwn := f.serviceLinked(wsOwn, "")
	svcHome := f.service("", "")

	f.seedSession("own-sess", wsOwn, 0)
	f.seedSession("foreign-sess", wsForeign, 0)
	f.seedSession("legacy-sess", "", 0)

	for _, sessionID := range []string{"own-sess", "foreign-sess", "legacy-sess"} {
		require.NoError(t, svcOwn.EnqueueRunQueueEntry(f.ctx, "key-"+sessionID, sessionID, []byte(`{}`)))
	}

	own, err := svcOwn.ListPendingRunQueueEntries(f.ctx)
	require.NoError(t, err)
	require.Len(t, own, 1, "only the caller's own session row may be scanned")
	require.Equal(t, "own-sess", own[0].SessionID)

	home, err := svcHome.ListPendingRunQueueEntries(f.ctx)
	require.NoError(t, err)
	require.Len(t, home, 1, "a home process scans the legacy row only")
	require.Equal(t, "legacy-sess", home[0].SessionID)

	// The foreign row is untouched: still pending, no attempt consumed by a
	// scan that must never have seen it.
	foreign, err := f.q.GetRunQueueEntry(f.ctx, "key-foreign-sess")
	require.NoError(t, err)
	require.Equal(t, "pending", foreign.Status)
	require.Equal(t, int64(0), foreign.Attempts)
}

// TestWakeScheduleOwnershipFilter: BOTH scheduler reads carry the ownership
// predicate -- ClaimDue never leases a foreign row and NextDue never reports a
// due moment this process cannot claim (the P0 infinite-loop shape: NextDue in
// the past, claim empty, repeat).
//
// Revert-check (both halves are load-bearing): remove the ownership predicate
// from NextDueWakeScheduleAt only -- the own/home NextDue assertions fail;
// remove it from ListDueWakeSchedules only -- the ClaimDue assertions fail.
func TestWakeScheduleOwnershipFilter(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newWsFx(t)

	f.seedSession("wake-own", wsOwn, 0)
	f.seedSession("wake-foreign", wsForeign, 0)
	f.seedSession("wake-legacy", "", 0)

	seed := NewWakeScheduleStore(f.conn)
	// RunAt values are all at least MinWakeOnceDelay past the creation clock
	// (base); the foreign one is the EARLIEST, so an unfiltered NextDue points
	// every other process at a moment only its owner may claim.
	foreignDue := base.Add(5 * time.Second)
	ownDue := base.Add(time.Hour)
	legacyDue := base.Add(30 * time.Minute)
	foreignRow, err := seed.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: "wake-foreign", Kind: WakeKindOnce, Message: "foreign", RunAt: foreignDue,
	}, base)
	require.NoError(t, err)
	ownRow, err := seed.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: "wake-own", Kind: WakeKindOnce, Message: "own", RunAt: ownDue,
	}, base)
	require.NoError(t, err)
	legacyRow, err := seed.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: "wake-legacy", Kind: WakeKindOnce, Message: "legacy", RunAt: legacyDue,
	}, base)
	require.NoError(t, err)

	own := NewWakeScheduleStoreWithWorkspace(f.conn, wsOwn, "", false)
	foreign := NewWakeScheduleStoreWithWorkspace(f.conn, wsForeign, "", false)
	home := NewWakeScheduleStore(f.conn)

	// NextDue: the owning process sleeps until its OWN due moment, never
	// until the earlier foreign moment.
	next, err := own.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.True(t, next.Equal(ownDue), "NextDue must be the own due time, got %v", next)

	// The foreign store does see its own row -- the positive control proving
	// the filter is ownership, not a blanket exclusion.
	nextForeign, err := foreign.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, nextForeign)
	require.True(t, nextForeign.Equal(foreignDue))

	// A home process only ever sees the legacy row.
	nextHome, err := home.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, nextHome)
	require.True(t, nextHome.Equal(legacyDue), "home NextDue must be the legacy due time, got %v", nextHome)

	// ClaimDue at a moment past every due time: the owning process claims
	// exactly its own row; the already-due foreign row is left alone.
	claimAt := base.Add(2 * time.Hour)
	claimed, err := own.ClaimDue(f.ctx, "pump-own", claimAt, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, ownRow.ID, claimed[0].ID)

	claimedHome, err := home.ClaimDue(f.ctx, "pump-home", claimAt, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimedHome, 1)
	require.Equal(t, legacyRow.ID, claimedHome[0].ID)

	// The foreign row survives both other claims untouched: unleased, still
	// active -- the scheduler that owns it will fire it.
	stillForeign, err := f.q.GetWakeSchedule(f.ctx, foreignRow.ID)
	require.NoError(t, err)
	require.Equal(t, "active", stillForeign.State)
	require.False(t, stillForeign.LeaseOwner.Valid, "a foreign due row must never be leased by another workspace")

	// And the foreign owner really can claim it (ownership, not exclusion).
	claimedForeign, err := foreign.ClaimDue(f.ctx, "pump-foreign", claimAt, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimedForeign, 1)
	require.Equal(t, foreignRow.ID, claimedForeign[0].ID)
}

// TestP1142_LegacyDrivenByGitCheckoutProcess is the SD-C home-vs-root
// regression pair (#1142 step C; the pre-SD-D world): a process with a
// NON-EMPTY workspace root is still a HOME process today
// (config.WorkspaceHome() == true), so every pre-existing legacy ” row --
// the shape ALL pre-WS-1 rows have -- stays its to continue and to pump.
// Deriving home from the root (the first draft's `workspaceRoot == ""`)
// turned this process into a shared-from-linked one and refused it its own
// history: --continue found nothing and the pump skipped legacy rows.
//
// Revert-check (mutant A, service half): in session_workspace.go's ownsArgs
// restore `homeFlag(s.workspaceRoot == "")` (the service ignores its home
// field) and regenerate nothing -- both sub-tests fail: GetLast errors with
// "no sessions found" (the legacy row is the only one seeded) and the pump
// scan comes back empty.
func TestP1142_LegacyDrivenByGitCheckoutProcess(t *testing.T) {
	f := newWsFx(t)
	// A git-checkout process: non-empty root, home per the single source.
	svc := f.service(wsOwn, "")

	t.Run("GetLast continues the legacy row", func(t *testing.T) {
		f.seedSession("legacy-latest", "", 500)
		got, err := svc.GetLast(f.ctx)
		require.NoError(t, err, "a home git-checkout process must continue its legacy history")
		require.Equal(t, "legacy-latest", got.ID)
	})

	t.Run("pump scan lists the legacy run-queue row", func(t *testing.T) {
		f.seedSession("legacy-queue", "", 0)
		require.NoError(t, svc.EnqueueRunQueueEntry(f.ctx, "legacy-rq", "legacy-queue", []byte(`{}`)))
		rows, err := svc.ListPendingRunQueueEntries(f.ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1, "the legacy row is this process's to execute")
		require.Equal(t, "legacy-queue", rows[0].SessionID)
	})
}

// TestP1142_LegacyWakeClaimedByGitCheckoutProcess is the wake half of the
// same regression: a non-empty-root HOME process's scheduler sees and claims
// the schedule of a legacy ” session (the only shape pre-WS-1 schedules
// have). With home derived from the root this claim came back empty forever
// while NextDue kept reporting the moment -- the P0 spin, one checkout wide.
//
// Revert-check (mutant A, store half): in wake_schedule_store.go's ownsArgs
// restore `homeFlag(s.workspaceRoot == "")` -- NextDue reports the due time
// but ClaimDue returns zero rows (the assertion below at require.Len fails
// first with "0 rows claimed").
func TestP1142_LegacyWakeClaimedByGitCheckoutProcess(t *testing.T) {
	base := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	f := newWsFx(t)
	f.seedSession("legacy-wake", "", 0)

	due := base.Add(time.Hour)
	seed := NewWakeScheduleStore(f.conn)
	row, err := seed.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: "legacy-wake", Kind: WakeKindOnce, Message: "legacy", RunAt: due,
	}, base)
	require.NoError(t, err)

	// The git-checkout process's scheduler: non-empty root, home.
	store := NewWakeScheduleStoreWithWorkspace(f.conn, wsOwn, "", true)

	next, err := store.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next, "the legacy schedule is this process's timer input")
	require.True(t, next.Equal(due), "NextDue must be the legacy row's due time, got %v", next)

	claimed, err := store.ClaimDue(f.ctx, "pump-git-checkout", base.Add(2*time.Hour), 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "the legacy schedule must be claimable by its home process")
	require.Equal(t, row.ID, claimed[0].ID)
}
