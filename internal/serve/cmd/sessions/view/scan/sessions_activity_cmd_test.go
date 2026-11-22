package sessscan

// The one-table oracle for R-ACT-2 (task item 4): a single table of fact
// shapes, each row asserted against the private list/tail routes and the
// real `sessions why` command path plus shared production classifier.
//
// Revert-check mutant: re-introducing ANY pre-classifier local decision (a
// lock-only running check, an mtime-based list status, a per-reader
// live-work aggregation) breaks exactly the rows whose shape the local rule
// misjudged -- e.g. an mtime rule reads shape "released" (fresh empty lock)
// as running and fails its list/why/tail cells. The why cell drives the real
// command (not explainSessionStatus directly) so the command's own flag and
// App route cannot drift from the table.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	sessstate "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/state"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestSessionsActivity_OneTableEveryCommand(t *testing.T) {
	ctx := context.Background()
	a, dataDir, store := cmdtest.NewActivityCmdApp(t)

	mk := func(name string) coresession.Session {
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		return sess
	}

	// --- fact shapes -----------------------------------------------------

	// in turn: the lock record names this (alive) process.
	inTurn := mk("in-turn")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "locks"), 0o755))
	heldPath := filepath.Join(dataDir, "locks", "session-"+sessionfacts.SessionLockStem(inTurn.ID)+".lock")
	require.NoError(t, os.WriteFile(heldPath, []byte(cmdtest.ItoaCmd(os.Getpid())+"\n"), 0o644))

	// between turns (R6C-1/R5C-5): released lock, live driver, debt pending.
	between := mk("between")
	require.NoError(t, store.ClaimSessionDriver(ctx, between.ID))
	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, between.ID, "wake", "x", true, ""))
	cmdtest.ReleasedLock(t, dataDir, between.ID)

	// delegating (D2): a live delegation outranks everything else. The
	// delegation is a live batch root on the fixture's own (alive) host;
	// `sessions why` reads it as batch live (the batch facts outrank the
	// classifier), while list/inject/tail read the classifier verdict.
	delegating := mk("delegating")
	require.NoError(t, store.ClaimSessionDriver(ctx, delegating.ID))
	child := mk("delegating-child")
	cmdtest.ClaimDelegation(t, a, delegating.ID, "call-1", child.ID)

	// crashed (D3): a dead foreign PID and NO live work, whatever the finish.
	crashed := mk("crashed")
	require.NoError(t, a.Sessions.SetEndedReason(ctx, crashed.ID, "end_turn"))
	crashedPath := filepath.Join(dataDir, "locks", "session-"+sessionfacts.SessionLockStem(crashed.ID)+".lock")
	require.NoError(t, os.WriteFile(crashedPath, []byte("2147483646\n"), 0o644))

	// ended (D4): the recorded reason is the only end signal.
	ended := mk("ended")
	require.NoError(t, a.Sessions.SetEndedReason(ctx, ended.ID, "end_turn"))

	// idle: nothing at all.
	idle := mk("idle")

	ids := []string{inTurn.ID, between.ID, delegating.ID, crashed.ID, ended.ID, idle.ID}
	for _, id := range ids {
		cmdtest.AddFinishedAssistant(t, a.Messages, id, message.FinishReasonEndTurn)
	}
	acts, err := a.SessionActivityBatch(ctx, ids)
	require.NoError(t, err)
	for _, id := range ids {
		require.Empty(t, acts.ByID[id].Facts.Unreadable, "no fact read may fail in this fixture (%s)", id)
	}

	type expect struct {
		status    string // list STATUS word ("" = at rest)
		whyStatus string // `sessions why` first line
		live      bool   // inject running / locks between-turns guard / watch keeps
	}
	table := map[string]expect{
		inTurn.ID:     {status: "running", whyStatus: "status: running", live: true},
		between.ID:    {status: "running", whyStatus: "status: running", live: true},
		delegating.ID: {status: "delegating", whyStatus: "status: batch live", live: true},
		crashed.ID:    {status: "crashed", whyStatus: "status: crashed", live: false},
		ended.ID:      {status: "done", whyStatus: "status: done", live: false},
		idle.ID:       {status: "", whyStatus: "status: at rest", live: false},
	}

	for _, id := range ids {
		want := table[id]
		// list cell: exercise the private list route itself.
		require.Equal(t, want.status, listStatus(acts.ByID[id].Verdict), id)
		// why first line: execute the real command with a config-backed App,
		// because NewSessionsWhyCmd reads Config() and shuts its App down.
		whyCmd := sessstate.NewSessionsWhyCmd(func(cmd *cobra.Command) (*app.App, error) {
			conn, err := db.Connect(cmd.Context(), dataDir)
			if err != nil {
				return nil, err
			}
			cfg := config.NewTestStore(&config.Config{Options: &config.Options{DataDirectory: dataDir}})
			whyApp, err := app.New(cmd.Context(), conn, cfg, app.SkipAgentSetup())
			if err != nil {
				_ = db.ReleaseConn(conn)
				return nil, err
			}
			return whyApp, nil
		})
		whyCmd.SetContext(ctx)
		whyCmd.SetArgs([]string{id})
		var whyErr error
		whyOutput := cmdtest.CaptureStdout(t, func() { whyErr = whyCmd.Execute() })
		require.NoError(t, whyErr, "sessions why for %s", id)
		require.True(t, strings.HasPrefix(whyOutput, want.whyStatus+"\n"),
			"%s: why output %q does not start with %q", id, whyOutput, want.whyStatus)
		// inject running / watch-tail predicate / locks guard: one verdict
		require.Equal(t, want.live, sesskit.KindIsLive(acts.ByID[id].Verdict.Kind), id)
		// Keep tail's independent white-box implementation check.
		tailFinished, _ := tailSessionFinished(ctx, a, id)
		require.Equal(t, !want.live, tailFinished, id)
		// locks stem map (D11): the real id is reachable from its stem
		stem := sessionfacts.SessionLockStem(id)
		require.Contains(t, acts.LockStems[stem], id, id)
	}

	// The dead-PID lock of the crashed session must never read as live in
	// the locks guard, even aged (the pre-classifier mtime rule said live
	// for a fresh empty lock -- D10's regression).
	require.False(t, sesskit.KindIsLive(acts.ByID[crashed.ID].Verdict.Kind))
}
