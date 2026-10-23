package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// failingCreateTxMessages wraps a real message.Service and makes CreateTx
// always fail, so a notice insert error can be injected.
type failingCreateTxMessages struct {
	message.Service
}

var errSimulatedCreateTxFailure = errors.New("simulated message insert failure")

func (f *failingCreateTxMessages) CreateTx(context.Context, *sql.Tx, string, message.CreateMessageParams) (message.Message, error) {
	return message.Message{}, errSimulatedCreateTxFailure
}

func batchOutboxReserve(t *testing.T, s *BatchStore, ctx context.Context, children int) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.Root.Children = nil
	for i := 0; i < children; i++ {
		p.Root.Children = append(p.Root.Children, batchStoreTestTask())
	}
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

func batchOutboxParams(row BatchNoticeRow) message.CreateMessageParams {
	return batchOutboxText(row.NodeID)
}

func batchOutboxText(text string) message.CreateMessageParams {
	return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}, BackgroundJobNotice: true}
}

func batchOutboxSettle(t *testing.T, s *BatchStore, ctx context.Context, snap BatchSnapshot, seq int64, ids ...string) {
	t.Helper()
	p := BatchRootProjection{Owner: "owner", Root: snap.Root.RootID}
	for i, id := range ids {
		v := batchStoreTestUpdate(id, "completed")
		v.OutcomeSummary = id
		p.Updates = append(p.Updates, v)
		n := batchStoreTestNotice(id)
		n.NoticeOrdinal = sql.NullInt64{Int64: int64(i), Valid: true}
		if id == snap.Root.RootID {
			n.NoticeKind = batchStoreTestString("root")
		}
		p.Notices = append(p.Notices, n)
	}
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: seq, Roots: []BatchRootProjection{p}}))
}

func batchOutboxAck(t *testing.T, s *BatchStore, ctx context.Context, m message.Service, root string) {
	t.Helper()
	msg, err := s.BatchAnnounceStarted(ctx, m, "owner", root, batchOutboxText("started"))
	require.NoError(t, err)
	persisted, err := s.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, msg.ID, persisted.Root.AnnounceMessageID.String, "ack names actual persisted history")
	history, err := m.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.Equal(t, "started", history.FullText())
}

func batchOutboxTwoStores(t *testing.T) (*BatchStore, *BatchStore, context.Context) {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	require.NoError(t, db.ReleaseConn(conn))
	open := func() *BatchStore {
		c, e := sql.Open("sqlite", storepath.DataDirPath(dir, storepath.ActiveFilename)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		require.NoError(t, e)
		c.SetMaxOpenConns(1)
		t.Cleanup(func() { require.NoError(t, c.Close()) })
		return NewBatchStore(c)
	}
	return open(), open(), ctx
}

// A deliberately shared stale candidate forces the database election, not the
// candidate list, to arbitrate each contender. rowid proves insertion order.
func TestBatchOutboxConcurrentPullExactlyOnceAndOrder(t *testing.T) {
	a, b, ctx := batchOutboxTwoStores(t)
	snap := batchOutboxReserve(t, a, ctx, 2)
	ma, mb := message.NewService(db.New(a.writer)), message.NewService(db.New(b.writer))
	batchOutboxAck(t, a, ctx, ma, snap.Root.RootID)
	batchOutboxSettle(t, a, ctx, snap, 2, snap.Nodes[2].NodeID)
	batchOutboxSettle(t, a, ctx, snap, 3, snap.Nodes[1].NodeID, snap.Root.RootID)
	// A later ordinal cannot overtake the earliest pending sequence.
	_, ok, err := b.batchPullOneNotice(ctx, mb, "owner", snap.Root.RootID, snap.Root.RootID, batchOutboxParams)
	require.NoError(t, err)
	require.False(t, ok)
	expected := []string{snap.Nodes[2].NodeID, snap.Nodes[1].NodeID, snap.Root.RootID}
	for index, id := range expected {
		if index == 1 {
			_, ok, err := b.batchPullOneNotice(ctx, mb, "owner", snap.Root.RootID, snap.Root.RootID, batchOutboxParams)
			require.NoError(t, err)
			require.False(t, ok, "later ordinal must not overtake within the same sequence")
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		wins := make(chan bool, 2)
		var wg sync.WaitGroup
		for _, pair := range []struct {
			s *BatchStore
			m message.Service
		}{{a, ma}, {b, mb}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, ok, e := pair.s.batchPullOneNotice(ctx, pair.m, "owner", snap.Root.RootID, id, batchOutboxParams)
				errs <- e
				wins <- ok
			}()
		}
		close(start)
		wg.Wait()
		count := 0
		for i := 0; i < 2; i++ {
			require.NoError(t, <-errs)
			if <-wins {
				count++
			}
		}
		require.Equal(t, 1, count)
	}
	rows, err := a.writer.QueryContext(ctx, "SELECT id FROM messages WHERE session_id='owner' ORDER BY rowid")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, ids, 4)
	for i, id := range ids[1:] {
		m, e := ma.Get(ctx, id)
		require.NoError(t, e)
		require.Equal(t, expected[i], m.FullText())
	}
}

