// BatchResetOwnerHistory coverage (R10c, D-R10c-2): the refusal of any
// nonterminal root incarnation -- pending, starting, running, cancelling
// (including "stopped but awaiting termination") -- is atomic with the wipe:
// the scan and the deletion share one transaction, so a concurrent Reserve
// either lands before the check (refusal, nothing written) or after the
// commit (the new root survives intact). Real SQLite, -race.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const batchResetOwner = "reset-owner"

type batchResetFx struct {
	t        *testing.T
	store    *BatchStore
	other    *BatchStore
	conn     *sql.DB
	dir      string
	ctx      context.Context
	messages message.Service
}

func newBatchResetFx(t *testing.T) *batchResetFx {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	// Migrate and seed through the guarded Connect/Release pair, then open two
	// single-connection stores with a busy timeout, so a Reserve can race the
	// reset's writer lock (batchOutboxTwoStores pattern).
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, db.New(conn), batchResetOwner))
	require.NoError(t, db.ReleaseConn(conn))
	open := func() *sql.DB {
		// Same shape as the production writer DSN (connect.go): BEGIN
		// IMMEDIATE and a busy timeout, so a racing tx waits for the writer
		// lock instead of failing a deferred upgrade with SQLITE_BUSY.
		c, e := sql.Open("sqlite", "file:"+dir+"/rush.db?_txlock=immediate&_pragma=busy_timeout(30000)&_pragma=foreign_keys(ON)")
		require.NoError(t, e)
		c.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		return c
	}
	a := open()
	f := &batchResetFx{t: t, store: NewBatchStore(a), conn: a, dir: dir, ctx: ctx, messages: message.NewService(db.New(a))}
	f.other = NewBatchStore(open())
	return f
}

func (f *batchResetFx) q() *db.Queries { return db.New(f.conn) }

func (f *batchResetFx) addUserMessage(text string) message.Message {
	f.t.Helper()
	m, err := f.messages.Create(f.ctx, batchResetOwner, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}})
	require.NoError(f.t, err)
	return m
}

// batchResetReserve reserves a fresh root incarnation (children = 2 tasks)
// and optionally projects it to state.
func (f *batchResetFx) reserve(state string) BatchSnapshot {
	f.t.Helper()
	p := batchStoreTestReservation()
	p.Owner, p.ToolCallID = batchResetOwner, fmt.Sprintf("call-%d", f.nodes()+1)
	r, err := f.store.Reserve(f.ctx, p)
	require.NoError(f.t, err)
	if state != "" && state != "pending" {
		p2 := BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: batchResetOwner, Root: r.Snapshot.Root.RootID, Updates: []db.ProjectBatchNodeParams{
			batchStoreTestUpdate(r.Snapshot.Root.NodeID, state), batchStoreTestUpdate(r.Snapshot.Nodes[1].NodeID, state), batchStoreTestUpdate(r.Snapshot.Nodes[2].NodeID, state),
		}}}}
		require.NoError(f.t, f.store.Project(f.ctx, p2))
	}
	return r.Snapshot
}

func (f *batchResetFx) nodes() int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.conn.QueryRowContext(f.ctx, "SELECT count(*) FROM batch_nodes").Scan(&n))
	return n
}

func (f *batchResetFx) reset() (BatchResetOutcome, error) {
	return f.store.BatchResetOwnerHistory(f.ctx, f.messages, batchResetOwner)
}

// Revert-checks (a) (b) (c): dropping pending / running / cancelling from the
// nonterminal refusal scan lets the reset wipe a session with a root in that
// state -- the batch_nodes count assertion below fails.
func TestBatchResetRefusedForEachLiveRootState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending", "starting", "running", "cancelling"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			f := newBatchResetFx(t)
			snap := f.reserve(state)
			if state == "cancelling" {
				// "Stopped but awaiting termination": a pending stop request
				// against the cancelling projection must refuse too (I8).
				_, err := f.store.CreateControl(f.ctx, batchResetOwner, "host", snap.Root.RootID, snap.Nodes[1].NodeID)
				require.NoError(t, err)
			}
			msg := f.addUserMessage("history")

			_, err := f.reset()
			require.ErrorIs(t, err, ErrBatchResetRefused)
			var refusal *BatchResetRefusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, batchResetOwner, refusal.Owner)
			require.Equal(t, snap.Root.RootID, refusal.RootID)
			require.Equal(t, state, refusal.State)
			require.Equal(t, snap.Root.ToolCallID.String, refusal.ToolCallID)

			require.Positive(t, f.nodes(), "a refused reset deletes no batch rows")
			left, err := f.messages.List(f.ctx, batchResetOwner)
			require.NoError(t, err)
			require.Len(t, left, 1, "a refused reset keeps the history")
			require.Equal(t, msg.ID, left[0].ID)
			if state == "cancelling" {
				controls, err := f.store.PendingControls(f.ctx, batchResetOwner, "host", snap.Root.RootID)
				require.NoError(t, err)
				require.Len(t, controls, 1, "a refused reset keeps the stop request")
			}
		})
	}
}

