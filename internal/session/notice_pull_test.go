// Notice-pull coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.3/3.4, step 3): the transaction-per-notice pull's exactly-once
// guarantee across two independent processes (DUR-3b), a per-row pull
// failure never propagating to the caller (DUR-3c), and the two void
// conditions (wake_only, supervision).
package session

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// jobNoticeParams and sessionNoticeParams are minimal build callbacks for
// PullJobNotices/PullSessionNotices -- the real ones (agent package's
// buildJobNoticeMessageParams/buildSessionNoticeMessageParams) live behind
// the package boundary these tests exercise from the other side.
func jobNoticeParams(row JobNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: "job notice " + row.ToolCallID}},
		BackgroundJobNotice: true,
		NoticeKind:          row.NoticeKind,
	}
}

func sessionNoticeParams(row SessionNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: row.Text}},
		BackgroundJobNotice: true,
		NoticeKind:          row.Kind,
	}
}

// twoConnStores opens TWO independent *sql.DB connections to the SAME
// on-disk database file, each wrapped in its own AsyncJobStore -- the
// "two drivers in two processes" shape DUR-3b requires (a single *sql.DB
// would just serialize on its own connection pool, proving nothing about
// cross-process safety; two separate connections is the closest a single
// test process can get to that).
func twoConnStores(t *testing.T, sessionIDs ...string) (a, b *AsyncJobStore, ctx context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx = context.Background()
	setup, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	for _, id := range sessionIDs {
		require.NoError(t, seedSession(ctx, db.New(setup), id))
	}
	require.NoError(t, db.Release(dataDir))

	path := dataDir + "/rush.db"
	connA, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	connA.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connA.Close() })
	connB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	connB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = connB.Close() })

	a = NewAsyncJobStore(connA, dataDir, 1, "process-a")
	b = NewAsyncJobStore(connB, dataDir, 2, "process-b")
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return a, b, ctx
}

// countMessages returns the number of messages rows for sessionID.
func countMessages(t *testing.T, q *db.Queries, ctx context.Context, sessionID string) int64 {
	t.Helper()
	n, err := q.CountMessagesBySession(ctx, sessionID)
	require.NoError(t, err)
	return n
}

// TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage pins DUR-3b: two
// drivers (two *sql.DB connections to the same file, standing in for two
// processes/hosts) racing to pull the SAME pending async_jobs row must
// produce exactly one history message -- the pull's UPDATE ... WHERE
// delivery='pending' CAS is the sole arbiter, not which goroutine happens to
// run first.
//
// Calls the unexported pullOneJobNotice directly with the SAME pre-fetched
// candidate row on both stores, rather than going through PullJobNotices'
// own List+pull pair on each side: with List included, one side's entire
// list-pull-insert-commit sequence reliably finishes before the other's List
// call even runs (it then sees zero pending candidates and never reaches the
// per-row CAS at all), which would pass even with the CAS guard removed --
// exercising the LIST step's own filter, not the CAS. Sharing one candidate
// forces both goroutines at the actual UPDATE ... WHERE delivery='pending'
// race.
//
// Revert-check performed: removed the `AND delivery = 'pending'` guard from
// PullPendingAsyncJobNotice's SQL (internal/db/sql/async_jobs.sql) and ran
// `sqlc generate` -- this test FAILED (message count 2: both goroutines'
// UPDATE matched and both inserted a message). Restored the guarded query,
// re-ran `sqlc generate`, re-ran the test: passed.
func TestPullJobNotices_TwoStoresOnSameFile_ExactlyOneMessage(t *testing.T) {
	storeA, storeB, ctx := twoConnStores(t, "owner-1")

	_, err := storeA.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, storeA.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = storeA.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	candidate, err := storeA.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", candidate.Delivery)

	messagesA := message.NewService(db.New(storeA.sqlDB))
	messagesB := message.NewService(db.New(storeB.sqlDB))

	var wg sync.WaitGroup
	var pulledA, pulledB bool
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, pulledA, errA = storeA.pullOneJobNotice(ctx, messagesA, "owner-1", candidate, jobNoticeParams)
	}()
	go func() {
		defer wg.Done()
		_, pulledB, errB = storeB.pullOneJobNotice(ctx, messagesB, "owner-1", candidate, jobNoticeParams)
	}()
	wg.Wait()
	require.NoError(t, errA)
	require.NoError(t, errB)

	total := 0
	if pulledA {
		total++
	}
	if pulledB {
		total++
	}
	require.Equal(t, 1, total, "exactly one of the two concurrent pulls must have won the row")
	require.EqualValues(t, 1, countMessages(t, storeA.q, ctx, "owner-1"), "exactly one message must be persisted")

	row, err := storeA.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
}

