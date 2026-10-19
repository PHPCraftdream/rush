package server

// F9: dual-mode live-work test seeding. seedLiveWorkRow seeds one live-work
// fact through the flag-appropriate store: flag off, a real AsyncJobStore
// Claim with EXACTLY the inputs the original fixtures used (same Input text,
// same ToolName, same kind); flag on, the same fact as a live batch root via
// the production session.BatchStore.Reserve API (root kind=batch with the
// tool call; one task child when the fact has a task kind and/or a delegation
// child session). projectLiveWorkRoot settles one seeded root through
// BatchStore.Project — the batch-side equivalent of the legacy Transition the
// tests' terminal phases perform.

import (
	"database/sql"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// liveWorkSeedSpec is one live-work fact to seed, in either mode. input and
// toolName carry the ORIGINAL fixture values so the flag-OFF row is
// byte-identical to what the test claimed before the port (Claim hashes Input
// into input_hash). state is the flag-ON root/child state; every current
// call site seeds a running row, and the flag-OFF branch always claims a
// running row.
type liveWorkSeedSpec struct {
	owner          string
	toolCallID     string
	taskKind       string // "" = bare command root (flag ON); drives the flag-OFF Kind
	input          string // the original fixture's Claim Input text
	toolName       string // the original fixture's ToolName ("" where it had none)
	childSessionID string
	state          string
}

// seedLiveWorkRow seeds one live-work fact through the flag-appropriate
// store. OFF: exactly the original Claim. ON: session.BatchStore.Reserve —
// root kind=batch (mode parallel, ControlState none) carrying the tool call,
// plus one task child when the spec has a task kind, carrying the delegation
// child session. The host id is deliberately unregistered: the reader probes
// it Dead, which never settles a node and never marks a read incomplete.
func seedLiveWorkRow(t *testing.T, a *appPkg.App, spec liveWorkSeedSpec) {
	t.Helper()
	ctx := t.Context()
	if batchRuntimeEnabled() {
		children := []session.BatchReservationNode(nil)
		if spec.taskKind != "" {
			children = append(children, session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: spec.taskKind, Valid: true},
				State: spec.state, ControlState: "none",
				ChildSessionID: sql.NullString{String: spec.childSessionID, Valid: spec.childSessionID != ""},
			}})
		}
		_, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
			Owner: spec.owner, Host: "batch-test-host", RunnerID: "lw-seed-runner",
			ToolCallID: spec.toolCallID, InputHash: "hash-" + spec.toolCallID, Sequence: 1,
			Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: spec.state, ControlState: "none",
			}, Children: children},
		})
		require.NoError(t, err)
		return
	}
	kind := session.JobKindCommand
	if spec.taskKind == "agent" || spec.taskKind == "agentic_fetch" {
		kind = session.JobKindAgent
	}
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	_, err := store.Claim(ctx, session.ClaimParams{
		Owner: spec.owner, ToolCallID: spec.toolCallID, Kind: kind,
		Input: spec.input, ToolName: spec.toolName, ChildSessionID: spec.childSessionID,
	})
	require.NoError(t, err)
}

// projectLiveWorkRoot settles one seeded root — every node of it — to the
// given (terminal) state through BatchStore.Project: the batch-side
// equivalent of the legacy store.Transition the tests' terminal phases
// perform on the flag-OFF branch.
func projectLiveWorkRoot(t *testing.T, a *appPkg.App, owner, toolCallID, state string) {
	t.Helper()
	ctx := t.Context()
	q := db.New(a.DB())
	root, err := q.GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{
		OwnerSessionID: owner, ToolCallID: sql.NullString{String: toolCallID, Valid: true},
	})
	require.NoError(t, err)
	nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root.RootID, OwnerSessionID: owner})
	require.NoError(t, err)
	require.NotEmpty(t, nodes, "the live root must have its tree readable")
	var updates []db.ProjectBatchNodeParams
	for _, n := range nodes {
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: n.NodeID, State: state, ControlState: "none"})
	}
	require.NoError(t, session.NewBatchStore(a.DB()).Project(ctx, session.BatchProjection{
		Host: root.HostID, RunnerID: root.RunnerID, Sequence: root.ProjectionSequence + 1,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: root.RootID, Updates: updates}},
	}))
}

func seedLiveWorkTerminal(t *testing.T, a *appPkg.App, spec liveWorkSeedSpec, transition session.TransitionParams) {
	t.Helper()
	ctx := t.Context()
	spec.state = "running"
	seedLiveWorkRow(t, a, spec)
	transition.Owner, transition.ToolCallID = spec.owner, spec.toolCallID
	if !batchRuntimeEnabled() {
		store := a.AsyncJobStore()
		_, err := store.Transition(ctx, transition)
		require.NoError(t, err)
		return
	}
	cause := jobbatch.CauseNone
	switch transition.State {
	case "cancelled":
		cause = jobbatch.CauseStop
	case "timed_out":
		cause = jobbatch.CauseTimeout
	case "failed":
		cause = jobbatch.CauseStopOnFail
	}
	q := db.New(a.DB())
	root, err := q.GetLiveBatchRoot(ctx, db.GetLiveBatchRootParams{
		OwnerSessionID: spec.owner, ToolCallID: sql.NullString{String: spec.toolCallID, Valid: true},
	})
	require.NoError(t, err)
	nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root.RootID, OwnerSessionID: spec.owner})
	require.NoError(t, err)
	var updates []db.ProjectBatchNodeParams
	for _, node := range nodes {
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: node.NodeID, State: transition.State, Cause: string(cause), ControlState: "none"})
	}
	require.NoError(t, session.NewBatchStore(a.DB()).Project(ctx, session.BatchProjection{
		Host: root.HostID, RunnerID: root.RunnerID, Sequence: root.ProjectionSequence + 1,
		Roots: []session.BatchRootProjection{{Owner: spec.owner, Root: root.RootID, Updates: updates}},
	}))
}
