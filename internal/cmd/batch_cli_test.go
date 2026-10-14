package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const batchCLIHelperEnv = "R14_BATCH_CLI_HELPER"

type batchCLIResult struct {
	Output string
	Error  string
}

func TestBatchCLISubprocessShowInstalledDatabaseWithRuntimeOff(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	for _, target := range []string{fixture.root, fixture.root[:8]} {
		result := runBatchCLIProcess(t, fixture, []string{"batch", "show", "--json", target})
		require.Empty(t, result.Error, result.Output)
		var got batchShowTree
		require.NoError(t, json.Unmarshal([]byte(result.Output), &got), result.Output)
		require.True(t, got.Complete, "show must report a complete live tree")
		require.Equal(t, fixture.root, got.RootID, "show must resolve exact and prefix root selectors")
		require.Equal(t, "batch-cli-owner", got.OwnerSessionID, "show must expose the FK-backed owner session")
		require.Equal(t, fixture.host, got.HostID, "show must expose the registered live host")
		require.Equal(t, "r14-test-runner", got.Incarnation, "show must expose the runner incarnation")
		require.Equal(t, []batchShowNode{{NodeID: fixture.root, Depth: 0, Kind: "batch", State: "pending"}, {NodeID: fixture.root + ".1", ParentID: fixture.root, Depth: 1, Kind: "batch", State: "pending"}, {NodeID: fixture.root + ".1.1", ParentID: fixture.root + ".1", Depth: 2, Kind: "task", State: "pending"}, {NodeID: fixture.root + ".1.2", ParentID: fixture.root + ".1", Depth: 2, Kind: "task", State: "pending"}, {NodeID: fixture.root + ".2", ParentID: fixture.root, Depth: 1, Kind: "task", State: "pending"}}, got.Nodes)
	}
}

func TestBatchCLIStopSubprocessQueuesExactNestedLeaf(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	leaf := fixture.root + ".1.2"
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", "--json", "--wait=20ms", leaf})
	require.Empty(t, result.Error, result.Output)
	var response struct {
		RequestID string `json:"request_id"`
		State     string `json:"state"`
		RootID    string `json:"root_id"`
		NodeID    string `json:"node_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Output), &response), result.Output)
	require.NotEmpty(t, response.RequestID)
	require.Equal(t, "queued", response.State)
	require.Equal(t, fixture.root, response.RootID)
	require.Equal(t, leaf, response.NodeID)
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	defer db.ReleaseConn(conn)
	store := session.NewBatchStore(conn)
	request, err := store.BatchReadControl(context.Background(), session.BatchControlTarget{Owner: "batch-cli-owner", Host: fixture.host, Root: fixture.root, Node: leaf}, response.RequestID)
	require.NoError(t, err)
	require.Equal(t, leaf, request.NodeID)
	require.Equal(t, "pending", request.Status)
}

func TestBatchCLIStopRejectsMissingTargetWithoutWriting(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", "missing-prefix"})
	require.NotEmpty(t, result.Error)
	assertNoBatchControlWrites(t, fixture)
}

func TestBatchCLIStopRejectsInvalidWaitWithoutWriting(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	for _, wait := range []string{"-1ms", "31s"} {
		t.Run(wait, func(t *testing.T) {
			fixture := seedBatchCLIAcceptance(t)
			defer fixture.close(t)
			result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", "--wait=" + wait, fixture.root})
			require.NotEmpty(t, result.Error)
			assertNoBatchControlWrites(t, fixture)
		})
	}
}

func TestBatchCLIStopRejectsForeignAddWithoutWriting(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	result := runBatchCLIProcess(t, fixture, []string{"batch", "add", fixture.root})
	require.Contains(t, result.Error, "batch add is owner-process-only")
	assertNoBatchControlWrites(t, fixture)
}

func TestBatchCLIStopSubprocessRejectsStaleTerminalIncarnation(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `UPDATE batch_nodes SET state='completed' WHERE root_id=?`, fixture.root)
	require.NoError(t, err)
	defer db.ReleaseConn(conn)
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", fixture.root})
	require.Contains(t, result.Error, "batch control: stale incarnation")
	assertNoBatchControlWrites(t, fixture)
}

func TestBatchCLIStopSubprocessRejectsUnknownHostWithoutWriting(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	lockPath := session.HostLockPath(fixture.dir, fixture.host)
	if err = os.Remove(lockPath); err != nil {
		t.Skipf("cannot remove host lock: %v", err)
	}
	if err = os.Mkdir(lockPath, 0o700); err != nil {
		t.Skipf("cannot replace host lock with directory: %v", err)
	}
	defer db.ReleaseConn(conn)
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", fixture.root})
	require.Contains(t, result.Error, "batch control: unknown host")
	assertNoBatchControlWrites(t, fixture)
}

func TestBatchCLIStopSubprocessRecoversDeadHostUnannouncedRoot(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	// Retain an independent connection so the database remains available after
	// closing the fixture store releases the host lock. The reserved root has
	// not been announced, so dead-host recovery removes its whole subtree.
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	defer db.ReleaseConn(conn)
	fixture.store.Close(context.Background())
	fixture.store = nil
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", fixture.root})
	require.Contains(t, result.Error, "batch control: stale incarnation")
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM batch_control_requests`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM batch_nodes WHERE root_id=?`, fixture.root).Scan(&count))
	require.Zero(t, count, "dead-host recovery deletes unannounced root subtree")
}

func TestBatchCLIShowSubprocessIncompleteTreeRefusesTerminalShape(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	fixture := seedBatchCLIAcceptance(t)
	defer fixture.close(t)
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `DELETE FROM batch_nodes WHERE node_id=?`, fixture.root+".1.1")
	require.NoError(t, err)
	defer db.ReleaseConn(conn)
	result := runBatchCLIProcess(t, fixture, []string{"batch", "show", fixture.root})
	require.Contains(t, result.Error, "batch tree is incomplete or host status unknown")
}

type batchCLIAcceptanceFixture struct {
	dir, root, host string
	conn            *sql.DB
	store           *session.AsyncJobStore
}

func (f *batchCLIAcceptanceFixture) close(t *testing.T) {
	t.Helper()
	if f.store != nil {
		require.NoError(t, f.store.Close(context.Background()))
		f.store = nil
	}
	if f.conn != nil {
		require.NoError(t, db.ReleaseConn(f.conn))
		f.conn = nil
	}
}

func seedBatchCLIAcceptance(t *testing.T) batchCLIAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	queries := db.New(conn)
	_, err = queries.CreateSession(ctx, db.CreateSessionParams{ID: "batch-cli-owner", Title: "batch CLI acceptance", Origin: "cli"})
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dir, os.Getpid(), "batch-cli-acceptance")
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	batchStore := session.NewBatchStore(conn)
	reserved, err := batchStore.Reserve(ctx, session.BatchReservation{Owner: "batch-cli-owner", Host: host, RunnerID: "r14-test-runner", ToolCallID: "acceptance-call", InputHash: "acceptance-hash", Sequence: 1, Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "pending", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "pending", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}}, {Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}}}}, {Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}}}}})
	require.NoError(t, err)
	return batchCLIAcceptanceFixture{dir: dir, root: reserved.Snapshot.Root.RootID, host: host, conn: conn, store: store}
}

func runBatchCLIProcess(t *testing.T, fixture batchCLIAcceptanceFixture, args []string) batchCLIResult {
	t.Helper()
	args = append([]string{"--data-dir", fixture.dir, "--cwd", fixture.dir}, args...)
	exe, err := os.Executable()
	require.NoError(t, err)
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestBatchCLISubprocessShowInstalledDatabaseWithRuntimeOff$")
	cmd.Env = append(os.Environ(), batchCLIHelperEnv+"=1", "R14_BATCH_CLI_ARGS="+string(encoded), "RUSH_BATCH_RUNTIME=off", "RUSH_GLOBAL_DATA="+t.TempDir(), "RUSH_GLOBAL_CONFIG="+t.TempDir())
	output, runErr := cmd.CombinedOutput()
	var got batchCLIResult
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output), &got), "helper must emit only its result envelope: %s", output)
	if runErr != nil && got.Error == "" {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			got.Error = runErr.Error()
		} else {
			got.Error = string(output)
		}
	}
	return got
}

func assertNoBatchControlWrites(t *testing.T, fixture batchCLIAcceptanceFixture) {
	t.Helper()
	conn, err := db.Connect(context.Background(), fixture.dir)
	require.NoError(t, err)
	defer db.ReleaseConn(conn)
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM batch_control_requests`).Scan(&count))
	require.Zero(t, count)
}

