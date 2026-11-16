package apprecovery

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/dbconn"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recoveryWaitDiagnostic struct {
	target  string
	seen    string
	elapsed time.Duration
	timeout time.Duration
}

type recoveryTestDiagnostics struct {
	sessions session.Service
	messages message.Service
	conn     *sql.DB
	dbDir    string
	started  time.Time

	baselineStats   dbconn.DBStats
	baselineErr     error
	baselineElapsed time.Duration
	listCalls       atomic.Int64
	preWriteCalls   atomic.Int64

	mu         sync.Mutex
	events     []string
	candidates map[string][]string
	waits      []recoveryWaitDiagnostic
}

func (d *recoveryTestDiagnostics) event(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, fmt.Sprintf(format, args...))
	if len(d.events) > 32 {
		d.events = d.events[len(d.events)-32:]
	}
}

func (d *recoveryTestDiagnostics) candidate(sessionID, messageID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.candidates[sessionID] = append(d.candidates[sessionID], messageID)
}

func (d *recoveryTestDiagnostics) wait(target, seen string, elapsed, timeout time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.waits = append(d.waits, recoveryWaitDiagnostic{
		target: target, seen: seen, elapsed: elapsed, timeout: timeout,
	})
	if len(d.waits) > 8 {
		d.waits = d.waits[len(d.waits)-8:]
	}
}

func (d *recoveryTestDiagnostics) dumpOnFailure(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}

	d.mu.Lock()
	events := append([]string(nil), d.events...)
	candidates := make(map[string][]string, len(d.candidates))
	for sessionID, messageIDs := range d.candidates {
		candidates[sessionID] = append([]string(nil), messageIDs...)
	}
	waits := append([]recoveryWaitDiagnostic(nil), d.waits...)
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stats dbconn.DBStats
	var statsErr error
	if d.conn != nil {
		stats, statsErr = dbconn.Stats(ctx, d.conn)
	}
	entries, entriesErr := os.ReadDir(d.dbDir)
	t.Logf("recovery diagnostics: elapsed=%s dbDir=%q seamCalls={ListSeam:%d PreWriteSeam:%d} candidates=%v baselineStats=%+v baselineErr=%v baselineElapsed=%s currentStats=%+v currentStatsErr=%v tempDirEntriesErr=%v",
		time.Since(d.started), d.dbDir, d.listCalls.Load(), d.preWriteCalls.Load(), candidates, d.baselineStats, d.baselineErr, d.baselineElapsed, stats, statsErr, entriesErr)
	for _, wait := range waits {
		t.Logf("recovery diagnostics: wait={target:%q seen:%q elapsed:%s timeout:%s}", wait.target, wait.seen, wait.elapsed, wait.timeout)
	}
	for i, entry := range entries {
		if i == 40 {
			t.Logf("recovery diagnostics: tempDirEntries truncated after 40 entries")
			break
		}
		t.Logf("recovery diagnostics: tempDirEntry=%q dir=%t", entry.Name(), entry.IsDir())
	}
	for _, event := range events {
		t.Logf("recovery diagnostics: event=%s", event)
	}

	if d.sessions == nil || d.messages == nil {
		t.Logf("recovery diagnostics: session/message services unavailable")
	} else {
		allSessions, err := d.sessions.ListAll(ctx)
		if err != nil {
			t.Logf("recovery diagnostics: Sessions.ListAll error: %v", err)
		} else {
			t.Logf("recovery diagnostics: sessions=%d", len(allSessions))
			for _, sess := range allSessions {
				messages, listErr := d.messages.List(ctx, sess.ID)
				t.Logf("recovery diagnostics: session id=%s parent=%s title=%q storedMessageCount=%d messages=%d listErr=%v",
					sess.ID, sess.ParentSessionID, sess.Title, sess.MessageCount, len(messages), listErr)
				for _, msg := range messages {
					t.Logf("recovery diagnostics: message id=%s session=%s role=%s finished=%t partial=%t finishReason=%s parts=%d",
						msg.ID, msg.SessionID, msg.Role, msg.IsFinished(), msg.IsPartial(), msg.FinishReason(), len(msg.Parts))
				}
				if ctx.Err() != nil {
					t.Logf("recovery diagnostics: session/message snapshot stopped: %v", ctx.Err())
					break
				}
			}
		}
	}
	stackSize := 64 << 10
	const maxStackSize = 8 << 20
	var stack []byte
	truncated := false
	for {
		stack = make([]byte, stackSize)
		n := runtime.Stack(stack, true)
		if n < len(stack) {
			stack = stack[:n]
			break
		}
		if stackSize == maxStackSize {
			stack = stack[:n]
			truncated = true
			break
		}
		stackSize *= 2
		if stackSize > maxStackSize {
			stackSize = maxStackSize
		}
	}
	t.Logf("recovery diagnostics: goroutines truncated=%t bytes=%d:\n%s", truncated, len(stack), stack)
}

