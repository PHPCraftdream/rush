package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// controlClient builds a client on a live hub so batch controls take the REAL
// dispatch path: handleIncoming enqueues to the work queue and a worker runs
// the handler (Go embedding is not virtual, so a wrapper overriding dispatch
// would never intercept it).
func controlClient(hub *Hub) *Client {
	c := newClient(hub, nil)
	c.send = make(chan []byte, 64)
	hub.register <- c
	return c
}

func controlFixture(t *testing.T) (*app.App, session.Session, string, string) {
	t.Helper()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	owner, err := a.Sessions.Create(t.Context(), "control owner")
	require.NoError(t, err)
	res, err := session.NewBatchStore(a.DB()).Reserve(t.Context(), session.BatchReservation{
		Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "control-test",
		ToolCallID: "call", InputHash: "hash", Sequence: 1,
		Root: session.BatchReservationNode{
			Values:   db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "running", ControlState: "none"}}},
		},
	})
	require.NoError(t, err)
	return a, owner, res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
}

// controlRequest sends a batch control message through handleIncoming on a
// real hub client, then drains client.send within a bounded deadline and
// returns the FIRST frame whose type matches expectedType (the queue may
// interleave unrelated frames).
func controlRequest(t *testing.T, a *app.App, c *Client, typ string, expectedType string, payload any) WSMessage {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	raw, err := json.Marshal(WSMessage{ID: "test", Type: typ, Payload: body})
	require.NoError(t, err)
	handleIncoming(context.Background(), &Server{app: a}, c, raw)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case response := <-c.send:
			var got WSMessage
			require.NoError(t, json.Unmarshal(response, &got))
			if got.Type == expectedType {
				return got
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("no %s reply arrived", expectedType)
	return WSMessage{}
}

func TestBatchControlDispatchUsesWorkerQueue(t *testing.T) {
	a, owner, _, _ := controlFixture(t)
	hub := newHub()
	go hub.Run(t.Context())
	got := controlRequest(t, a, controlClient(hub), CmdBatchStatus, EventSessionBatchWork, batchControlPayload{SessionID: owner.ID})
	require.Equal(t, EventSessionBatchWork, got.Type)
}

func TestBatchStopPersistsQueuedDurableRequest(t *testing.T) {
	a, owner, root, node := controlFixture(t)
	hub := newHub()
	go hub.Run(t.Context())
	got := controlRequest(t, a, controlClient(hub), CmdBatchStop, EventBatchControlResult, batchControlPayload{SessionID: owner.ID, RootID: root, NodeID: node})
	var result struct {
		SessionID string `json:"sessionID"`
		RootID    string `json:"rootID"`
		NodeID    string `json:"nodeID"`
		Result    string `json:"result"`
		OK        bool   `json:"ok"`
		RequestID string `json:"requestID"`
	}
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.True(t, result.OK)
	require.Equal(t, "queued", result.Result)
	require.NotEmpty(t, result.RequestID)
	require.Equal(t, root, result.RootID)
	require.Equal(t, node, result.NodeID)
	fac, err := a.BatchCLIStore()
	require.NoError(t, err)
	identity, _, err := fac.Store.BatchCLIFind(t.Context(), node)
	require.NoError(t, err)
	target := session.BatchControlTarget{Owner: owner.ID, Host: identity.Host, Root: root, Node: node}
	row, err := fac.Store.BatchReadControl(t.Context(), target, result.RequestID)
	require.NoError(t, err)
	require.Equal(t, "pending", row.Status, "queued intent must not claim physical termination")
}

func TestBatchAddValidatesScopeAndPassesExactItemsAndIDs(t *testing.T) {
	a, owner, root, node := controlFixture(t)
	other, err := a.Sessions.Create(t.Context(), "other")
	require.NoError(t, err)
	hub := newHub()
	go hub.Run(t.Context())
	client := controlClient(hub)
	items := []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Kind: "test", Payload: json.RawMessage(`{"x":1}`)}}}
	calls := 0
	old := batchAddForWeb
	batchAddForWeb = func(_ context.Context, _ *app.App, gotRoot, gotNode string, gotItems []jobbatch.Spec) error {
		calls++
		require.Equal(t, root, gotRoot)
		require.Equal(t, node, gotNode)
		require.Equal(t, items, gotItems)
		return nil
	}
	t.Cleanup(func() { batchAddForWeb = old })
	payload := batchControlPayload{SessionID: owner.ID, RootID: root, NodeID: node, Items: items}
	got := controlRequest(t, a, client, CmdBatchAdd, EventBatchControlResult, payload)
	var result BatchControlResult
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.True(t, result.OK)
	require.Equal(t, 1, calls)
	payload.SessionID = other.ID
	got = controlRequest(t, a, client, CmdBatchAdd, EventBatchControlResult, payload)
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.False(t, result.OK)
	require.Equal(t, 1, calls, "foreign-session rejection happens before App boundary")
	payload.SessionID, payload.NodeID = owner.ID, "missing-node"
	got = controlRequest(t, a, client, CmdBatchAdd, EventBatchControlResult, payload)
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.False(t, result.OK)
	require.Equal(t, 1, calls, "missing-node rejection happens before App boundary")
}

