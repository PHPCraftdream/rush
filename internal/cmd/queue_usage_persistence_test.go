package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/queue"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `CREATE TABLE queue_tasks (
		id TEXT PRIMARY KEY, session_id TEXT, prompt TEXT NOT NULL, role TEXT,
		max_cost REAL, max_tokens INTEGER, timeout_sec INTEGER,
		status TEXT NOT NULL, cost REAL DEFAULT 0, tokens INTEGER DEFAULT 0,
		exit_reason TEXT, created_at INTEGER NOT NULL, started_at INTEGER,
		finished_at INTEGER, workspace_root TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	return db
}

func TestRunQueueTaskPersistsEnvelopeUsage(t *testing.T) {
	const envelope = `{"session_id":"actual-session-id","exit_reason":"stop","usage":{"delta_tokens":4,"delta_cost_usd":0.25,"session":{"total_tokens":1234,"cost_usd":0.05}}}`
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) { return []byte(envelope), nil }
	t.Cleanup(func() { queueTaskExecOverride = nil })
	task := queue.Task{ID: "queue-task", Prompt: "hello"}
	sessionID, cost, tokens, exitReason, err := runQueueTask(context.Background(), t.TempDir(), t.TempDir(), task)
	require.NoError(t, err)
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	q := queue.NewService(db)
	ctx := context.Background()
	id, err := q.Add(ctx, "", task.Prompt, "", 0, 0, 0)
	require.NoError(t, err)
	_, err = q.ClaimPending(ctx, 1)
	require.NoError(t, err)
	require.NoError(t, q.UpdateStatusWithSession(ctx, id, sessionID, queue.StatusDone, cost, tokens, exitReason))
	stored, err := q.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, queue.StatusDone, stored.Status)
	require.Equal(t, "actual-session-id", stored.SessionID)
	require.Equal(t, 0.05, stored.Cost)
	require.EqualValues(t, 1234, stored.Tokens)
	require.Equal(t, "stop", stored.ExitReason)
}

func TestQueueRunCmdRun_FailedEnvelopeStillPersistsSessionAndUsage(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	origOverride, origStdin := queueTaskExecOverride, os.Stdin
	t.Cleanup(func() { queueTaskExecOverride = origOverride; os.Stdin = origStdin })
	origCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	projectDir, dataDir := filepath.Join(tmp, "project"), filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	commands := []*cobra.Command{queueAddCmd, queueRunCmd, queueListCmd}
	for _, command := range commands {
		command.SetContext(context.Background())
		for _, name := range []string{"cwd", "data-dir", "json", "status", "concurrent", "stop-on-fail", "max-tasks"} {
			if flag := command.Flags().Lookup(name); flag != nil {
				cmdForCleanup, flagName, old := command, name, flag.Value.String()
				t.Cleanup(func() { _ = cmdForCleanup.Flags().Set(flagName, old) })
			}
		}
	}
	ensureRootFlagStandIns(queueAddCmd, dataDir)
	if queueAddCmd.Flags().Lookup("cwd") == nil {
		queueAddCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, queueAddCmd.Flags().Set("cwd", projectDir))
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, err = w.WriteString("queued task")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	os.Stdin = r
	addErr := queueAddCmd.RunE(queueAddCmd, nil)
	os.Stdin = origStdin
	require.NoError(t, r.Close())
	require.NoError(t, addErr)

	const failedEnvelope = `{"session_id":"failed-loop-session","exit_reason":"error","usage":{"delta_tokens":3,"delta_cost_usd":0.02,"session":{"total_tokens":99,"cost_usd":0.01}}}`
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) {
		return []byte(failedEnvelope), fmt.Errorf("exit status 1")
	}
	ensureRootFlagStandIns(queueRunCmd, dataDir)
	if queueRunCmd.Flags().Lookup("cwd") == nil {
		queueRunCmd.Flags().StringP("cwd", "c", "", "")
	}
	for name, value := range map[string]string{"cwd": projectDir, "concurrent": "1", "stop-on-fail": "false", "max-tasks": "0"} {
		require.NoError(t, queueRunCmd.Flags().Set(name, value))
	}
	stderr := captureStderr(t, func() { require.NoError(t, queueRunCmd.RunE(queueRunCmd, nil)) })
	require.Contains(t, stderr, "processed 1 task(s)")
	ensureRootFlagStandIns(queueListCmd, dataDir)
	if queueListCmd.Flags().Lookup("cwd") == nil {
		queueListCmd.Flags().StringP("cwd", "c", "", "")
	}
	if queueListCmd.Flags().Lookup("json") == nil {
		queueListCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, queueListCmd.Flags().Set("cwd", projectDir))
	require.NoError(t, queueListCmd.Flags().Set("json", "true"))
	stdout := captureStdout(t, func() { require.NoError(t, queueListCmd.RunE(queueListCmd, nil)) })
	var got queue.Task
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "queue list --json output:\n%s", stdout)
	assert.Equal(t, queue.StatusFailed, got.Status)
	assert.Equal(t, "failed-loop-session", got.SessionID)
	assert.Equal(t, 0.01, got.Cost)
	assert.EqualValues(t, 99, got.Tokens)
	assert.Equal(t, "error", got.ExitReason)
	waitForSQLiteHandleRelease(t, dataDir)
}

func TestRunQueueTaskFallsBackToUsageDeltaWithoutSessionSnapshot(t *testing.T) {
	const envelope = `{"session_id":"delta-session","exit_reason":"stop","usage":{"delta_tokens":12,"delta_cost_usd":0.03}}`
	queueTaskExecOverride = func(args []string, cwd, prompt string) ([]byte, error) { return []byte(envelope), nil }
	t.Cleanup(func() { queueTaskExecOverride = nil })
	sessionID, cost, tokens, exitReason, err := runQueueTask(context.Background(), t.TempDir(), t.TempDir(), queue.Task{ID: "delta-task"})
	require.NoError(t, err)
	require.Equal(t, "delta-session", sessionID)
	require.Equal(t, 0.03, cost)
	require.EqualValues(t, 12, tokens)
	require.Equal(t, "stop", exitReason)
}
