package sessstate

// Shared test fixtures for the relocated `sessions status` / `sessions why`
// / `sessions cache` state-command test family (M7 lane A). Helpers that
// were byte-shared across the root cmd package's sessions_* tests moved to
// the cmdtest seam; the helpers here are the state-package-local ones that
// have no shared home (list-command harness plumbing and the re-exec'd
// lock-holder fixture copied from the root package's
// sessions_locks_root_fixtures_test.go).

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	locks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live/locks"
	sessscan "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view/scan"

	"github.com/PHPCraftdream/rush/internal/session"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if os.Getenv(killTestHelperProcessEnv) == "1" {
		holdTestSessionLock()
		return
	}
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	rushlog.Setup("", false)
	_ = NewSessionsStatusCmd(cmdkit.SetupApp)
	_ = NewSessionsCacheCmd(cmdkit.SetupApp)
	_ = NewSessionsWhyCmd(cmdkit.SetupApp)
	cmdtest.Main(m)
}

// sanitiseSessionIDForFilename forwards to the exported sesslocks symbol
// (the root package's test helper was a forwarder too).
func sanitiseSessionIDForFilename(id string) string { return locks.SanitiseSessionIDForFilename(id) }

// killTestHelperProcessEnv — byte-copy of the sesslocks kill_test marker for
// the re-exec'd lock-holding helper process.
const killTestHelperProcessEnv = "RUSH_SESSIONS_KILL_LOCK_HELPER"

type killTestLockHolder struct {
	cmd     *exec.Cmd
	pid     int
	stdinW  io.WriteCloser
	waitErr chan error
}

// spawnKillTestLockHolder starts a real child process that holds a session
// lock on (dataDir, sessionID) via sessionlock.TryAcquireSessionLock,
// blocking until stop() kills it. Byte-copy of the root package's fixture
// (sessions_locks_root_fixtures_test.go); see that file's reapInBackground
// doc comment for the opt-in background-reaper rationale.
func spawnKillTestLockHolder(t *testing.T, dataDir, sessionID string, reapInBackground bool) *killTestLockHolder {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := platform.Command(ctx, exe, "-test.run=^$")
	c.Env = append(os.Environ(),
		killTestHelperProcessEnv+"=1",
		"RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR="+dataDir,
		"RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID="+sessionID,
	)
	stdoutR, err := c.StdoutPipe()
	require.NoError(t, err)
	c.Stderr = nil

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	c.Stdin = stdinR

	require.NoError(t, c.Start())
	_ = stdinR.Close()

	var waitErr chan error
	if reapInBackground {
		waitErr = make(chan error, 1)
		go func() { waitErr <- c.Wait() }()
	}

	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			lineCh <- line
			return
		}
		lineCh <- ""
	}()

	select {
	case line := <-lineCh:
		require.True(t, bytes.HasPrefix([]byte(line), []byte("LOCKED")), "helper failed to lock: %s", line)
	case <-time.After(15 * time.Second):
		_ = c.Process.Kill()
		t.Fatalf("timed out waiting for kill-test helper process to report lock status")
	}

	return &killTestLockHolder{cmd: c, pid: c.Process.Pid, stdinW: stdinW, waitErr: waitErr}
}

// reapedFromWaitErr adapts the background reaper's error channel to the
// struct{}-close shape cmdtest.WaitOrDump expects.
func reapedFromWaitErr(waitErr <-chan error) <-chan struct{} {
	reaped := make(chan struct{})
	go func() {
		<-waitErr
		close(reaped)
	}()
	return reaped
}

func (h *killTestLockHolder) stop(t *testing.T) {
	if h.stdinW != nil {
		_ = h.stdinW.Close()
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	if h.waitErr != nil {
		if err := cmdtest.WaitOrDump(t, reapedFromWaitErr(h.waitErr), 30*time.Second, "background reaper goroutine of lock-holder process"); err != nil {
			t.Error(err)
		}
		return
	}
	waited := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(waited) }()
	if err := cmdtest.WaitOrDump(t, waited, 30*time.Second, "Wait() on killed lock-holder process"); err != nil {
		t.Error(err)
	}
}

// holdTestSessionLock is the re-exec entry point of
// spawnKillTestLockHolder in this test binary (the child is this very
// binary, so TestMain performs the handoff). It uses only the scoped session
// lock filesystem and does not need the package's config isolation.
func holdTestSessionLock() {
	dataDir := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_DATADIR")
	sessionID := os.Getenv("RUSH_SESSIONS_KILL_LOCK_HELPER_SESSIONID")
	if dataDir == "" || sessionID == "" {
		fmt.Println("FAILED missing-env")
		os.Exit(2)
	}
	lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		fmt.Printf("FAILED %v\n", err)
		os.Exit(1)
	}
	fmt.Println("LOCKED " + sessionID)
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = lk.Release()
}

