// E3 (test fixtures only): e3-prefixed helpers shared by the package-E3
// ports. Nothing here touches production code; every helper drives the
// batch authority only (batch_nodes through the production BatchStore API).
// The batch completion-notice delivery identity
// is read from batch_nodes.delivery (the f6/a2 projection path selects
// notices onto the node rows; batch_commit.go's batchCommitNotice maps them
// into ProjectBatchNode params, not session_notices rows).
package app

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// e3NodeRow is one pre-answer snapshot of the held question's persisted node
// row (the launched TASK node "<root>.1" the marker names), taken on the
// handler goroutine and compared after the run to prove the incarnation did
// not change across the inject answer.
type e3NodeRow struct {
	token          int64
	childSessionID string
	rootID         string
}

// e3BatchQuestionRe matches the production batch-question marker the ON
// publisher writes (internal/agent/batch_install_question.go:35):
// [batch-question node=<id> token=<n> sequence=<n> depth=<n> child=<childSessionID>]
var e3BatchQuestionRe = regexp.MustCompile(`\[batch-question node=(\S+) token=(\d+) sequence=(\d+) depth=(\d+) child=([^\]]+)\]`)

// e3GrantCoderInjectAgent grants inject_agent to the root coder agent, so the
// ON answer turn can inject into the HELD batch execution instead of resuming
// a new delegation generation (inject_agent is normally available through
// config's allToolNames; the explicit grant pins the contract).
func e3GrantCoderInjectAgent(t *testing.T, application *App) {
	t.Helper()
	cfg := application.config.Config()
	coder, ok := cfg.Agents[config.AgentCoder]
	require.True(t, ok, "the coder agent must be configured")
	tools := slices.Clone(coder.AllowedTools)
	if !slices.Contains(tools, "inject_agent") {
		tools = append(tools, "inject_agent")
	}
	application.config.UpdateAgentAllowedTools(config.AgentCoder, tools)
}