// failingCreateTxMessages wraps a real message.Service and makes CreateTx
// always fail -- DUR-3c: a pull error must not fail the caller, and the row
// must stay pending for a later pull.
type failingCreateTxMessages struct {
	message.Service
}

var errSimulatedCreateTxFailure = errors.New("simulated message insert failure")

func (f *failingCreateTxMessages) CreateTx(context.Context, *sql.Tx, string, message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, errSimulatedCreateTxFailure
}

// TestPullJobNotices_CreateTxErrorLeavesRowPending pins DUR-3c: a per-row
// pull failure (here, the message insert itself failing) is logged and
// skipped -- PullJobNotices returns no error and the row's delivery stays
// 'pending' for a later pull to retry, rather than the whole pull call
// failing or the row being marked done/void with no message to show for it.
//
// Revert-check performed: changed PullJobNotices' per-row error handling
// from `continue` to `return nil, err` -- this test FAILED (PullJobNotices
// returned a non-nil error instead of nil). Restored the `continue`; re-ran,
// passed.
func TestPullJobNotices_CreateTxErrorLeavesRowPending(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)

	failing := &failingCreateTxMessages{Service: message.NewService(store.q)}
	pulled, pullErr := store.PullJobNotices(ctx, failing, "owner-1", jobNoticeParams)
	require.NoError(t, pullErr, "a per-row pull error must never fail the whole pull call")
	require.Empty(t, pulled)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "the row must stay pending after a failed insert, not be marked done")
	require.False(t, row.NoticeMessageID.Valid)
}

// TestPullSessionNotices_WakeOnlyVoidsWhenJobNoLongerRunning and
// TestPullSessionNotices_WakeOnlyDeliversWhileJobStillRunning pin doc
// sec.3.4's first void rule: a wake_only check-in names a specific
// async_jobs row (job_tool_call_id) and is void at pull time iff that job is
// no longer 'running'.
//
// Revert-check performed (shared with the "still running" test below):
// changed sessionNoticeVoidCondition's NoticeKindWakeOnly branch to always
// return false (never void) -- TestPullSessionNotices_WakeOnlyVoidsWhenJobNoLongerRunning
// FAILED (a message was created for a job that had already completed).
// Restored the real condition; re-ran, both tests passed.
func TestPullSessionNotices_WakeOnlyVoidsWhenJobNoLongerRunning(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	// The job finishes BEFORE the wake_only check-in is ever pulled.
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindWakeOnly, "still running, please wait", true, "call-1"))

	messages := message.NewService(store.q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a wake_only notice for a job that finished before it was pulled must be voided, not delivered")

	notices, err := store.ListSessionNotices(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, notices, 1)

	var delivery string
	require.NoError(t, store.sqlDB.QueryRowContext(ctx, `SELECT delivery FROM session_notices WHERE id = ?`, notices[0].ID).Scan(&delivery))
	require.Equal(t, "void", delivery)
}

func TestPullSessionNotices_WakeOnlyDeliversWhileJobStillRunning(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	// The job is still running when the check-in is pulled.

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindWakeOnly, "still running, please wait", true, "call-1"))

	messages := message.NewService(store.q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "a wake_only notice for a job still running must be delivered, not voided")
	require.True(t, pulled[0].Wake)
}

