package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchCLIRunTextJSONAndConsecutiveRuns(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	dir := t.TempDir()
	writeBatchCLISpec(t, dir, batchRunCommandForOutput("FIRST"))
	text := runBatchCLIRunSubprocess(t, dir, false)
	require.Empty(t, text.Error)
	require.Contains(t, text.Output, "FIRST")
	require.Contains(t, text.Output, "completed")

	writeBatchCLISpec(t, dir, batchRunCommandForOutput("SECOND"))
	structured := runBatchCLIRunSubprocess(t, dir, true)
	require.Empty(t, structured.Error)
	var result struct {
		RootID string `json:"root_id"`
		State  string `json:"state"`
		Leaves []struct {
			State  string `json:"state"`
			Output string `json:"output"`
		} `json:"leaves"`
	}
	require.NoError(t, json.Unmarshal([]byte(structured.Output), &result), structured.Output)
	require.NotEmpty(t, result.RootID)
	require.Equal(t, "completed", result.State)
	require.Len(t, result.Leaves, 1)
	require.Equal(t, "completed", result.Leaves[0].State)
	require.Contains(t, result.Leaves[0].Output, "SECOND")

	show := runBatchCLIRunSubprocessArgs(t, dir, []string{"batch", "show", result.RootID})
	require.Empty(t, show.Error)
	require.Contains(t, show.Output, result.RootID)
	root := result.RootID
	persisted := runBatchCLIRunSubprocessArgs(t, dir, []string{"batch", "show", root})
	require.Empty(t, persisted.Error)
	require.Contains(t, persisted.Output, root)
	writeBatchCLISpec(t, dir, batchRunCommandForOutput("THIRD"))
	third := runBatchCLIRunSubprocess(t, dir, true)
	require.Empty(t, third.Error)
	var last struct {
		RootID string `json:"root_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(third.Output), &last))
	require.NotEqual(t, root, last.RootID)
	old := runBatchCLIRunSubprocessArgs(t, dir, []string{"batch", "show", root})
	require.Empty(t, old.Error)
	require.Contains(t, old.Output, root)
	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	var rows int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM batch_nodes WHERE root_id=?`, root).Scan(&rows))
	require.Greater(t, rows, 0, "Runner.Release disposes handles but does not delete durable batch rows")
	require.NoError(t, db.ReleaseConn(conn))
}

func TestBatchCLIRunRootDisposalAfterPurge(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	dir := t.TempDir()
	writeBatchCLISpec(t, dir, batchRunCommandForOutput("PURGE-ME"))
	run := runBatchCLIRunSubprocess(t, dir, true)
	require.Empty(t, run.Error)
	var result struct {
		RootID string `json:"root_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.Output), &result), run.Output)
	require.NotEmpty(t, result.RootID)

	conn, err := db.Connect(context.Background(), dir)
	require.NoError(t, err)
	store := session.NewBatchStore(conn)
	purged, err := store.BatchPurgeExpired(context.Background(), time.Now().Add(time.Second).Unix())
	require.NoError(t, err)
	require.Greater(t, purged, int64(0))
	require.NoError(t, db.ReleaseConn(conn))

	show := runBatchCLIRunSubprocessArgs(t, dir, []string{"batch", "show", result.RootID})
	require.Contains(t, show.Error, "not found: ")
}

func batchRunCommandSpec(program string, args []string) string {
	raw, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"tool": "run_command", "input": map[string]any{"program": program, "args": args}}}})
	return string(raw)
}

func batchRunCommandForOutput(output string) string {
	if runtime.GOOS == "windows" {
		return batchRunCommandSpec("cmd", []string{"/c", "echo", output})
	}
	return batchRunCommandSpec("sh", []string{"-c", "printf " + output})
}

func writeBatchCLISpec(t *testing.T, dir, raw string) {
	t.Helper()
	require.NoError(t, os.WriteFile(dir+string(os.PathSeparator)+"spec.json", []byte(raw), 0o600))
}

func runBatchCLIRunSubprocess(t *testing.T, dir string, asJSON bool) batchCLIResult {
	args := []string{"batch", "run", "-f", dir + string(os.PathSeparator) + "spec.json"}
	if asJSON {
		args = append(args, "--json")
	}
	return runBatchCLIRunSubprocessArgs(t, dir, args)
}

func runBatchCLIRunSubprocessArgs(t *testing.T, dir string, args []string) batchCLIResult {
	t.Helper()
	encoded, err := json.Marshal(append([]string{"--data-dir", dir, "--cwd", dir}, args...))
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestBatchCLIRunTextJSONAndConsecutiveRuns$")
	cmd.Env = append(os.Environ(), batchCLIHelperEnv+"=1", "R14_BATCH_CLI_ARGS="+string(encoded), "RUSH_GLOBAL_DATA="+t.TempDir(), "RUSH_GLOBAL_CONFIG="+t.TempDir())
	output, runErr := cmd.CombinedOutput()
	var got batchCLIResult
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output), &got), fmt.Sprintf("helper output: %s", output))
	if runErr != nil && got.Error == "" {
		got.Error = runErr.Error()
	}
	return got
}