func executeBatchCLIHelper() {
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("R14_BATCH_CLI_ARGS")), &args); err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(batchCLIResult{Error: err.Error()})
		os.Exit(0)
	}
	var output bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	err := rootCmd.Execute()
	result := batchCLIResult{Output: output.String()}
	if err != nil {
		result.Error = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	os.Exit(0)
}

func TestBatchSpecRejectsUnknownTrailingAndMixedFields(t *testing.T) {
	cases := []struct{ name, raw, contains string }{{"unknown", `{"mode":"parallel","unknown":1}`, "unknown field"}, {"trailing", `{"tool":"view","input":{}} {}`, "trailing JSON value"}, {"mixed", `{"tool":"view","mode":"parallel"}`, "task and batch fields cannot be mixed"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("spec", "-", "")
			cmd.SetIn(strings.NewReader(tc.raw))
			_, err := parseBatchSpec(cmd)
			require.ErrorContains(t, err, tc.contains)
		})
	}
}

func TestBatchSpecDefaultsAndNestedValidInput(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("spec", "-", "")
	cmd.SetIn(strings.NewReader(`{"items":[{"tool":"view","input":{"path":"x"}},{"mode":"sequential","items":[{"tool":"edit","input":{}}]}]}`))
	spec, err := parseBatchSpec(cmd)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Equal(t, jobbatch.ModeParallel, spec.Batch.Mode)
	require.Equal(t, jobbatch.OnFailStop, spec.Batch.OnFail)
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify)
	require.Len(t, spec.Batch.Items, 2)
	require.NotNil(t, spec.Batch.Items[1].Batch)
	require.Equal(t, jobbatch.ModeSequential, spec.Batch.Items[1].Batch.Mode)
}

func TestBatchRunRefusesWhenRuntimeOffBeforeAppSetup(t *testing.T) {
	old := os.Getenv("RUSH_BATCH_RUNTIME")
	require.NoError(t, os.Setenv("RUSH_BATCH_RUNTIME", "off"))
	t.Cleanup(func() { _ = os.Setenv("RUSH_BATCH_RUNTIME", old) })
	cmd := &cobra.Command{}
	cmd.Flags().Bool("detach", false, "")
	cmd.Flags().String("spec", "-", "")
	err := batchRun(cmd, nil)
	require.EqualError(t, err, "batch runtime is disabled; set RUSH_BATCH_RUNTIME=1")
}

func execErrorAs(err error, target any) bool { return errors.As(err, target) }

var _ io.Reader = (*bytes.Buffer)(nil)