// TestPullSessionNotices_SupervisionVoidsWhenScopeClosed and
// TestPullSessionNotices_SupervisionDeliversWhileScopeOpen pin doc sec.3.4's
// second void rule: a supervision check-in is debt only while the owner's
// scope still has a running async_jobs row; otherwise it voids at pull time.
//
// Revert-check performed: changed sessionNoticeVoidCondition's
// NoticeKindSupervision branch to always return false -- the "closed" test
// FAILED (a message was created for a scope with zero running jobs).
// Restored the real condition; re-ran, both tests passed.
func TestPullSessionNotices_SupervisionVoidsWhenScopeClosed(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	// No async_jobs row at all for owner-1: the scope is closed.

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in", true, ""))

	messages := message.NewService(store.q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a supervision check-in must void once the scope has no running row left")
}

func TestPullSessionNotices_SupervisionDeliversWhileScopeOpen(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in", true, ""))

	messages := message.NewService(store.q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "a supervision check-in must be delivered while the scope still has a running row")
}

// TestPullSessionNotices_SupervisionVoidsWhenOnlyRunningRowIsOnADeadHost is
// A15's fix: the supervision void condition must reuse the scope's own
// liveness predicate, not count ANY running row regardless of whether its
// host is provably dead. A running row on a confirmed-dead host is not open
// scope.
//
// REVERT CHECK: temporarily reverted the void condition to
// `len(running) == 0` (pre-fix, counting the dead-host row as open scope).
// This test's `require.Empty(t, pulled, ...)` FAILED (a message was created
// even though the only running row's host was dead). Restored the
// liveness-checking fix; re-ran, passed.
func TestPullSessionNotices_SupervisionVoidsWhenOnlyRunningRowIsOnADeadHost(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	seed, err := TryAcquireFileLock(HostLockPath(store.dataDir, "dead-host-supervision"))
	require.NoError(t, err)
	require.NoError(t, seed.Release())
	require.NoError(t, claimRunning(ctx, q, "owner-1", "call-1", "command", "dead-host-supervision", ""))

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in", true, ""))

	messages := message.NewService(store.q)
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a supervision check-in must void when the owner's only running row is on a provably dead host")
}

// TestPullJobNotices_InsertsMessageWithNoticeInvariant proves the pulled
// job-notice message satisfies the web composer history filter's invariant
// (BackgroundJobNotice=true or a non-empty NoticeKind) via the build
// callback's own params, and that notice_message_id is recorded on the row.
func TestPullJobNotices_InsertsMessageWithNoticeInvariant(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)

	messages := message.NewService(store.q)
	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", jobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.True(t, pulled[0].Message.BackgroundJobNotice)
	require.True(t, pulled[0].Wake)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
	require.True(t, row.NoticeMessageID.Valid)
	require.Equal(t, pulled[0].Message.ID, row.NoticeMessageID.String)

	// A second pull must find nothing left to pull (idempotent: the row is
	// already 'done').
	pulled2, err := store.PullJobNotices(ctx, messages, "owner-1", jobNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled2)
}

// TestPullJobNotices_JobKillDeliveryDoneNeverPulled pins the Transition
// Delivery parameter's 'done' path (doc sec.3.2, used by step 6's job_kill):
// a row committed straight to 'done' is never pulled and never produces a
// message. In step 3 every cause, job_kill included, still passes 'pending'.
func TestPullJobNotices_JobKillDeliveryDoneNeverPulled(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	res, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
		ResultSummary: "killed on request", Wake: false, Delivery: "done",
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, res.Outcome)
	require.Equal(t, "done", res.Row.Delivery, "Delivery 'done' must be committed as given, never rewritten to pending")

	messages := message.NewService(store.q)
	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", jobNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a row committed straight to done must never be pulled as a notice")
	require.EqualValues(t, 0, countMessages(t, store.q, ctx, "owner-1"))
}

