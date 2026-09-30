package cmd

// R7C-4: a run's ended_reason and budget are on the session row, but
// `sessions show` read them through a Get that did not carry them. These tests
// pin the command side (the printed / JSON fields); the store side (Get and
// List carrying the columns) is covered in internal/session.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The list JSON documents ended_reason (run.go's help promises it).
//
// Revert-check: dropping EndedReason from makeSessionListItem loses the field.
func TestMakeSessionListItem_CarriesEndedReason(t *testing.T) {
	raw, err := json.Marshal(makeSessionListItem(session.Session{ID: "s", EndedReason: "canceled"}))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"ended_reason":"canceled"`)

	raw, err = json.Marshal(makeSessionListItem(session.Session{ID: "s"}))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "ended_reason", "a session without a finished run omits the field")
}

// runSessionsCmd runs the real RunE of a sessions subcommand against dataDir
// with the given boolean flags and returns its stdout.
func runSessionsCmd(t *testing.T, cmd *cobra.Command, dataDir string, boolFlags map[string]bool, args ...string) string {
	t.Helper()
	ensureRootFlagStandIns(cmd, dataDir)
	if f := cmd.Flags().Lookup("cwd"); f == nil {
		cmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, cmd.Flags().Set("cwd", ""))
	for name, val := range boolFlags {
		require.NoError(t, cmd.Flags().Set(name, boolFlag(val)))
	}
	cmd.SetContext(context.Background())
	stdout, _ := captureStdoutAndStderr(t, func() { require.NoError(t, cmd.RunE(cmd, args)) })
	return stdout
}

// End to end through the store: the values a run persists come back from
// `sessions show` (text and JSON) and `sessions list --json`. Needs
// Sessions.Get / List to carry EndedReason and the budget columns (R7C-4,
// store part): it fails while fromDBItem drops them.
//
// Revert-check: dropping the fields from fromDBItem's Session literal makes
// every assertion on them fail.
func TestSessionsShowAndListCmdRun_ShowEndedReasonAndBudget(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "show-ended", "show ended")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.SetBudget(ctx, sess.ID, 2.5, 1000, 3600))
	require.NoError(t, a.Sessions.SetEndedReason(ctx, sess.ID, "canceled"))
	a.Shutdown()

	out := runSessionsCmd(t, sessionsShowCmd, dataDir, map[string]bool{"json": true, "with-messages": false, "full": false, "with-subagents": false}, sess.ID)
	var shown struct {
		EndedReason      string  `json:"ended_reason"`
		BudgetMaxCost    float64 `json:"budget_max_cost"`
		BudgetMaxTokens  int64   `json:"budget_max_tokens"`
		BudgetTimeoutSec int64   `json:"budget_timeout_sec"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &shown))
	require.Equal(t, "canceled", shown.EndedReason)
	require.InDelta(t, 2.5, shown.BudgetMaxCost, 1e-9)
	require.EqualValues(t, 1000, shown.BudgetMaxTokens)
	require.EqualValues(t, 3600, shown.BudgetTimeoutSec)

	out = runSessionsCmd(t, sessionsShowCmd, dataDir, map[string]bool{"json": false, "with-messages": false, "full": false, "with-subagents": false}, sess.ID)
	require.Contains(t, out, "Ended:        canceled")
	require.Contains(t, out, "/ $2.50 budget")
	require.Contains(t, out, "Token budget:")

	out = runSessionsCmd(t, sessionsListCmd, dataDir, map[string]bool{"json": true})
	var listed []struct {
		ID          string `json:"id"`
		EndedReason string `json:"ended_reason"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var item struct {
			ID          string `json:"id"`
			EndedReason string `json:"ended_reason"`
		}
		require.NoError(t, dec.Decode(&item))
		listed = append(listed, item)
	}
	require.Len(t, listed, 1)
	require.Equal(t, "canceled", listed[0].EndedReason)
}
