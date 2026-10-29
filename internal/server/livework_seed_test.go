package server

// Batch-only live-work test seeding. seedLiveWorkRow seeds one live-work
// fact as a live batch root via the production session.BatchStore.Reserve
// API (root kind=batch with the tool call; one task child when the fact has
// a task kind and/or a delegation child session). projectLiveWorkRoot
// settles one seeded root through BatchStore.Project — every node of it to
// the given (terminal) state; seedLiveWorkTerminal is the same settle for a
// liveWorkSeedSpec, mapping the terminal state to the batch cause.

import (
	"database/sql"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// liveWorkSeedSpec is one live-work fact to seed. taskKind "" is a bare
// command root; every current call site seeds a running row.
type liveWorkSeedSpec struct {
	owner          string
	toolCallID     string
	taskKind       string // "" = bare command root
	childSessionID string
	state          string
	// payloadInput, when set, is stored as the task node's payload exactly like
	// a direct tool call stores its input ({"input": <payloadInput>}).
	payloadInput string
	// inputHash overrides the default "hash-<toolCallID>" (a re-claimed id with
	// different input has a different hash).
	inputHash string
}

// seedLiveWorkRow seeds one live-work fact as a live batch root:
// session.BatchStore.Reserve — root kind=batch (mode parallel, ControlState
// none) carrying the tool call, plus one task child when the spec has a task
// kind, carrying the delegation child session. The host id is deliberately
// unregistered: the reader probes it Dead, which never settles a node and
// never marks a read incomplete (dead hosts stay live until recovery
// commits).
func seedLiveWorkRow(t *testing.T, a *appPkg.App, spec liveWorkSeedSpec) {
	t.Helper()
	ctx := t.Context()
	children := []session.BatchReservationNode(nil)
	if spec.taskKind != "" {
		task := db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: spec.taskKind, Valid: true},
			State: spec.state, ControlState: "none",
			ChildSessionID: sql.NullString{String: spec.childSessionID, Valid: spec.childSessionID != ""},
		}
		if spec.payloadInput != "" {
			task.TaskPayload = sql.NullString{String: `{"input":` + spec.payloadInput + `}`, Valid: true}
		}
		children = append(children, session.BatchReservationNode{Values: task})
	}
	inputHash := spec.inputHash
	if inputHash == "" {
		inputHash = "hash-" + spec.toolCallID
	}
	_, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: spec.owner, Host: "batch-test-host", RunnerID: "lw-seed-runner",
		ToolCallID: spec.toolCallID, InputHash: inputHash, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: spec.state, ControlState: "none",
		}, Children: children},
	})
	require.NoError(t, err)
}

// projectLiveWorkRoot settles one seeded root — every node of it — to the
// given (terminal) state through BatchStore.Project.
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

// seedLiveWorkTerminal seeds one running live-work fact and immediately
// settles it to the given (terminal) state, mapping the state to the batch
// cause vocabulary exactly like the production terminal projections.
func seedLiveWorkTerminal(t *testing.T, a *appPkg.App, spec liveWorkSeedSpec, state string) {
	t.Helper()
	ctx := t.Context()
	spec.state = "running"
	seedLiveWorkRow(t, a, spec)
	cause := jobbatch.CauseNone
	switch state {
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
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: node.NodeID, State: state, Cause: string(cause), ControlState: "none"})
	}
	require.NoError(t, session.NewBatchStore(a.DB()).Project(ctx, session.BatchProjection{
		Host: root.HostID, RunnerID: root.RunnerID, Sequence: root.ProjectionSequence + 1,
		Roots: []session.BatchRootProjection{{Owner: spec.owner, Root: root.RootID, Updates: updates}},
	}))
}
