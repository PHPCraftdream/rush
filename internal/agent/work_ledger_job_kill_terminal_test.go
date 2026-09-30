// B11 (docs/reviews/2026-09-29-async-phase4-round1.md), lost-race half: when
// the ledger row already reached a terminal state via another cause,
// job_kill must answer from the COMMITTED row and must not touch the shell
// manager at all -- no KillOwned, and no generic "background shell not
// found"/"terminated successfully" wording.
package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestWorkLedger_MarkJobStopped_AlreadyTerminalAnswersFromCommittedRow pins
// the ledger half deterministically: a job that finished naturally but is
// still in the map (its "started" ack not yet landed) is already terminal --
// MarkJobStopped must report JobStopAlreadyTerminal with text worded from
// the committed row, never "stopped (job_kill)", and must not mark it
// killRequested or cancel anything.
func TestWorkLedger_MarkJobStopped_AlreadyTerminalAnswersFromCommittedRow(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	var cancelled bool
	_, _, err := l.Start("owner", "call", "", "bash", "", false, false, nil, func() { cancelled = true })
	require.NoError(t, err)
	// No l.acknowledged: the terminal job stays in the map, undelivered.
	l.finish(jobOf(l, "owner", "call"), jobResult{content: "real committed output"})

	text, _, verdict := l.MarkJobStopped("owner", "call")
	require.Equal(t, tools.JobStopAlreadyTerminal, verdict)
	require.Contains(t, text, "real committed output")
	require.Contains(t, text, "finished")
	require.NotContains(t, text, "was stopped (job_kill)")
	require.False(t, cancelled)

	// A gone job is NotFound, never AlreadyTerminal.
	_, _, verdict = l.MarkJobStopped("owner", "no-such-call")
	require.Equal(t, tools.JobStopNotFound, verdict)
}

// TestJobKillTool_RealLedger_LostRaceNeverKillsTheShell drives the REAL
// job_kill tool against the REAL ledger and a REAL background shell: the job
// finished naturally (terminal in the ledger) while its shell entry still
// lingers in the manager -- exactly the state B11 describes. job_kill must
// answer from the committed row and leave the shell alone.
//
// Revert-check performed: made job_kill.go treat JobStopAlreadyTerminal like
// JobStopStopped (fall through to KillOwned) -- this test FAILED
// (bgShell.IsDone() was true and the answer was the ledger's text only by
// accident of order). Restored the early return; re-ran, passed.
func TestJobKillTool_RealLedger_LostRaceNeverKillsTheShell(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "owner")

	workDir := t.TempDir() // created first: its cleanup must run AFTER the shell manager closes
	bgManager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { bgManager.Close(context.Background()) })
	bgShell, err := bgManager.StartOwned(ctx, "owner", workDir, nil, "sleep 30", "")
	require.NoError(t, err)

	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err = l.Start("owner", "call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.setShellID(jobOf(l, "owner", "call"), bgShell.ID)
	l.finish(jobOf(l, "owner", "call"), jobResult{content: "finished on its own"}) // terminal, still in the map (unannounced)

	tool := tools.NewJobKillTool(l, l, bgManager)
	input, err := json.Marshal(tools.JobKillParams{JobID: "call"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "kill-call", Name: tools.JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "finished on its own")
	require.NotContains(t, resp.Content, "was stopped (job_kill)")
	require.NotContains(t, resp.Content, "terminated successfully")
	require.False(t, bgShell.IsDone(), "job_kill must not kill a shell whose job already reached a terminal state")
}