func TestBatchOutboxFailedInsertRetryAndOrderBarrier(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 2)
	m := message.NewService(db.New(s.writer))
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Nodes[1].NodeID, snap.Nodes[2].NodeID, snap.Root.RootID)
	failing := &failingCreateTxMessages{Service: m}
	out, err := s.BatchPullNotices(ctx, failing, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Empty(t, out)
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	for _, n := range got.Nodes {
		require.Equal(t, "pending", n.Delivery)
		require.False(t, n.NoticeMessageID.Valid)
	}
	out, err = s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, out, 3)
	again, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Empty(t, again)
}

func TestBatchOutboxDelayedAckTwoLeavesAndTransactionalGate(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 2)
	m := message.NewService(db.New(s.writer))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Nodes[1].NodeID)
	batchOutboxSettle(t, s, ctx, snap, 3, snap.Nodes[2].NodeID, snap.Root.RootID)
	// Bypass listing to prove the root gate is in the transactional CAS.
	for _, n := range snap.Nodes {
		_, ok, err := s.batchPullOneNotice(ctx, m, "owner", snap.Root.RootID, n.NodeID, batchOutboxParams)
		require.NoError(t, err)
		require.False(t, ok)
	}
	debt, err := s.BatchReactionDebtExists(ctx, "owner")
	require.NoError(t, err)
	require.False(t, debt)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	out, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, out, 3)
	require.Equal(t, snap.Nodes[1].NodeID, out[0].NodeID)
	require.Equal(t, snap.Nodes[2].NodeID, out[1].NodeID)
}

func TestBatchOutboxFailedAckRetainsControllableReservation(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(s.writer))
	_, err := s.BatchAnnounceStarted(ctx, &failingCreateTxMessages{Service: m}, "owner", snap.Root.RootID, batchOutboxText("started"))
	require.Error(t, err)
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, snap, got)
	again, err := s.Reserve(ctx, func() BatchReservation {
		p := batchStoreTestReservation()
		p.Root.Children = p.Root.Children[:1]
		return p
	}())
	require.NoError(t, err)
	require.True(t, again.Attached)
	require.Equal(t, snap.Root.RootID, again.Snapshot.Root.RootID)
	_, err = s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	require.EqualValues(t, 0, countMessages(t, db.New(s.writer), ctx, "owner"))
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	_, err = s.BatchAnnounceStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("duplicate"))
	require.ErrorIs(t, err, ErrBatchAnnouncementLost)
	require.EqualValues(t, 1, countMessages(t, db.New(s.writer), ctx, "owner"))
}

