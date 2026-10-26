package app

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const fenceLockHelperEnv = "RUSH_FENCE_LOCK_HELPER"

// TestFenceLockHolderHelper acquires a real session lock in a re-executed
// process. EOF on stdin releases it cleanly before the child exits.
func TestFenceLockHolderHelper(t *testing.T) {
	if os.Getenv(fenceLockHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	dataDir, sessionID := os.Getenv("RUSH_FENCE_LOCK_DATA"), os.Getenv("RUSH_FENCE_LOCK_SESSION")
	lock, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		t.Fatalf("acquire helper session lock: %v", err)
	}
	defer lock.Release()
	if _, err := io.WriteString(os.Stdout, "locked\n"); err != nil {
		t.Fatalf("signal helper lock acquired: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("wait for helper release: %v", err)
	}
}

func startFenceLockHolder(t *testing.T, dataDir, sessionID string) func() {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := platform.Command(context.Background(), exe, "-test.run=^TestFenceLockHolderHelper$")
	cmd.Env = append(os.Environ(), fenceLockHelperEnv+"=1", "RUSH_FENCE_LOCK_DATA="+dataDir, "RUSH_FENCE_LOCK_SESSION="+sessionID)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	var finishOnce sync.Once
	var waitErr error
	finish := func(kill bool) {
		finishOnce.Do(func() {
			if kill && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = stdin.Close()
			waitErr = cmd.Wait()
		})
	}
	ready := make(chan string, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
			return
		}
		ready <- ""
	}()
	t.Cleanup(func() {
		finish(true)
		<-scanDone
	})

	select {
	case line := <-ready:
		if line != "locked" {
			finish(true)
			<-scanDone
			t.Fatalf("subprocess lock-ready signal: got %q", line)
		}
	case <-time.After(30 * time.Second):
		finish(true)
		<-scanDone
		t.Fatal("subprocess did not acquire session lock")
	}
	return func() {
		finish(false)
		require.NoError(t, waitErr)
		<-scanDone
	}
}

// TestFenceAdmissionLockLoserLeavesPersistentFenceUntouched checks the real
// session lock held by a child process and verifies both provider intents lose.
// The old pre-admission fence mutation fails byte-equality assertions;
// provider traffic fails request counters and disk-write assertions.
func TestFenceAdmissionLockLoserLeavesPersistentFenceUntouched(t *testing.T) {
	application, sessionID, _, disk := runLiveProviderTurn(t, "FENCE_LOCK_MARK", func() string { return "unused" }, "")
	dataDir := application.dataDir
	require.NotEmpty(t, dataDir)
	loader := fenceLoader(t, application)
	before, err := loader.LoadSessionExecutionRequirement(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, before)
	var beforeJSON string
	require.NoError(t, application.DB().QueryRowContext(context.Background(), `SELECT call_data FROM session_run_queue WHERE id = ?`, session.SessionExecutionRequirementRowID(sessionID)).Scan(&beforeJSON))
	fenceBytes := []byte(beforeJSON)
	customDiskWrites := disk.writes.Load()
	requestsBefore := fenceLockProviderRequests.Load()
	dir := filepath.Join(application.config.WorkingDir(), "scope")
	request := func(provider tools.DiskProvider) error {
		_, runErr := application.ExecuteRun(context.Background(), RunRequest{
			Prompt: "write probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
			Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
			Overrides: RunOverrides{
				FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
				DiskProvider: provider,
			},
		})
		return runErr
	}
	stop := startFenceLockHolder(t, dataDir, sessionID)
	var busyErr *session.SessionLockBusyError
	err = request(disk)
	require.Error(t, err)
	require.ErrorAs(t, err, &busyErr)
	var afterJSON string
	require.NoError(t, application.DB().QueryRowContext(context.Background(), `SELECT call_data FROM session_run_queue WHERE id = ?`, session.SessionExecutionRequirementRowID(sessionID)).Scan(&afterJSON))
	require.Equal(t, fenceBytes, []byte(afterJSON), "the custom lock loser must preserve the fence bytes")
	require.Equal(t, requestsBefore, fenceLockProviderRequests.Load(), "the custom lock loser must not reach the model provider")
	require.Equal(t, customDiskWrites, disk.writes.Load(), "the custom lock loser must not reach disk tools")

	err = request(tools.OSDisk())
	require.Error(t, err)
	require.ErrorAs(t, err, &busyErr)
	require.NoError(t, application.DB().QueryRowContext(context.Background(), `SELECT call_data FROM session_run_queue WHERE id = ?`, session.SessionExecutionRequirementRowID(sessionID)).Scan(&afterJSON))
	require.Equal(t, fenceBytes, []byte(afterJSON), "the explicit-OS lock loser must preserve the fence bytes")
	require.Equal(t, requestsBefore, fenceLockProviderRequests.Load(), "the explicit-OS lock loser must not reach the model provider")
	require.Equal(t, customDiskWrites, disk.writes.Load(), "neither lock loser may reach disk tools")
	after, err := loader.LoadSessionExecutionRequirement(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(before, after))
	stop()

	require.NoError(t, request(tools.OSDisk()), "after child release, the explicit OS run can establish its epoch")
	cleared, err := loader.LoadSessionExecutionRequirement(context.Background(), sessionID)
	require.NoError(t, err)
	require.Nil(t, cleared)
}
