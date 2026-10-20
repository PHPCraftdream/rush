package agent

// Durable delegation child-link bind (task #1490), agent side: the
// coordinator's lifecycle bind must persist the batch_nodes.child_session_id
// edge BEFORE the child provider is entered, a bind failure must be fatal to
// the launch, and batchAgentExecution.bindChildSession must only update the
// in-memory request after the durable write succeeds.

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchBindAgentFixture creates a test env, a parent session and one live
// batch root (one agent task child) reserved under it. The engine derives
// rootID as batchOutputRoot(nodeID) at launch; fixture node ids are opaque,
// so callers pass Snapshot.Root.RootID directly to the binder.
func batchBindAgentFixture(t *testing.T) (fakeEnv, *session.BatchStore, string, string, string) {
	t.Helper()
	env := testEnv(t)
	store := session.NewBatchStore(env.conn)
	parent, err := env.sessions.Create(t.Context(), "bind-owner")
	require.NoError(t, err)
	res, err := store.Reserve(t.Context(), session.BatchReservation{
		Owner: parent.ID, Host: "host", RunnerID: "runner", ToolCallID: "call-bind", InputHash: "hash", Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: "running", ControlState: "none",
			},
			Children: []session.BatchReservationNode{{
				Values: db.ReserveBatchNodeParams{
					Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
					State: "running", ControlState: "none",
				},
			}},
		},
	})
	require.NoError(t, err)
	return env, store, parent.ID, res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
}

func TestBatchDelegationPersistsChildLinkBeforeChildRuns(t *testing.T) {
	t.Parallel()
	env, store, owner, rootID, taskNodeID := batchBindAgentFixture(t)
	lifecycle := batchAgentLifecycle{
		BindChildSession: func(ctx context.Context, id string) error {
			return store.BatchBindChildSession(ctx, owner, "host", "runner", rootID, taskNodeID, 0, id)
		},
		NoChildSelected: func() { t.Error("a successful bind must not fire NoChildSelected") },
	}

	var entries atomic.Int64
	var observedLink sql.NullString
	stub := newMockAgent("bind-provider", 4096, func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		entries.Add(1)
		// Read the edge FIRST: this is the "already persisted before the
		// child provider was entered" proof.
		err := env.conn.QueryRowContext(ctx,
			"SELECT child_session_id FROM batch_nodes WHERE node_id = ?", taskNodeID).Scan(&observedLink)
		if err != nil {
			return nil, err
		}
		return agentResultWithText("done"), nil
	})
	coord := newTestCoordinator(t, env, "bind-provider", config.ProviderConfig{ID: "bind-provider"})
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	_, err := coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: stub, SessionID: owner, AgentMessageID: "msg-bind", ToolCallID: "call-bind",
		Prompt: "go", SessionTitle: "child",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), entries.Load())
	childID := env.sessions.CreateAgentToolSessionID("msg-bind", "call-bind")
	// MUTATION: a bind that only sets req.ChildSessionID in memory (or that
	// runs AFTER the child starts) leaves the edge NULL when the provider
	// reads it → the Valid/childID assertions below fail.
	require.True(t, observedLink.Valid, "the edge must already be persisted when the child provider is entered")
	require.Equal(t, childID, observedLink.String)

	// The validated-resume path must persist too: same child, same edge.
	observedLink = sql.NullString{}
	_, err = coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: stub, SessionID: owner, ResumeSessionID: childID, Prompt: "continue",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), entries.Load())
	require.True(t, observedLink.Valid, "resume must re-persist the edge before the child turn")
	require.Equal(t, childID, observedLink.String, "resume must keep the SAME child bound")
}

func TestBatchBindFailureIsFatalToLaunch(t *testing.T) {
	t.Parallel()
	env, store, owner, rootID, taskNodeID := batchBindAgentFixture(t)
	injected := errors.New("bind injected")
	var noChild atomic.Int64
	lifecycle := batchAgentLifecycle{
		BindChildSession: func(ctx context.Context, id string) error {
			if err := store.BatchBindChildSession(ctx, owner, "host", "runner", rootID, taskNodeID, 0, id); err != nil {
				return err
			}
			return injected
		},
		NoChildSelected: func() { noChild.Add(1) },
	}

	var entries atomic.Int64
	stub := newMockAgent("bind-provider", 4096, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		entries.Add(1)
		return agentResultWithText("done"), nil
	})
	coord := newTestCoordinator(t, env, "bind-provider", config.ProviderConfig{ID: "bind-provider"})
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	// MUTATION: swallowing the bind error (fire-and-forget bind) would let
	// the child run unbound → the error assertion fails and entries becomes
	// 1; skipping the defer's NoChildSelected marking would leave the node
	// unmarked → the noChild assertion fails.
	_, err := coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: stub, SessionID: owner, AgentMessageID: "msg-bind-fail", ToolCallID: "call-bind-fail",
		Prompt: "go", SessionTitle: "child",
	})
	require.ErrorIs(t, err, injected, "a bind failure must be fatal to the launch")
	require.Zero(t, entries.Load(), "the child provider must never be entered on a failed bind")
	require.Equal(t, int64(1), noChild.Load(), "the deferred NoChildSelected must mark the node")
}

func TestBatchBindChildSessionMemoryFollowsPersist(t *testing.T) {
	t.Parallel()
	env, store, owner, rootID, taskNodeID := batchBindAgentFixture(t)
	ctx := t.Context()
	child := "some-child-id"

	newExecution := func(bind func(context.Context, string, string, string, uint64, string) error) *batchAgentExecution {
		return &batchAgentExecution{
			deps:   batchAgentDeps{Bind: bind, Host: "host", RunnerID: "runner"},
			nodeID: jobbatch.ID(taskNodeID), rootID: jobbatch.ID(rootID),
			req: batchAgentRunRequest{SessionID: owner, NodeID: jobbatch.ID(taskNodeID), Token: 0},
		}
	}

	// Real binder: the memory request follows the durable write.
	_, createErr := env.sessions.CreateTaskSession(ctx, child, owner, "child")
	require.NoError(t, createErr)
	// MUTATION: setting req.ChildSessionID BEFORE the durable write (or on
	// its failure) would expose an unbound child to the driver → the empty
	// snapshot assertion in the failing-binder case below fails.
	e := newExecution(func(ctx context.Context, o, r, n string, token uint64, c string) error {
		return store.BatchBindChildSession(ctx, o, "host", "runner", r, n, token, c)
	})
	require.NoError(t, e.bindChildSession(ctx, child))
	require.Equal(t, child, e.requestSnapshot().ChildSessionID)

	// Failing binder: error returned, memory NOT updated.
	eFail := newExecution(func(context.Context, string, string, string, uint64, string) error {
		return errors.New("bind down")
	})
	require.Error(t, eFail.bindChildSession(ctx, child))
	require.Empty(t, eFail.requestSnapshot().ChildSessionID)

	// Nil Bind: flag-off memory-only behavior, byte-identical, no DB call.
	eMem := newExecution(nil)
	require.NoError(t, eMem.bindChildSession(ctx, child))
	require.Equal(t, child, eMem.requestSnapshot().ChildSessionID)
}