func explainWhy(a *app.App, dataDir, sessionID string, out io.Writer) error {
	a.SetDataDirForTest(dataDir)
	return explainSessionStatus(context.Background(), a, dataDir, sessionID, out)
}

func insertChildQuestionNotice(t *testing.T, store *session.SessionNoticeStore, owner, childID, jobID, question string) {
	t.Helper()
	text := "Supervision summary.\n\nSUB-AGENT QUESTION (session " + childID + "): QUESTION: " + question +
		"\n\nThe sub-agent is paused on this question.\nAnswer: call `agent` with resume_session_id=\"" + childID + "\"."
	require.NoError(t, store.InsertSessionNotice(context.Background(), owner, session.NoticeKindChildQuestion, text, true, jobID))
}

// waitForSQLiteHandleRelease polls (bounded) for dataDir to become fully
// removable, absorbing Windows' OS-level lag in finishing a CloseHandle
// after sqlite3_close_v2() returns. Byte-copy of the models_use_test.go
// helper (the shared seam never landed in cmdtest for this one).
func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dataDir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

// ensureRootFlagStandIns registers debug/data-dir flags directly on cmd if
// not already present (idempotent), and points data-dir at dataDir. cwd is
// intentionally left unset.
func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

// isolatedListEnvWithConfiguredDataDir stands up a real app via cmdkit.SetupApp
// against a data directory deliberately NOT <cwd>/.rush, and returns the real
// `sessions list` command built through sessscan.NewSessionsListCmd so the
// end-to-end tests keep exercising the actual production command (both
// independent paths: this real-command path and the sesskit.ListStatus seam
// the wrapper-level tests use).
func isolatedListEnvWithConfiguredDataDir(t *testing.T) (a *app.App, listCmd *cobra.Command, dataDir string) {
	t.Helper()
	tmp := cmdtest.IsolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	dataDir = filepath.Join(tmp, "configured-elsewhere-data")

	ctx, cancel := context.WithCancel(context.Background())

	listCmd = sessscan.NewSessionsListCmd()
	ensureRootFlagStandIns(listCmd, dataDir)
	if f := listCmd.Flags().Lookup("json"); f == nil {
		listCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, listCmd.Flags().Set("json", "false"))
	listCmd.SetContext(ctx)

	built, err := cmdkit.SetupApp(listCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, listCmd, dataDir
}

// runSessionsCmd parses bool flags onto cmd (a freshly built production
// command), captures both output streams, and runs the real RunE with cmd as
// its own receiver — the same shape the root package's helper used.
func runSessionsCmd(t *testing.T, cmd *cobra.Command, dataDir string, boolFlags map[string]bool, args ...string) string {
	t.Helper()
	ensureRootFlagStandIns(cmd, dataDir)
	if f := cmd.Flags().Lookup("cwd"); f == nil {
		cmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, cmd.Flags().Set("cwd", ""))
	for name, val := range boolFlags {
		if val {
			require.NoError(t, cmd.Flags().Set(name, "true"))
		} else {
			require.NoError(t, cmd.Flags().Set(name, "false"))
		}
	}
	cmd.SetContext(context.Background())
	stdout, _ := cmdtest.CaptureStdoutAndStderr(t, func() { require.NoError(t, cmd.RunE(cmd, args)) })
	return stdout
}

// wakeWhyFixture is the state-package copy of the root package's
// sessions_wake_livework_test.go fixture (that file is not part of this
// lane, but the batch-facts tests here consume the same fixture shape).
func wakeWhyFixture(t *testing.T, in time.Duration) (*app.App, *sessionwake.WakeScheduleStore, string, string) {
	t.Helper()
	conn, q := cmdtest.NewSessionFactsTestDB(t)
	s := coresession.NewService(q, conn)
	m := message.NewService(q)
	wake := sessionwake.NewWakeScheduleStore(conn)

	sess, err := s.Create(context.Background(), "waiting on timer")
	require.NoError(t, err)
	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "waiting"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	row, err := wake.CreateSchedule(context.Background(), sessionwake.CreateWakeScheduleParams{
		Owner: sess.ID, Kind: sessionwake.WakeKindOnce, Message: "timer", RunAt: time.Now().Add(in),
	}, time.Now())
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetWakeScheduleStoreForTest(wake)
	return a, wake, sess.ID, row.ID
}