// Revert-check: letting a terminal root block the reset (dropping the
// terminal filter) fails the NoError below.
func TestBatchResetAllowedWhenAllRootsTerminal(t *testing.T) {
	t.Parallel()
	f := newBatchResetFx(t)
	f.reserve("completed")
	f.addUserMessage("history")
	// A second, older terminal incarnation must not block either.
	f.reserve("completed")

	out, err := f.reset()
	require.NoError(t, err)
	require.Equal(t, 1, out.MessagesDeleted)
	require.EqualValues(t, 2, out.NodesDeleted) // the two root rows; children cascade
	require.Zero(t, f.nodes())
	left, err := f.messages.List(f.ctx, batchResetOwner)
	require.NoError(t, err)
	require.Empty(t, left)
}

// Revert-check (d): running the refusal scan in its own transaction (before
// the wipe's BEGIN) lets a concurrent Reserve interleave -- the race test
// then observes a root deleted by a reset that scanned BEFORE it existed and
// never refused: a half state. Run with -race.
func TestBatchResetVsConcurrentReserveRace(t *testing.T) {
	f := newBatchResetFx(t) // not parallel: shares one database across iterations
	storeB := f.other

	const iterations = 100
	clean := func() {
		_, err := f.conn.ExecContext(f.ctx, "DELETE FROM batch_nodes")
		require.NoError(t, err)
		_, err = f.conn.ExecContext(f.ctx, "DELETE FROM messages WHERE session_id=?", batchResetOwner)
		require.NoError(t, err)
	}
	locked := func(e error) bool { return e != nil && strings.Contains(e.Error(), "database is locked") }
	for i := 0; i < iterations; i++ {
		for attempt := 0; ; attempt++ {
			f.reserve("completed")
			f.addUserMessage(fmt.Sprintf("iteration-%d", i))

			var wg sync.WaitGroup
			var resetErr, reserveErr error
			var reserved BatchReservationResult
			wg.Add(1)
			go func() { defer wg.Done(); _, resetErr = f.reset() }()
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := batchStoreTestReservation()
				p.Owner, p.ToolCallID = batchResetOwner, "race-root"
				reserved, reserveErr = storeB.Reserve(f.ctx, p)
			}()
			wg.Wait()

			// A BEGIN-DEFERRED upgrade can lose to the other writer at the
			// SQLite layer (SQLITE_BUSY is returned without honoring the
			// busy timeout). That is transport contention, not a modeled
			// outcome: seed again and rerun the iteration.
			if locked(resetErr) || locked(reserveErr) {
				require.Less(t, attempt, 25, "iteration %d: repeated lock contention (reset=%v reserve=%v)", i, resetErr, reserveErr)
				clean()
				continue
			}
			require.NoError(t, reserveErr)
			require.False(t, reserved.Attached)
			// Invariant: whatever the order, the racing root is either refused
			// against or created after the committed wipe -- never a half state.
			got, readErr := storeB.Read(f.ctx, batchResetOwner, reserved.Snapshot.Root.RootID)
			require.NoError(t, readErr, "iteration %d: the racing root must survive", i)
			require.Equal(t, "pending", got.Root.State, "iteration %d", i)
			require.Len(t, got.Nodes, 3, "iteration %d: the whole subtree survives", i)
			// Consistency: a committed reset wiped the history; a refusal kept it.
			left, listErr := f.messages.List(f.ctx, batchResetOwner)
			require.NoError(t, listErr)
			if resetErr == nil {
				require.Empty(t, left, "iteration %d: committed reset must wipe the history", i)
			} else {
				require.ErrorIs(t, resetErr, ErrBatchResetRefused, "iteration %d", i)
				require.NotEmpty(t, left, "iteration %d: a refused reset keeps the history", i)
			}
			break
		}
		// Reset for the next iteration.
		clean()
	}
}