// e3CompleteOwnJobErr is the goroutine-safe completion of an own command job.
// It resolves the batch ROOT by owner + tool call id and delegates to
// f6a2CompleteDelegationErr: the coherent completion projects the root AND
// its task child terminal with the wake notice selected on the CHILD node --
// exactly what production selects on (an own command job still owns one
// running "command" task node under the batch runtime). Every failure
// returns so a caller goroutine can deliver the outcome.
func e3CompleteOwnJobErr(ctx context.Context, application *App, owner, toolCallID, summary string) error {
	var rootID string
	err := application.DB().QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes
		 WHERE owner_session_id = ? AND tool_call_id = ? AND node_id = root_id
		 ORDER BY rowid DESC LIMIT 1`,
		owner, toolCallID).Scan(&rootID)
	if err != nil {
		return fmt.Errorf("e3 own job root row missing for tool call %s: %w", toolCallID, err)
	}
	_ = rootID
	// The coherent f6a2 completion: root AND its task child terminal, with
	// the wake notice selected on the CHILD node -- exactly what production
	// selects on (an own command job still owns one running "command" task
	// node under the batch runtime).
	return f6a2CompleteDelegationErr(ctx, application, owner, toolCallID, summary, true)
}

func e3BatchNoticeDeliveries(t *testing.T, application *App, owner string) []string {
	t.Helper()
	rows, err := application.DB().QueryContext(context.Background(),
		`SELECT delivery FROM batch_nodes
		 WHERE owner_session_id = ? AND state = 'completed' AND notice_selected = 1
		 ORDER BY rowid`, owner)
	require.NoError(t, err)
	defer rows.Close()
	var deliveries []string
	for rows.Next() {
		var delivery string
		require.NoError(t, rows.Scan(&delivery))
		deliveries = append(deliveries, delivery)
	}
	require.NoError(t, rows.Err())
	return deliveries
}

// e3ReserveDelegationWithToken reserves the SAME shape f6ReserveDelegation
// builds (batch root kind=batch mode=parallel state=running + ONE linked task
// child, announced through BatchAnnounceStarted with a BackgroundJobNotice),
// but sets Token: 1 on BOTH root and child Values: batchInsertTree
// (internal/session/batch_store.go:130-149) inserts spec.Values verbatim per
// node, so without it the fixture's fenced node would carry token 0 and
// BatchPublishQuestion (internal/session/batch_question.go:31-44) would
// refuse the publish. Token 1 is the honest positive incarnation-token shape;
// the row's persisted token is read back, never assumed.
func e3ReserveDelegationWithToken(t *testing.T, application *App, owner, toolCallID, childID string) (root, node string) {
	t.Helper()
	ctx := context.Background()
	store := session.NewBatchStore(application.DB())
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: f6HostID(t, application), RunnerID: f6BatchRunner, ToolCallID: toolCallID,
		InputHash: "e3-hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none", Token: 1,
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
			ChildSessionID: sql.NullString{String: childID, Valid: childID != ""},
			State:          "running", ControlState: "none", Token: 1,
		}}}},
	})
	require.NoError(t, err)
	root, node = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, application.Messages, owner, root,
		message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "e3 fixture delegation started"}},
			// A started delegation is a background-job announcement: the same
			// shape the production announce path persists.
			BackgroundJobNotice: true,
		})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, node
}

// e3PublishHeldQuestion publishes the CURRENT held-question identity through
// the real committed path (session.BatchStore.BatchPublishQuestion) for a
// delegation seeded by e3ReserveDelegationWithToken: the marker carries the
// node's PERSISTED incarnation token (read from the row, required > 0 -- the
// publisher fences token == row token with a live state, batch_question.go:31-44),
// so LiveHeldQuestions' incarnation fence accepts it. The marker/text frame
// is byte-identical to the production publisher's (internal/agent/batch_install_question.go:35-37).
func e3PublishHeldQuestion(t *testing.T, ctx context.Context, application *App, owner, toolCallID, childSessionID, question string) {
	t.Helper()
	_, node := f6FindDelegationNodes(t, application, owner, toolCallID)
	var token int64
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT token FROM batch_nodes WHERE node_id = ?`, node).Scan(&token))
	require.Greater(t, token, int64(0),
		"the seeded node must carry a positive incarnation token (e3ReserveDelegationWithToken)")
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=1 depth=1 child=%s]\n", node, token, childSessionID)
	text := marker + "SUB-AGENT QUESTION: " + question + "\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.NoError(t, session.NewBatchStore(application.DB()).BatchPublishQuestion(ctx, owner, node, token, marker, text))
}

// e3BatchJobIdentityPresent requires exactly one completed batch ROOT row for
// owner + tool call id (one logical job, no second generation) and one
// corresponding completed TASK child whose outcome summary carries the
// marker. The structural ROOT row's outcome_summary is empty by design
// (tree_apply never copies task outcomes into roots), so the marker is
// asserted on the TASK row with Contains -- the task output is formatted
// content that embeds the cwd, not the bare command stdout. The root/task
// double-export of batchFinishedWork is package D and is not asserted here.
func e3BatchJobIdentityPresent(t *testing.T, application *App, owner, toolCallID, wantMarker string) {
	t.Helper()
	ctx := context.Background()
	var count int
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM batch_nodes
		 WHERE owner_session_id = ? AND tool_call_id = ? AND node_id = root_id AND state = 'completed'`,
		owner, toolCallID).Scan(&count),
		"completed batch root for tool call %s", toolCallID)
	require.Equal(t, 1, count, "exactly one completed batch root for tool call %s", toolCallID)
	var rootID, taskSummary string
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT root.node_id, task.outcome_summary
		 FROM batch_nodes AS root
		 JOIN batch_nodes AS task ON task.root_id = root.node_id AND task.kind = 'task' AND task.state = 'completed'
		 WHERE root.owner_session_id = ? AND root.tool_call_id = ? AND root.node_id = root.root_id AND root.state = 'completed'`,
		owner, toolCallID).Scan(&rootID, &taskSummary),
		"the completed task child of the root for tool call %s", toolCallID)
	if wantMarker != "" {
		require.Contains(t, taskSummary, wantMarker,
			"the task's outcome summary must carry the command output marker")
	}
}
