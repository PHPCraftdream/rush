package batchflow

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchAwaitNotifyEachChildFinished is the white-box half of the
// notify-each check moved from internal/agent/batch_await_test.go (the
// agent-glue half, stored root mode through batchAwaitNotifyModes, stayed in
// the agent package). The root test cannot name the unexported
// batchAwaitTarget fields, so the evaluation runs here, against the same
// production entry (BatchAwaitWorker's internal batchAwaitEvaluateFor path).
func TestBatchAwaitNotifyEachChildFinished(t *testing.T) {
	ref := session.BatchLiveRef{OwnerSessionID: "s1", RootID: "each-root"}
	live := session.BatchLevelResult{
		Ref: ref, Complete: true,
		Node:     session.BatchLiveNode{ToolCallID: "call-each-root"},
		Children: []session.BatchLiveNode{{State: "running"}},
	}
	levels := session.BatchCapturedLevels{Complete: true, Roots: []session.BatchLevelResult{live}}
	targets := []batchAwaitTarget{{ref: ref, notifyAll: false}}
	finished, remaining := batchAwaitEvaluateFor("any", targets, levels)
	require.Empty(t, finished)
	require.Len(t, remaining, 1)

	live.Children = []session.BatchLiveNode{{State: "completed"}}
	levels = session.BatchCapturedLevels{Complete: true, Roots: []session.BatchLevelResult{live}}
	finished, remaining = batchAwaitEvaluateFor("any", targets, levels)
	require.Len(t, finished, 1)
	require.Empty(t, remaining)
}
