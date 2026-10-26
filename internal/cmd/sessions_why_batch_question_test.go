package cmd

// Coverage for the UNBOUND batch-published held question: the notice row is
// written by the REAL producer API (session.BatchStore.BatchPublishQuestion)
// with job_tool_call_id NULL and a marker-first frame, so the per-root bound
// query never sees it and the render goes through the fillBatchQuestionFromPending
// path (sessions_batch_read.go:497-516, session.ParseBatchChildQuestionNotice).
// The BOUND legacy-shaped variant is covered by
// TestExplainSessionStatus_AwaitingAnswerNamesQuestion.
//
// No raw UPDATE is used: the projection and the question publish both go
// through the production APIs (BatchStore.Project, BatchStore.BatchPublishQuestion).

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestExplainSessionStatus_BatchPublishedQuestionNamesChildAndResume: a
// question published the real batch way (BatchPublishQuestion, token-fenced)
// must render the asking child, the question text, and the resume line under
// `sessions why`, while the delegation stays batch live (never at rest).
func TestExplainSessionStatus_BatchPublishedQuestionNamesChildAndResume(t *testing.T) {
	a, _, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "batch question root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "batch-question-child", parent.ID, "worker")
	require.NoError(t, err)
	rootID, childNodeID := seedCmdBatchDelegation(t, a.DB(), parent.ID, "batch-q-fixture-host", "delegate-9", child.ID)

	// Token fence: BatchPublishQuestion refuses a token not matching the
	// persisted node, so first Project the task node's token to 7 with the
	// seed's live host identity (same shape projectBatchRoot uses).
	require.NoError(t, session.NewBatchStore(a.DB()).Project(ctx, session.BatchProjection{
		Host: "batch-q-fixture-host", RunnerID: "cmd-fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: parent.ID, Root: rootID, Updates: []db.ProjectBatchNodeParams{
			{NodeID: childNodeID, State: "running", ControlState: "none", Token: 7},
		}}},
	}))

	// Publish exactly like the producer (internal/agent/batch_install_question.go).
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n", childNodeID, 7, 3, 1, child.ID)
	text := marker + "SUB-AGENT QUESTION: ship or hold?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.NoError(t, session.NewBatchStore(a.DB()).BatchPublishQuestion(ctx, parent.ID, childNodeID, 7, marker, text))

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	out := buf.String()
	t.Logf("sessions why (batch-published question):\n%s", out)

	require.Contains(t, out, "ship or hold?",
		"the batch-published question text must be quoted (fillBatchQuestionFromPending path: the question is unbound, so the per-root bound query never sees it)")
	require.Contains(t, out, "waiting for your answer: child "+child.ID+" asked:",
		"the asking child must be named")
	require.Contains(t, out, "child_session_id=\""+child.ID+"\"",
		"the inject_agent answer path must name the child session")
	require.Contains(t, out, "status: batch live",
		"the held delegation keeps the verdict live")
	require.NotContains(t, out, "status: at rest")
}