// TestPullJobNotices_JobKillRowNeverSurfacesAsPulledNotice pins step 6's
// ACTUAL, CURRENT job_kill contract (doc sec.3.2): job_kill's own tool
// response IS the job's answer, so its Transition call commits
// delivery='done'/reacted=1 directly -- never 'pending' -- so the row never
// also surfaces as a second, pulled history notice. An earlier draft of this
// test pinned a DIFFERENT, since-superseded step-3 contract (job_kill keeping
// delivery='pending' like every other cause, delivered once via the ordinary
// pull) that production's causeStateNoticeKindWake (work_ledger_transition.go)
// has never actually implemented since step 6 landed -- this test now
// asserts what the store really does when given the EXACT params
// causeJobKill produces.
//
// Revert-check performed: changed the Transition call below back to the
// stale contract (Delivery/Reacted left at their zero values, defaulting to
// pending/false) -- PullJobNotices then found and delivered the row (this
// test's own require.Empty failed). Restored Delivery:"done"/Reacted:true;
// re-ran, passed.
func TestPullJobNotices_JobKillRowNeverSurfacesAsPulledNotice(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	res, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
		ResultSummary: "partial output before the stop", Wake: false,
		Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)
	require.Equal(t, "done", res.Row.Delivery, "job_kill's Transition call commits delivery='done' directly, not 'pending'")
	require.EqualValues(t, 1, res.Row.Reacted)
	require.EqualValues(t, 0, res.Row.Wake)

	messages := message.NewService(store.q)
	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", jobNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a job_kill'd row (delivery='done' already) must never also surface as a pulled notice")
	require.EqualValues(t, 0, countMessages(t, store.q, ctx, "owner-1"), "job_kill's own tool response is the answer -- the pull inserts no second message for it")
}

// TestPullSessionNotices_SupervisionVoidsWhenOnlyRunningRowIsVoided is R2A-9:
// a Rerun voids a still-running delegation (delivery='void', state stays
// 'running' while its executor is stopped); that row is not open scope, so a
// supervision check-in with no other running row must void at pull time
// instead of being delivered as a wake=1 notice over rolled-back work (§6).
//
// REVERT CHECK: the `r.Delivery == "void"` skip removed from
// sessionNoticeVoidCondition's supervision branch -- the notice was delivered
// (pulled had 1 entry) and the require.Empty below failed. Restored; re-ran,
// passed.
func TestPullSessionNotices_SupervisionVoidsWhenOnlyRunningRowIsVoided(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(store.q)

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'owner-1' AND tool_call_id = 'call-1'`)
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in", true, ""))
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a Rerun-voided running row is not open scope: the supervision check-in must void")

	// Control: a second, non-voided running row keeps the scope open.
	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in 2", true, ""))
	pulled, err = store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "a live running row must still deliver the check-in")
}

// TestPullSessionNotices_WakeOnlyOfArchivedJobDoesNotFollowTheReusedKey is
// R2A-4's wake_only half: a wake_only check-in names its job by tool_call_id
// text. When the job's row is archived by a reused id, the check-in follows the
// row (RepointSessionNoticesJobToolCallID); otherwise the text resolves to the
// NEW running row and a check-in about the finished job is delivered as if it
// were about the new one.
//
// REVERT CHECK: the RepointSessionNoticesJobToolCallID call removed from
// claimOnce -- the old check-in was delivered (pulled had 1 entry) and the
// require.Empty below failed. Restored; re-ran, passed.
func TestPullSessionNotices_WakeOnlyOfArchivedJobDoesNotFollowTheReusedKey(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(store.q)

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "first", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call_0"))
	// The check-in about the FIRST job is queued while it runs...
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindWakeOnly, "first job still running", true, "call_0"))
	// ...the job then finishes and its id is reused before the check-in is pulled.
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call_0", State: "completed", ResultSummary: "done", Wake: true})
	require.NoError(t, err)
	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "second", ToolName: "bash"})
	require.NoError(t, err)

	pulled, err := store.PullSessionNotices(ctx, messages, "owner-1", sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "the check-in about the finished first job must void, not resolve to the new running row")
}
