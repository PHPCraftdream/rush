package sessscan

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	sessview "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The list JSON documents ended_reason (run.go's help promises it).
//
// Revert-check: dropping EndedReason from makeSessionListItem loses the field.
func TestMakeSessionListItem_CarriesEndedReason(t *testing.T) {
	raw, err := json.Marshal(makeSessionListItem(session.Session{ID: "s", EndedReason: "canceled"}, 0))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"ended_reason":"canceled"`)

	raw, err = json.Marshal(makeSessionListItem(session.Session{ID: "s"}, 0))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "ended_reason", "a session without a finished run omits the field")
}

// seedEndedReasonSession uses a real configured App/database for persistence;
// the command under test is invoked separately through its production RunE.
func seedEndedReasonSession(t *testing.T, cmd *cobra.Command, id string) (dataDir string) {
	t.Helper()
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	dataDir = filepath.Join(tmp, "sessions-ended-reason")
	if cmd.Flags().Lookup("debug") == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if cmd.Flags().Lookup("data-dir") == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	require.NoError(t, cmd.Flags().Set("data-dir", dataDir))
	cmd.SetContext(context.Background())

	a, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	t.Cleanup(a.Shutdown)
	sess, err := a.Sessions.CreateWithID(context.Background(), id, "ended reason session")
	require.NoError(t, err)
	require.NoError(t, a.Sessions.SetBudget(context.Background(), sess.ID, 2.5, 1000, 3600))
	require.NoError(t, a.Sessions.SetEndedReason(context.Background(), sess.ID, "canceled"))
	a.Shutdown()
	return dataDir
}

// End to end through the store: the values a run persists come back from
// `sessions show` (text and JSON) and `sessions list --json`. Needs
// Sessions.Get / List to carry EndedReason and the budget columns (R7C-4,
// store part): it fails while fromDBItem drops them.
//
// Revert-check: dropping the fields from fromDBItem's Session literal makes
// every assertion on them fail.
//
// Show and list are exercised through independent production commands and app
// setups, so both real command routes—not only their private projections—are
// covered.
func TestSessionsShowAndListCmdRun_ShowEndedReasonAndBudget(t *testing.T) {
	t.Run("show", func(t *testing.T) {
		cmd := sessview.NewSessionsShowCmd(cmdkit.SetupApp)
		const sessionID = "show-ended-reason"
		dataDir := seedEndedReasonSession(t, cmd, sessionID)
		require.NoError(t, cmd.Flags().Set("json", "true"))

		ensureEndedReasonCommandDataDir(t, cmd, dataDir)
		out := cmdtest.CaptureStdout(t, func() {
			require.NoError(t, cmd.RunE(cmd, []string{sessionID}))
		})
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

		require.NoError(t, cmd.Flags().Set("json", "false"))
		out = cmdtest.CaptureStdout(t, func() {
			require.NoError(t, cmd.RunE(cmd, []string{sessionID}))
		})
		require.Contains(t, out, "Ended:        canceled")
		require.Contains(t, out, "/ $2.50 budget")
		require.Contains(t, out, "Token budget:")
	})

	t.Run("list", func(t *testing.T) {
		cmd := NewSessionsListCmd()
		const sessionID = "list-ended-reason"
		dataDir := seedEndedReasonSession(t, cmd, sessionID)
		require.NoError(t, cmd.Flags().Set("json", "true"))

		ensureEndedReasonCommandDataDir(t, cmd, dataDir)
		out := cmdtest.CaptureStdout(t, func() {
			require.NoError(t, cmd.RunE(cmd, nil))
		})
		dec := json.NewDecoder(strings.NewReader(out))
		var listed []struct {
			ID          string `json:"id"`
			EndedReason string `json:"ended_reason"`
		}
		for {
			var row struct {
				ID          string `json:"id"`
				EndedReason string `json:"ended_reason"`
			}
			err := dec.Decode(&row)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if row.ID == sessionID {
				listed = append(listed, row)
				require.Equal(t, "canceled", row.EndedReason)
			}
		}
		require.Len(t, listed, 1)
		require.Equal(t, sessionID, listed[0].ID)
		require.Equal(t, "canceled", listed[0].EndedReason)
	})
}

// Ensure each RunE's own SetupApp opens the same database used by the seed.
func ensureEndedReasonCommandDataDir(t *testing.T, cmd *cobra.Command, dataDir string) {
	t.Helper()
	if cmd.Flags().Lookup("data-dir") == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	require.NoError(t, cmd.Flags().Set("data-dir", dataDir))
	cmd.SetContext(context.Background())
}
