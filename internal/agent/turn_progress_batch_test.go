// Test-only batch wiring for the in-turn progress guard's held-job fixture
// (turn_progress_fixture_test.go). T5's held job W is launched through the
// retained LEGACY asyncTool, which registers it only in the in-memory ledger;
// under RUSH_BATCH_RUNTIME the guard reads own work from the bound batch
// cutover facts (ownWorkBatch), so the live row has to exist in the batch
// store too. guardBatchTool + guardBatchLedger provide that, seed-only, with
// no production change.
package agent

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// guardBatchLedger records the batch roots guardBatchTool seeded for W, so
// killHeldJob can project them terminal. Not goroutine-safe by accident: the
// tool wrapper runs on a tool goroutine while the test goroutine terminalizes.
type guardBatchLedger struct {
	t     *testing.T
	conn  *sql.DB
	owner string

	mu    sync.Mutex
	roots map[string]string // tool call id -> batch root id ("" while seeding)
}

func newGuardBatchLedger(t *testing.T, conn *sql.DB, owner string) *guardBatchLedger {
	return &guardBatchLedger{t: t, conn: conn, owner: owner, roots: map[string]string{}}
}

// guardBatchTool wraps one asyncTool and, ONLY under RUSH_BATCH_RUNTIME and
// only after the inner call succeeded, records the call as a LIVE batch
// command row for the fixture's owner. Under OFF the embedded tool answers
// every seam untouched, so the wiring is byte-for-byte the bare asyncTool.
type guardBatchTool struct {
	fantasy.AgentTool // the real asyncTool value; never nil
	batch             *guardBatchLedger
}

func (t *guardBatchTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.AgentTool.Run(ctx, call)
	if err == nil && !resp.IsError && batchFlagEnabled() && t.batch != nil {
		t.batch.recordRunning(call.ID)
	}
	return resp, err
}

// recordRunning seeds the live batch row for callID exactly as
// seedBatchDelegation does for a live state (root kind=batch running, child
// task_kind=command running, announced), then remembers the root. It runs on
// the tool goroutine: require/t.Fatal are FORBIDDEN there, so every failure is
// t.Errorf + return. Re-entry (an idempotent retry of the same call id) is a
// no-op.
func (g *guardBatchLedger) recordRunning(callID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.roots[callID]; ok {
		return
	}
	g.roots[callID] = "" // claim before the DB work: no double seed
	root, err := g.seedRunning(callID)
	if err != nil {
		g.t.Errorf("guard batch seed for %s: %v", callID, err)
		return
	}
	g.roots[callID] = root
}

// seedRunning is seedBatchDelegation's Reserve + BatchAnnounceStarted pair
// without require: it returns the error instead of calling FailNow.
func (g *guardBatchLedger) seedRunning(callID string) (string, error) {
	ctx := context.Background()
	store := session.NewBatchStore(g.conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: g.owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	if err != nil {
		return "", err
	}
	root := res.Snapshot.Root.RootID
	if _, err := store.BatchAnnounceStarted(ctx, message.NewService(db.New(g.conn)), g.owner, root, batchFixtureMessage("held job started")); err != nil {
		return "", err
	}
	return root, nil
}

// terminalize projects W's batch root+child to completed through the same
// constraint-valid shape seedBatchJobDebt uses (one Project call at sequence
// 2). It is idempotent and total: no row (W never launched — the t.Cleanup
// path when the turn failed early) and an already-terminal row (killHeldJob
// ran mid-test, and again via t.Cleanup) both return nil without writing.
func (g *guardBatchLedger) terminalize(callID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	root, ok := g.roots[callID]
	if !ok || root == "" {
		return nil
	}
	ctx := context.Background()
	store := session.NewBatchStore(g.conn)
	snap, err := store.Read(ctx, g.owner, root)
	if err != nil {
		return err
	}
	if !batchNodeStateLive(snap.Root.State) {
		return nil // already terminal: LiveWorkSummary no longer counts it
	}
	child := ""
	for _, node := range snap.Nodes {
		if node.NodeID != root {
			child = node.NodeID
		}
	}
	release := func(nodeID string) db.ProjectBatchNodeParams {
		return db.ProjectBatchNodeParams{
			NodeID: nodeID, State: "completed", OutcomeState: "completed",
			OutcomeSummary: "held job released", ControlState: "none",
		}
	}
	updates := []db.ProjectBatchNodeParams{release(root)}
	if child != "" {
		updates = append(updates, release(child))
	}
	return store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: g.owner, Root: root, Updates: updates}},
	})
}