func TestBatchOutboxInlineAckPullRaceNoDuplicate(t *testing.T) {
	for i := 0; i < 4; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			a, b, ctx := batchOutboxTwoStores(t)
			snap := batchOutboxReserve(t, a, ctx, 1)
			batchOutboxSettle(t, a, ctx, snap, 2, snap.Root.RootID)
			ma, mb := message.NewService(db.New(a.writer)), message.NewService(db.New(b.writer))
			start := make(chan struct{})
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, e := a.BatchAnnounceInlineResult(ctx, ma, "owner", snap.Root.RootID, batchOutboxText("inline"))
				errs <- e
			}()
			go func() {
				defer wg.Done()
				<-start
				_, e := b.BatchAnnounceStarted(ctx, mb, "owner", snap.Root.RootID, batchOutboxText("started"))
				if e == nil {
					_, e = b.BatchPullNotices(ctx, mb, "owner", batchOutboxParams)
				}
				errs <- e
			}()
			close(start)
			wg.Wait()
			wins := 0
			for j := 0; j < 2; j++ {
				e := <-errs
				if e == nil {
					wins++
				} else {
					require.ErrorIs(t, e, ErrBatchAnnouncementLost)
				}
			}
			require.Equal(t, 1, wins)
			got, e := a.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, e)
			require.Equal(t, "done", got.Root.Delivery)
			msgs, e := ma.List(ctx, "owner")
			require.NoError(t, e)
			if got.Root.Wake == 0 {
				require.Len(t, msgs, 1)
				require.Equal(t, got.Root.AnnounceMessageID, got.Root.NoticeMessageID)
			} else {
				require.Len(t, msgs, 2)
			}
			out, e := a.BatchPullNotices(ctx, ma, "owner", batchOutboxParams)
			require.NoError(t, e)
			require.Empty(t, out)
			_, e = a.BatchAnnounceInlineResult(ctx, ma, "owner", snap.Root.RootID, batchOutboxText("duplicate"))
			require.ErrorIs(t, e, ErrBatchAnnouncementLost)
		})
	}
}

func TestBatchOutboxInlineEligibilityAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children int
		settle   bool
		selected bool
	}{{"live-stop-intent", 1, false, false}, {"unselected", 1, true, false}, {"multi-item", 2, true, true}, {"one-item", 1, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchOutboxReserve(t, s, ctx, tc.children)
			m := message.NewService(db.New(conn))
			if tc.selected {
				batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
			} else if tc.settle {
				require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.RootID, "completed")}}}}))
			}
			_, err := s.BatchAnnounceInlineResult(ctx, &failingCreateTxMessages{Service: m}, "owner", snap.Root.RootID, batchOutboxText("result"))
			require.Error(t, err)
			got, e := s.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, e)
			require.Zero(t, got.Root.Announced)
			require.False(t, got.Root.NoticeMessageID.Valid)
			require.EqualValues(t, 0, countMessages(t, db.New(conn), ctx, "owner"))
			ann, err := s.BatchAnnounceInlineResult(ctx, m, "owner", snap.Root.RootID, batchOutboxText("result"))
			if tc.name == "one-item" {
				require.NoError(t, err)
				require.EqualValues(t, 1, ann.DeliveryRows)
				persisted, e := s.Read(ctx, "owner", snap.Root.RootID)
				require.NoError(t, e)
				require.Equal(t, "done", persisted.Root.Delivery, "inline consumes completion")
				require.Zero(t, persisted.Root.Wake)
				require.EqualValues(t, 1, persisted.Root.Reacted)
				require.Equal(t, ann.Msg.ID, persisted.Root.AnnounceMessageID.String)
				require.Equal(t, ann.Msg.ID, persisted.Root.NoticeMessageID.String)
				history, e := m.Get(ctx, ann.Msg.ID)
				require.NoError(t, e)
				require.Equal(t, "result", history.FullText())
				out, e := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
				require.NoError(t, e)
				require.Empty(t, out, "inline cannot produce a later completion turn")
				debt, e := s.BatchReactionDebtExists(ctx, "owner")
				require.NoError(t, e)
				require.False(t, debt)
				require.EqualValues(t, 1, countMessages(t, db.New(conn), ctx, "owner"))
			} else {
				require.ErrorIs(t, err, ErrBatchAnnouncementLost)
			}
		})
	}
}