func TestBatchControlStopRejectsForeignSession(t *testing.T) {
	a, _, root, node := controlFixture(t)
	other, err := a.Sessions.Create(t.Context(), "foreign stop")
	require.NoError(t, err)
	hub := newHub()
	go hub.Run(t.Context())
	got := controlRequest(t, a, controlClient(hub), CmdBatchStop, EventBatchControlResult, batchControlPayload{SessionID: other.ID, RootID: root, NodeID: node})
	var result BatchControlResult
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.False(t, result.OK)
	require.Contains(t, result.Result, "not found in requested session/root")
}

func TestBatchOutputReturnsRetainedEnvelopeAndChecksOwner(t *testing.T) {
	a, owner, root, _ := controlFixture(t)
	store := session.NewBatchStore(a.DB())
	node, err := store.BatchCLIGetNode(t.Context(), owner.ID, root, root)
	require.NoError(t, err)
	err = store.Project(t.Context(), session.BatchProjection{Host: node.HostID, RunnerID: node.RunnerID, Sequence: node.ProjectionSequence + 1, Roots: []session.BatchRootProjection{{Owner: owner.ID, Root: root, Updates: []db.ProjectBatchNodeParams{{NodeID: root, State: "completed", ControlState: "none", OutcomeCode: "ok", OutcomeSummary: "finished", ResponseEnvelopeRef: sql.NullString{String: root + "#2", Valid: true}, ResponseEnvelope: sql.NullString{String: `{"state":"completed","summary":"finished","details":{"answer":7}}`, Valid: true}}}}}})
	require.NoError(t, err)
	hub := newHub()
	go hub.Run(t.Context())
	client := controlClient(hub)
	got := controlRequest(t, a, client, CmdBatchOutput, EventBatchControlResult, batchControlPayload{SessionID: owner.ID, RootID: root, NodeID: root})
	var result map[string]any
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.Equal(t, true, result["ok"])
	require.Equal(t, "retained outcome (not a live stream)", result["result"])
	require.Contains(t, result["responseEnvelope"].(string), "answer")
	other, err := a.Sessions.Create(t.Context(), "foreign")
	require.NoError(t, err)
	got = controlRequest(t, a, client, CmdBatchOutput, EventBatchControlResult, batchControlPayload{SessionID: other.ID, RootID: root, NodeID: root})
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.Equal(t, false, result["ok"])
}

func TestBatchControlRefusesInvalidWaitAndDrainWithoutPublicBoundary(t *testing.T) {
	a, owner, root, node := controlFixture(t)
	hub := newHub()
	go hub.Run(t.Context())
	client := controlClient(hub)
	got := controlRequest(t, a, client, CmdBatchStop, EventBatchControlResult, batchControlPayload{SessionID: owner.ID, RootID: root, NodeID: node, WaitMS: 30001})
	var result BatchControlResult
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.False(t, result.OK)
	require.Contains(t, result.Result, "waitMS")
	got = controlRequest(t, a, client, CmdBatchDrain, EventBatchControlResult, batchControlPayload{SessionID: owner.ID})
	require.NoError(t, json.Unmarshal(got.Payload, &result))
	require.False(t, result.OK)
	require.Contains(t, result.Result, "no public")
}
