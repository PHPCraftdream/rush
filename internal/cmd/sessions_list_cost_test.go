package cmd

// The COST column of the `sessions list` table must be the same subtree
// budget --json reports as cost_usd: delegated spend is real spend. This
// file pins the table cell to that value.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionsListCmdRun_TableCostMatchesJSONSubtreeBudget: the table's COST
// cell must equal the --json cost_usd (the delegation-subtree budget), not
// the session's own cost. REVERT CHECK: pointing the table's COST column
// back at s.OwnCost turns this red.
func TestSessionsListCmdRun_TableCostMatchesJSONSubtreeBudget(t *testing.T) {
	a, _, _ := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	root, err := a.Sessions.CreateWithID(ctx, "list-cost-root", "orchestrator root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "list-cost-child", root.ID, "implementation sub-agent")
	require.NoError(t, err)

	// CreateTaskSession already set the child's cost_parent_id, so both
	// rows sit in the root's cost subtree: subtree budget = 4.82.
	_, err = a.DB().ExecContext(ctx,
		"UPDATE sessions SET cost_self = ? WHERE id = ?", 0.02, root.ID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		"UPDATE sessions SET cost_self = ? WHERE id = ?", 4.80, child.ID)
	require.NoError(t, err)

	// The command builds its own full App. Release the seed App's
	// process-wide MCP owner before invoking the command so the lifetimes
	// do not overlap.
	a.Shutdown()

	// JSON pass: the root's cost_usd is the subtree budget.
	require.NoError(t, sessionsListCmd.Flags().Set("json", "true"))
	jsonOut := captureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})

	var item *sessionListItem
	for _, line := range strings.Split(jsonOut, "\n") {
		if !strings.Contains(line, root.ID) {
			continue
		}
		var parsed sessionListItem
		require.NoError(t, json.Unmarshal([]byte(line), &parsed))
		item = &parsed
		break
	}
	require.NotNil(t, item, "the root session must appear in the --json output")
	require.InDelta(t, 4.82, item.CostUSD, 1e-9)

	// Table pass: the COST cell must print the same value.
	require.NoError(t, sessionsListCmd.Flags().Set("json", "false"))
	tableOut := captureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})

	var row string
	for _, line := range strings.Split(tableOut, "\n") {
		if strings.Contains(line, root.ID[:8]) {
			row = line
			break
		}
	}
	require.NotEmpty(t, row, "the root session must appear in the table output")
	want := fmt.Sprintf("$%.4f", item.CostUSD)
	require.Contains(t, row, want)
	require.NotContains(t, row, "$0.0200",
		"the COST cell must not be the root's own cost alone")
	require.NotContains(t, tableOut, child.ID,
		"child sessions are filtered out of sessions list")
}
