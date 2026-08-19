// Narrow-update concurrency guards: rename racing usage/todos updates, the
// broad read-modify-write overwrite those narrow methods fix, and title
// generation racing the main turn's token and cost counters.
package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentRenameAndUsage_NoDataLoss is the regression guard for the
// "broad Save clobbers concurrent edits" data-loss bug (#128). A rename
// (title column) running concurrently with a usage update (token columns)
// must leave BOTH changes visible, because the narrow Rename / SetUsage
// methods touch disjoint columns. Under the old single Save — which
// re-wrote every column from a stale snapshot — the loser's change was
// silently overwritten (last-writer-wins).
func TestConcurrentRenameAndUsage_NoDataLoss(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	// Model production exactly: the real pool is single-connection
	// (db/connect.go SetMaxOpenConns(1)), so SQL ops serialize and the race
	// is Go-level interleaving across goroutines sharing that one conn. A
	// plain ":memory:" is also per-connection, so pinning the pool to 1
	// keeps every goroutine on the same in-memory schema.
	sqlDB.SetMaxOpenConns(1)
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const iterations = 200
	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "init")
		require.NoError(t, err)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			// Errors are collected on a buffered channel rather than
			// asserted with require.NoError inside the goroutines
			// themselves: testing.T's FailNow/Fatal (which require.NoError
			// calls on failure) is documented to be safe only from the
			// test's own goroutine — calling it from a goroutine spawned by
			// the test is undefined behavior. So each goroutine only sends
			// its error (nil on success) and every assertion happens back
			// in the main test goroutine below.
			errCh = make(chan error, 2)
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.Rename(ctx, sess.ID, "renamed-title")
		}()
		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.SetUsage(ctx, sess.ID, 111, 222)
		}()
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			require.NoError(t, err)
		}

		got, err := svc.Get(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed-title", got.Title, "iter %d: rename lost — title clobbered by concurrent usage write", i)
		assert.Equal(t, int64(111), got.PromptTokens, "iter %d: usage lost — tokens clobbered by concurrent rename", i)
		assert.Equal(t, int64(222), got.CompletionTokens, "iter %d: completion tokens lost", i)
	}
}

// TestConcurrentRenameAndTodos_NoDataLoss extends the guard to the todos
// column: a rename (title) and a todos edit racing must both survive.
func TestConcurrentRenameAndTodos_NoDataLoss(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const iterations = 200
	newTodos := []Todo{{Content: "ship it", Status: TodoStatusInProgress, ActiveForm: "shipping"}}
	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "init")
		require.NoError(t, err)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			// See TestConcurrentRenameAndUsage_NoDataLoss for why errors are
			// collected on a channel and asserted only in the main test
			// goroutine, rather than calling require.NoError from inside
			// the spawned goroutines directly.
			errCh = make(chan error, 2)
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.Rename(ctx, sess.ID, "renamed")
		}()
		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.SetTodos(ctx, sess.ID, newTodos, nil)
		}()
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			require.NoError(t, err)
		}

		got, err := svc.Get(ctx, sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Title, "iter %d: title lost", i)
		require.Len(t, got.Todos, 1, "iter %d: todos lost", i)
		assert.Equal(t, "ship it", got.Todos[0].Content)
	}
}

// TestBroadOverwriteLosesConcurrentUsage deterministically reproduces the
// EXACT mechanism the narrow methods fix. It simulates the deleted Save /
// old handleRenameSession behaviour — read a full snapshot, mutate only the
// title, then write ALL columns back from that now-stale snapshot — and
// forces the losing interleaving with channels so it is not flaky. The
// concurrent SetUsage lands between the read and the write, so the stale
// snapshot's prompt_tokens=0 clobbers the 111. This documents WHY Save was
// removed: any caller that re-writes unrelated columns from a snapshot can
// lose a concurrent writer's update to those columns.
func TestBroadOverwriteLosesConcurrentUsage(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	sess, err := svc.Create(ctx, "init")
	require.NoError(t, err)

	// Reset to a known state, then force the losing interleaving.
	require.NoError(t, svc.SetUsage(ctx, sess.ID, 0, 0))
	require.NoError(t, svc.Rename(ctx, sess.ID, "init"))

	readDone := make(chan struct{})
	usageDone := make(chan struct{})
	var (
		wg       sync.WaitGroup
		broadErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 1. Read a full snapshot (prompt_tokens still 0 here).
		var (
			sbTitle, sbSummary, sbTodos, sbDeleted string
			sbPrompt, sbCompletion                 int64
		)
		rerr := sqlDB.QueryRowContext(ctx,
			`SELECT title, prompt_tokens, completion_tokens,
			        COALESCE(summary_message_id, ''), COALESCE(todos, ''), deleted_todos
			 FROM sessions WHERE id = ?`,
			sess.ID,
		).Scan(&sbTitle, &sbPrompt, &sbCompletion, &sbSummary, &sbTodos, &sbDeleted)
		if rerr != nil {
			broadErr = rerr
			close(readDone)
			return
		}
		close(readDone) // snapshot captured with prompt_tokens=0
		<-usageDone     // wait for the concurrent usage update to land (now 111)
		// 3. Write ALL columns back from the stale snapshot → clobbers tokens.
		_, broadErr = sqlDB.ExecContext(ctx,
			`UPDATE sessions SET title=?, prompt_tokens=?, completion_tokens=?,
			        summary_message_id=?, todos=?, deleted_todos=?, updated_at=strftime('%s','now')
			 WHERE id=?`,
			"renamed", sbPrompt, sbCompletion, sbSummary, sbTodos, sbDeleted, sess.ID,
		)
	}()

	<-readDone
	// 2. Usage update lands AFTER the snapshot read, BEFORE the write.
	require.NoError(t, svc.SetUsage(ctx, sess.ID, 111, 222))
	close(usageDone)
	wg.Wait()
	require.NoError(t, broadErr)

	got, err := svc.Get(ctx, sess.ID)
	require.NoError(t, err)
	// The broad-overwrite renamed the title but stomped prompt_tokens back
	// to the stale 0 — exactly the data loss the narrow methods prevent.
	assert.Equal(t, "renamed", got.Title, "title should have been renamed")
	assert.Equal(t, int64(0), got.PromptTokens,
		"broad read-modify-write overwriting all columns MUST have clobbered the concurrent usage update — if this is 111 the test is no longer reproducing the old bug")
}