// All candidates contend concurrently in reverse order. Each loser retries
// the same identity; insertion order must follow sequence/ordinal, not launch.
func TestBatchOutboxConcurrentReverseCandidatesPreserveOrder(t *testing.T) {
	a, b, ctx := batchOutboxTwoStores(t)
	snap := batchOutboxReserve(t, a, ctx, 2)
	ma, mb := message.NewService(db.New(a.writer)), message.NewService(db.New(b.writer))
	batchOutboxAck(t, a, ctx, ma, snap.Root.RootID)
	batchOutboxSettle(t, a, ctx, snap, 2, snap.Nodes[2].NodeID)
	batchOutboxSettle(t, a, ctx, snap, 3, snap.Nodes[1].NodeID, snap.Root.RootID)
	expected := []string{snap.Nodes[2].NodeID, snap.Nodes[1].NodeID, snap.Root.RootID}
	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for i := len(expected) - 1; i >= 0; i-- {
		id := expected[i]
		s, m := a, ma
		if i%2 == 0 {
			s, m = b, mb
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				_, ok, e := s.batchPullOneNotice(ctx, m, "owner", snap.Root.RootID, id, batchOutboxParams)
				if e != nil {
					errs <- e
					return
				}
				if ok {
					errs <- nil
					return
				}
				runtime.Gosched()
			}
			errs <- fmt.Errorf("delivery did not progress: %s", id)
		}()
	}
	close(start)
	wg.Wait()
	for range expected {
		require.NoError(t, <-errs)
	}
	rows, err := a.writer.QueryContext(ctx, "SELECT id FROM messages WHERE session_id='owner' ORDER BY rowid")
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, ids, 4)
	for i, id := range ids[1:] {
		msg, e := ma.Get(ctx, id)
		require.NoError(t, e)
		require.Equal(t, expected[i], msg.FullText())
	}
}

func TestBatchOutboxLateIdentityFailureRollsBackHistory(t *testing.T) {
	for _, operation := range []string{"pull", "ack", "inline"} {
		t.Run(operation, func(t *testing.T) {
			s, conn, ctx := batchStoreTestDB(t)
			snap := batchOutboxReserve(t, s, ctx, 1)
			m := message.NewService(db.New(conn))
			batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
			if operation == "pull" {
				batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
			}
			column := "notice_message_id"
			if operation == "ack" {
				column = "announce_message_id"
			}
			_, err := conn.ExecContext(ctx, "CREATE TRIGGER batch_late_identity_failure BEFORE UPDATE OF "+column+" ON batch_nodes WHEN NEW."+column+" IS NOT NULL AND NEW."+column+" != 'reserved' BEGIN SELECT RAISE(ABORT,'identity failed'); END")
			require.NoError(t, err)
			before, err := s.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, err)
			count := countMessages(t, db.New(conn), ctx, "owner")
			switch operation {
			case "pull":
				_, _, err = s.batchPullOneNotice(ctx, m, "owner", snap.Root.RootID, snap.Root.RootID, batchOutboxParams)
			case "ack":
				_, err = s.BatchAnnounceStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("started"))
			case "inline":
				_, err = s.BatchAnnounceInlineResult(ctx, m, "owner", snap.Root.RootID, batchOutboxText("inline"))
			}
			require.ErrorContains(t, err, "identity failed")
			after, err := s.Read(ctx, "owner", snap.Root.RootID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, count, countMessages(t, db.New(conn), ctx, "owner"))
		})
	}
}

func TestBatchOutboxElisionAndOwnerCascade(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchOutboxReserve(t, s, ctx, 1)
	m := message.NewService(db.New(conn))
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Root.RootID)
	batchOutboxAck(t, s, ctx, m, snap.Root.RootID)
	out, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, snap.Root.RootID, out[0].NodeID)
	got, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Zero(t, got.Nodes[1].NoticeSelected)
	require.Equal(t, "none", got.Nodes[1].Delivery)
	debt, err := s.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, []BatchDebtNodeRef{{RootID: snap.Root.RootID, NodeID: snap.Root.RootID, NoticeMessageID: out[0].Message.ID}}, debt.Nodes, "selected root alone creates debt; elided child excluded")
	visible, err := s.BatchReactionCaptureDebtSnapshot(ctx, "owner")
	require.NoError(t, err)
	require.Equal(t, debt.Nodes, visible.Nodes)
	_, err = s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "DELETE FROM sessions WHERE id='owner'")
	require.NoError(t, err)
	for _, table := range []string{"batch_nodes", "batch_control_requests", "messages"} {
		var n int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		require.Zero(t, n)
	}
}
