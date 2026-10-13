package server

// Batch-work snapshot handler + push (R15a): structured batch_nodes trees
// delivered as session_batch_work, INERT unless RUSH_BATCH_RUNTIME is on.
//
// Revert-check: forcing Complete=true in buildBatchWorkSnapshot fails
// TestBatchWorkSnapshot_IncompleteKeepsRoots and the unknown-host test;
// deriving completions from a parsed summary string fails
// TestBatchWorkSnapshot_StructuredCompletions; sorting nodes by NodeID
// fails TestBatchWorkSnapshot_NodesOrderedByOrdinal; dropping the flag
// guard in handleIncoming's dispatch fails
// TestHandleGetSessionBatchWork_FlagOffIsUnknownCommand; treating an
// unknown host as dead fails TestBatchWorkSnapshot_UnknownHostIsConservative.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeBatchSource serves canned node rows per owner list; failOn makes the
// corresponding fetch return an error (a failed chunk).
type fakeBatchSource struct {
	rows   []db.BatchNode
	failOn map[string]bool
}

func (f *fakeBatchSource) BatchLiveNodesForOwners(_ context.Context, owners []string) ([]db.BatchNode, error) {
	for _, o := range owners {
		if f.failOn[o] {
			return nil, errors.New("chunk read failed")
		}
	}
	return f.rows, nil
}

// withBatchSeams swaps the production seams for fakes and restores them.
func withBatchSeams(t *testing.T, src batchNodeSource, probe func(dataDir, host string) session.HostLockStatus, own func(string) bool) {
	t.Helper()
	oldSrc, oldDir, oldProbe, oldOwn := batchSourceFor, batchDataDir, batchProbeHost, batchIsOwnHost
	batchSourceFor = func(*app.App) batchNodeSource { return src }
	batchDataDir = func(*app.App) string { return "/fake-data-dir" }
	batchProbeHost = probe
	if own != nil {
		batchIsOwnHost = own
	}
	t.Cleanup(func() {
		batchSourceFor, batchDataDir, batchProbeHost, batchIsOwnHost = oldSrc, oldDir, oldProbe, oldOwn
	})
}

func probeAlive(_ string, _ string) session.HostLockStatus { return session.HostStatusAlive }

func batchNode(nodeID, rootID, owner, state, toolCall string, ordinal int64) db.BatchNode {
	n := db.BatchNode{
		NodeID:         nodeID,
		RootID:         rootID,
		OwnerSessionID: owner,
		Kind:           "task",
		State:          state,
		Cause:          "",
		Ordinal:        ordinal,
		CreatedAt:      time.Unix(1700000000, 0).Unix(),
		HostID:         "host-alive",
	}
	n.ToolCallID = sql.NullString{String: toolCall, Valid: toolCall != ""}
	return n
}

func TestBatchWorkSnapshot_GoldenShape(t *testing.T) {
	root := batchNode("r1", "r1", "s1", "running", "tc-root", 0)
	root.TaskKind = sql.NullString{String: "run", Valid: true}
	child := batchNode("r1-c", "r1", "s1", "completed", "tc-c1", 2)
	child.OutcomeCode = "done"
	child.ResponseEnvelopeRef = sql.NullString{String: "env-1", Valid: true}
	mid := batchNode("r1-b", "r1", "s1", "running", "tc-b1", 1)
	withBatchSeams(t, &fakeBatchSource{rows: []db.BatchNode{root, child, mid}}, probeAlive, nil)

	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.True(t, snap.Complete)
	require.Len(t, snap.Roots, 1)
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	var golden map[string]any
	require.NoError(t, json.Unmarshal(raw, &golden))
	require.Equal(t, float64(1), golden["v"])
	require.Equal(t, "s1", golden["sessionID"])
	require.Equal(t, true, golden["complete"])
	rootWire := golden["roots"].([]any)[0].(map[string]any)
	require.Equal(t, "r1", rootWire["rootID"])
	require.Equal(t, "running", rootWire["state"])
	require.Equal(t, float64(1700000000000), rootWire["startedAt"])
	require.Equal(t, "alive", rootWire["host"])
	counts := rootWire["counts"].(map[string]any)
	require.Equal(t, float64(2), counts["running"])
	require.Equal(t, float64(1), counts["completed"])
	// Nodes ordered by ordinal, each carrying its tree position.
	nodes := rootWire["nodes"].([]any)
	require.Len(t, nodes, 3)
	require.Equal(t, "r1", nodes[0].(map[string]any)["nodeID"])
	require.Equal(t, float64(1), nodes[1].(map[string]any)["ordinal"])
	require.Equal(t, float64(0), nodes[0].(map[string]any)["depth"])
	// Structured completion from columns, not prose.
	comps := golden["completions"].([]any)
	require.Len(t, comps, 1)
	comp := comps[0].(map[string]any)
	require.Equal(t, "r1", comp["rootID"])
	require.Equal(t, "r1-c", comp["nodeID"])
	require.Equal(t, "done", comp["outcomeCode"])
	require.Equal(t, "env-1", comp["outcomeRef"])
}

func TestBatchWorkSnapshot_IncompleteKeepsRoots(t *testing.T) {
	root := batchNode("r1", "r1", "s1", "running", "tc-root", 0)
	root.ChildSessionID = sql.NullString{String: "child-dead", Valid: true}
	src := &fakeBatchSource{rows: []db.BatchNode{root}, failOn: map[string]bool{"child-dead": true}}
	withBatchSeams(t, src, probeAlive, nil)

	// A failed chunk for a CHILD session must downgrade the scope without
	// dropping the roots that WERE read.
	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.False(t, snap.Complete, "a failed fetch must make the scope incomplete")
	require.NotEmpty(t, snap.Roots, "an incomplete snapshot still carries the roots it read")
}