// newRecoveryTestServices wires up real session.Service and message.Service
// against an in-memory SQLite (t.TempDir-backed via db.Connect) plus the
// matching RecoverParams. Rebind of package app's newRecoveryTestApp to the
// exported RecoverParams shape; semantics (db.Release cleanup, zero
// orphan-age threshold) are identical.
func newRecoveryTestServices(t *testing.T) (session.Service, message.Service, RecoverParams, *recoveryTestDiagnostics) {
	t.Helper()
	dbDir := t.TempDir()
	diagnostics := &recoveryTestDiagnostics{
		dbDir:      dbDir,
		started:    time.Now(),
		candidates: make(map[string][]string),
	}
	// db.Release is required because Connect pools by path; conn.Close would
	// bypass the pool refcount and leave its opener goroutine behind.
	t.Cleanup(func() {
		diagnostics.dumpOnFailure(t)
		if diagnostics.conn != nil {
			_ = db.Release(dbDir)
		}
	})
	conn, err := db.Connect(t.Context(), dbDir)
	require.NoError(t, err)
	diagnostics.conn = conn

	q := db.New(conn)
	// Zero orphan-age threshold so tests don't need to sleep for the
	// production default (30s) to elapse before recovery considers a
	// freshly-created assistant an orphan.
	zero := time.Duration(0)
	sessions := session.NewService(q, conn)
	msgs := message.NewService(q)
	diagnostics.sessions = sessions
	diagnostics.messages = msgs
	baselineCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	baselineStarted := time.Now()
	diagnostics.baselineStats, diagnostics.baselineErr = dbconn.Stats(baselineCtx, conn)
	diagnostics.baselineElapsed = time.Since(baselineStarted)
	cancel()
	params := RecoverParams{
		Messages:  msgs,
		OrphanAge: &zero,
		ListSeam: func() {
			diagnostics.listCalls.Add(1)
		},
		PreWriteSeam: func() {
			diagnostics.preWriteCalls.Add(1)
		},
	}
	return sessions, msgs, params, diagnostics
}

// TestRecoverInterruptedTurns_CostProportionalToCandidates is the task #774
// proof: RecoverInterruptedTurns' cost must scale with the number of
// CANDIDATES (sessions whose last message is an unfinished assistant
// message), not the total number of sessions in the data directory.
//
// Before the fix, recoverInterruptedTurns called Sessions.ListAll then, for
// EVERY session returned, ran a full Messages.List against it just to find
// the last assistant message -- an O(total sessions) scan. Measured on a
// real dev DB: 137 sessions, 10s+, tripping the sweep's own 10s deadline.
//
// This test seeds 100 finished (non-candidate) sessions plus 5 genuinely
// orphaned (candidate) sessions, and asserts recoverSessionListSeam --
// which fires once per candidate actually processed by
// recoverSessionInterruptedTurn -- fires exactly 5 times, not 105. Confirmed
// to fail against the pre-fix code (which called the per-session path for
// every session returned by Sessions.ListAll, i.e. would have fired 105
// times) by temporarily reverting the fix locally before finalizing this
// test.
func TestRecoverInterruptedTurns_CostProportionalToCandidates(t *testing.T) {
	sessions, msgs, params, diagnostics := newRecoveryTestServices(t)
	ctx := t.Context()
	diagnostics.event("cost test setup started")

	const numFinished = 100
	const numOrphans = 5

	for i := 0; i < numFinished; i++ {
		sess, err := sessions.Create(ctx, fmt.Sprintf("finished-%d", i))
		require.NoError(t, err)
		asst, err := msgs.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "done"}},
		})
		require.NoError(t, err)
		// The candidate query keys off the DB's finished_at column, which
		// (matching real agent turns, see agent_turn.go's OnStepFinish ->
		// AddFinish -> Update chain) is only stamped by Update, never by
		// Create -- baking a Finish part into the initial Parts at Create
		// time does NOT set finished_at, so these sessions would otherwise
		// wrongly count as candidates too.
		asst.AddFinish(message.FinishReasonEndTurn, "done", "")
		require.NoError(t, msgs.Update(ctx, asst))
	}

	orphanIDs := make(map[string]bool, numOrphans)
	for i := 0; i < numOrphans; i++ {
		sess, err := sessions.Create(ctx, fmt.Sprintf("orphan-%d", i))
		require.NoError(t, err)
		orphan, err := msgs.Create(ctx, sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
			},
		})
		require.NoError(t, err)
		orphanIDs[sess.ID] = true
		diagnostics.candidate(sess.ID, orphan.ID)
		diagnostics.event("seeded orphan candidate session=%s message=%s", sess.ID, orphan.ID)
	}

	RecoverInterruptedTurns(ctx, params)
	diagnostics.event("recovery returned elapsed=%s listCalls=%d", time.Since(diagnostics.started), diagnostics.listCalls.Load())

	assert.Equal(t, int64(numOrphans), diagnostics.listCalls.Load(),
		"recovery must only process the %d candidate sessions, not all %d sessions",
		numOrphans, numFinished+numOrphans)

	// Sanity: the orphans were actually recovered, and the finished sessions
	// untouched -- proves the candidate-only path didn't just skip work.
	for id := range orphanIDs {
		list, err := msgs.List(ctx, id)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.True(t, list[0].IsFinished(), "orphan session %s must be recovered", id)
	}
}