// TestTitleGenerationDoesNotRaceMainTurnTokens is the regression guard for
// P1.6: the title-generation goroutine (agent.go's generateTitle) used to
// call the now-removed UpdateTitleAndUsage, which additively bumped
// prompt_tokens/completion_tokens on top of whatever the main turn's
// SetUsage snapshot last wrote. Since title generation runs concurrently
// with the main turn (see agent.go Run's wg.Go around the generateTitle
// call), the final token counters depended on which of the two finished
// last — nondeterministic. The fix: title generation now calls Rename
// (title only) + IncrementCost (cost only), leaving prompt_tokens/
// completion_tokens exclusively owned by SetUsage's overwrite semantics.
//
// This test drives both orderings explicitly (title-finishes-first and
// title-finishes-last) and asserts that in BOTH cases:
//   - prompt_tokens/completion_tokens end up exactly what SetUsage wrote
//     (title generation must never contribute to them), and
//   - cost ends up as the SUM of the main turn's cost and the title
//     generation's cost (both contributions must land, regardless of order).
func TestTitleGenerationDoesNotRaceMainTurnTokens(t *testing.T) {
	const (
		mainPromptTokens     = int64(1000)
		mainCompletionTokens = int64(200)
		mainCost             = 0.05
		titleCost            = 0.001
	)

	runOrdering := func(t *testing.T, titleFirst bool) {
		sqlDB, q := newTestDB(t)
		svc := NewService(q, sqlDB)
		ctx := t.Context()

		sess, err := svc.Create(ctx, "Untitled Session")
		require.NoError(t, err)
		sessionID := sess.ID

		mainTurn := func() {
			require.NoError(t, svc.SetUsage(ctx, sessionID, mainPromptTokens, mainCompletionTokens))
			_, err := svc.IncrementCost(ctx, sessionID, mainCost)
			require.NoError(t, err)
		}
		titleGen := func() {
			require.NoError(t, svc.Rename(ctx, sessionID, "Generated Title"))
			_, err := svc.IncrementCost(ctx, sessionID, titleCost)
			require.NoError(t, err)
		}

		if titleFirst {
			titleGen()
			mainTurn()
		} else {
			mainTurn()
			titleGen()
		}

		final, err := svc.Get(ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, mainPromptTokens, final.PromptTokens,
			"prompt_tokens must equal exactly what SetUsage wrote, unaffected by title generation")
		assert.Equal(t, mainCompletionTokens, final.CompletionTokens,
			"completion_tokens must equal exactly what SetUsage wrote, unaffected by title generation")
		assert.InDelta(t, mainCost+titleCost, final.Cost, 1e-9,
			"cost must include both the main turn's and title generation's contributions")
		assert.Equal(t, "Generated Title", final.Title)
	}

	t.Run("title finishes first", func(t *testing.T) {
		runOrdering(t, true)
	})
	t.Run("title finishes last", func(t *testing.T) {
		runOrdering(t, false)
	})
}

// TestTitleGenerationConcurrentWithMainTurn actually races the two goroutines
// (rather than serializing them in a fixed order) using a start barrier so
// both begin as close to simultaneously as possible, repeated across many
// sessions. Run with -race to catch any data race, and verifies the same
// invariants as TestTitleGenerationDoesNotRaceMainTurnTokens hold regardless
// of true scheduling order.
func TestTitleGenerationConcurrentWithMainTurn(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sqlDB, q := newTestDB(t)
	sqlDB.SetMaxOpenConns(1) // production-faithful single-connection pool; see TestConcurrentRenameAndUsage_NoDataLoss.
	svc := NewService(q, sqlDB)
	ctx := t.Context()

	const (
		mainPromptTokens     = int64(4321)
		mainCompletionTokens = int64(987)
		mainCost             = 0.02
		titleCost            = 0.0005
		iterations           = 50
	)

	for i := 0; i < iterations; i++ {
		sess, err := svc.Create(ctx, "Untitled Session")
		require.NoError(t, err)
		sessionID := sess.ID

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			// Errors are collected on a buffered channel rather than
			// asserted with require.NoError inside the goroutines
			// themselves; see TestConcurrentRenameAndUsage_NoDataLoss for
			// why calling require.NoError from a spawned goroutine (not the
			// test's own) is undefined behavior.
			errCh = make(chan error, 4)
		)
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.SetUsage(ctx, sessionID, mainPromptTokens, mainCompletionTokens)
			_, err := svc.IncrementCost(ctx, sessionID, mainCost)
			errCh <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			errCh <- svc.Rename(ctx, sessionID, "Generated Title")
			_, err := svc.IncrementCost(ctx, sessionID, titleCost)
			errCh <- err
		}()

		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			require.NoError(t, err)
		}

		final, err := svc.Get(ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, mainPromptTokens, final.PromptTokens, "iter %d", i)
		assert.Equal(t, mainCompletionTokens, final.CompletionTokens, "iter %d", i)
		assert.InDelta(t, mainCost+titleCost, final.Cost, 1e-9, "iter %d", i)
		assert.Equal(t, "Generated Title", final.Title, "iter %d", i)
	}
}