func TestBatchWorkSnapshot_UnknownHostIsConservative(t *testing.T) {
	root := batchNode("r1", "r1", "s1", "running", "tc-root", 0)
	root.HostID = "host-mystery"
	withBatchSeams(t, &fakeBatchSource{rows: []db.BatchNode{root}},
		func(_, _ string) session.HostLockStatus { return session.HostStatusUnknown }, nil)

	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.False(t, snap.Complete, "an unknown host must keep the scope incomplete")
	require.False(t, snap.Roots[0].Complete)
	require.Equal(t, "unknown", snap.Roots[0].Host)
	require.Equal(t, "unknown", snap.Roots[0].Nodes[0].Host)
}

func TestBatchWorkSnapshot_NodesOrderedByOrdinal(t *testing.T) {
	// Rows arrive deliberately out of ordinal order, and the ids are chosen
	// so NodeID lexical order (r1, r1-b, r1-m) ALSO differs from ordinal
	// order: only an ordinal sort yields 0,1,2.
	root := batchNode("r1", "r1", "s1", "running", "tc-root", 0)
	b := batchNode("r1-b", "r1", "s1", "running", "tc-b", 2)
	m := batchNode("r1-m", "r1", "s1", "running", "tc-m", 1)
	withBatchSeams(t, &fakeBatchSource{rows: []db.BatchNode{root, b, m}}, probeAlive, nil)

	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	got := []string{}
	for _, n := range snap.Roots[0].Nodes {
		got = append(got, n.NodeID)
	}
	require.Equal(t, []string{"r1", "r1-m", "r1-b"}, got, "nodes must be ordered by ordinal, not arrival or NodeID")
}

func TestBatchWorkSnapshot_StructuredCompletions(t *testing.T) {
	root := batchNode("r1", "r1", "s1", "running", "tc-root", 0)
	done := batchNode("r1-d", "r1", "s1", "completed", "tc-d", 1)
	done.OutcomeCode = "ok"
	done.OutcomeSummary = "prose summary text"
	done.ResponseEnvelopeRef = sql.NullString{String: "env-9", Valid: true}
	running := batchNode("r1-r", "r1", "s1", "running", "tc-r", 2)
	withBatchSeams(t, &fakeBatchSource{rows: []db.BatchNode{root, done, running}}, probeAlive, nil)

	snap := buildBatchWorkSnapshot(t.Context(), nil, "s1")
	require.Len(t, snap.Completions, 1, "only the terminal node yields a completion")
	comp := snap.Completions[0]
	require.Equal(t, BatchCompletionWire{RootID: "r1", NodeID: "r1-d", OutcomeCode: "ok", OutcomeRef: "env-9"}, comp)
	// The completion carries ONLY column values: no summary prose field
	// exists on the wire, and nothing on this path parses text.
	raw, err := json.Marshal(snap.Completions)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "prose summary text")
	require.NotContains(t, string(raw), "summary")
}

func TestHandleGetSessionBatchWork_FlagOffIsUnknownCommand(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "")
	require.False(t, batchRuntimeEnabled())
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	payload, _ := json.Marshal(GetSessionBatchWorkPayload{SessionID: "whatever"})
	require.NoError(t, handleIncomingSync(t, a, client, payload))

	var gotErr bool
	var text string
	deadline := time.Now().Add(2 * time.Second)
	for !gotErr && time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type == EventError {
				gotErr = true
				text = env.Error
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.True(t, gotErr, "flag off must answer the plain unknown-command error")
	require.Contains(t, text, "unknown command: get_session_batch_work")
}

// handleIncomingSync runs the dispatcher inline (dispatch runs handlers on
// the calling goroutine through the semaphore, so a single reply arrives
// synchronously enough for the channel drain above).
func handleIncomingSync(t *testing.T, a *app.App, client *Client, payload []byte) error {
	t.Helper()
	s := &Server{app: a}
	msg, err := json.Marshal(WSMessage{ID: "req-1", Type: CmdGetSessionBatchWork, Payload: payload})
	if err != nil {
		return err
	}
	go handleIncoming(t.Context(), s, client, msg)
	return nil
}

func TestHandleGetSessionBatchWork_FlagOnRepliesSnapshot(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, batchRuntimeEnabled())
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	sess, err := a.Sessions.Create(t.Context(), "batch flag on")
	require.NoError(t, err)
	root := batchNode("r1", "r1", sess.ID, "running", "tc-root", 0)
	withBatchSeams(t, &fakeBatchSource{rows: []db.BatchNode{root}}, probeAlive, nil)

	payload, _ := json.Marshal(GetSessionBatchWorkPayload{SessionID: sess.ID})
	require.NoError(t, handleIncomingSync(t, a, client, payload))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionBatchWork {
				continue
			}
			var snap BatchWorkSnapshotWire
			require.NoError(t, json.Unmarshal(env.Payload, &snap))
			require.Equal(t, sess.ID, snap.SessionID)
			require.Len(t, snap.Roots, 1)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no session_batch_work reply arrived")
}

func TestHandleGetSessionBatchWork_UnknownSessionRefused(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	payload, _ := json.Marshal(GetSessionBatchWorkPayload{SessionID: "does-not-exist"})
	require.NoError(t, handleIncomingSync(t, a, client, payload))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventError {
				continue
			}
			require.Contains(t, env.Error, "session not found")
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no error reply for a non-existent session")
}
