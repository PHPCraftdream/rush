package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchCLIStopSubprocessAppliesOnlyNestedLeaf exercises the complete
// cross-process path: the child queues a durable stop and the installed owner
// runtime consumes it through Runner.Stop while sibling executions remain live.
func TestBatchCLIStopSubprocessAppliesOnlyNestedLeaf(t *testing.T) {
	if os.Getenv(batchCLIHelperEnv) == "1" {
		executeBatchCLIHelper()
		return
	}
	ctx := context.Background()
	fixture := seedLiveBatchCLIProcessFixture(t)

	conn, err := db.Connect(ctx, fixture.dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(conn) })
	// Keep this connection alive until the installed runner has shut down; its
	// commit hook still uses the SQLite connection during cancellation.
	queries := db.New(conn)
	_, err = queries.GetSessionByID(ctx, "batch-cli-owner")
	require.NoError(t, err)

	executor := newBatchCLIStopExecutor()
	profile, err := agent.NewBatchProfileDefault()
	require.NoError(t, err)
	install, err := agent.InstallBatchRuntime(ctx, agent.BatchInstallDeps{
		Host: fixture.host, RunnerID: "r14-test-runner", Profile: profile,
		Registries: agent.NewBatchLeafRegistryStore(), Store: session.NewBatchStore(conn),
		Messages: message.NewService(queries), Executor: executor,
		Limits:        jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		CommitTimeout: time.Second, ControlTimeout: time.Second, RecordRetention: 64,
	})
	if install != nil && install.Runner != nil {
		t.Cleanup(func() {
			executor.reportAll(jobbatch.StateCancelled)
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			require.NoError(t, install.Runner.Close(closeCtx))
		})
	}
	require.NoError(t, err)

	owner := "batch-cli-owner"
	task := func() jobbatch.Spec {
		payload, marshalErr := json.Marshal(map[string]any{"session_id": owner, "tool": "view", "tool_call_id": "cli-test", "input": json.RawMessage(`{}`)})
		require.NoError(t, marshalErr)
		return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: "generic", Payload: payload}}
	}
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: []jobbatch.Spec{
		{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Items: []jobbatch.Spec{task(), task()}}}, task(),
	}}}
	batch, err := install.SubmitCLI(ctx, owner, spec)
	require.NoError(t, err)
	root := string(batch.ID())
	awaitBatchCLIStopStarts(t, install.Runner, 3)

	target := root + ".1.2"
	result := runBatchCLIProcess(t, fixture, []string{"batch", "stop", "--json", "--wait=0", target})
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
	require.Equal(t, root, response.RootID)
	require.Equal(t, target, response.NodeID)

	batchStore := session.NewBatchStore(conn)
	controlTarget := session.BatchControlTarget{Owner: owner, Host: fixture.host, Root: root, Node: target}
	request, err := batchStore.BatchReadControl(ctx, controlTarget, response.RequestID)
	require.NoError(t, err)
	require.Equal(t, "pending", request.Status)
	require.NoError(t, install.PumpCLIControls(ctx, owner, batch.ID()))
	request, err = batchStore.BatchReadControl(ctx, controlTarget, response.RequestID)
	require.NoError(t, err)
	require.Equal(t, "answered", request.Status)
	require.Equal(t, "accepted", request.AnswerCode)

	snapshot, err := install.Runner.Snapshot(ctx, batch.ID())
	require.NoError(t, err)
	states := make(map[jobbatch.ID]jobbatch.State)
	states[snapshot.Node.ID] = snapshot.Node.State
	for _, node := range snapshot.Descendants {
		states[node.ID] = node.State
	}
	require.Equal(t, jobbatch.StateCancelling, states[jobbatch.ID(target)])
	require.Equal(t, jobbatch.StateRunning, states[jobbatch.ID(root+".1.1")])
	require.Equal(t, jobbatch.StateRunning, states[jobbatch.ID(root+".2")])
	executor.waitStop(t, target)
	require.Equal(t, 1, executor.stopCount(target))
	require.Zero(t, executor.stopCount(root+".1.1"))
	require.Zero(t, executor.stopCount(root+".2"))
}

type batchCLIStopExecutor struct {
	mu          sync.Mutex
	started     map[jobbatch.ID]chan struct{}
	stopped     map[jobbatch.ID]int
	stopCh      map[jobbatch.ID]chan struct{}
	reports     map[jobbatch.ID]func(jobbatch.Result)
	startNotify chan struct{}
	closing     bool
	closeState  jobbatch.State
}

type batchCLIStopExecution struct {
	owner *batchCLIStopExecutor
	id    jobbatch.ID
}

func (e *batchCLIStopExecution) Stop(context.Context) error {
	e.owner.mu.Lock()
	e.owner.stopped[e.id]++
	if ch := e.owner.stopCh[e.id]; ch != nil {
		close(ch)
		delete(e.owner.stopCh, e.id)
	}
	e.owner.mu.Unlock()
	return nil
}

func newBatchCLIStopExecutor() *batchCLIStopExecutor {
	return &batchCLIStopExecutor{started: make(map[jobbatch.ID]chan struct{}), stopped: make(map[jobbatch.ID]int), stopCh: make(map[jobbatch.ID]chan struct{}), reports: make(map[jobbatch.ID]func(jobbatch.Result)), startNotify: make(chan struct{}, 64)}
}

func (e *batchCLIStopExecutor) Start(_ context.Context, launch jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	e.mu.Lock()
	if e.started[launch.ID] != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("duplicate launch %s", launch.ID)
	}
	e.started[launch.ID] = make(chan struct{})
	close(e.started[launch.ID])
	select {
	case e.startNotify <- struct{}{}:
	default:
	}
	if e.closing {
		state := e.closeState
		e.mu.Unlock()
		report(jobbatch.Result{State: state})
		return &batchCLIStopExecution{owner: e, id: launch.ID}, nil
	}
	e.reports[launch.ID] = report
	e.mu.Unlock()
	return &batchCLIStopExecution{owner: e, id: launch.ID}, nil
}

func (e *batchCLIStopExecutor) stopCount(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped[jobbatch.ID(id)]
}

func (e *batchCLIStopExecutor) waitStop(t *testing.T, id string) {
	t.Helper()
	e.mu.Lock()
	key := jobbatch.ID(id)
	if e.stopped[key] > 0 {
		e.mu.Unlock()
		return
	}
	ch := e.stopCh[key]
	if ch == nil {
		ch = make(chan struct{})
		e.stopCh[key] = ch
	}
	e.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for Stop(%s)", id)
	}
}

func (e *batchCLIStopExecutor) reportAll(state jobbatch.State) {
	e.mu.Lock()
	e.closing = true
	e.closeState = state
	reports := make([]func(jobbatch.Result), 0, len(e.reports))
	for id, report := range e.reports {
		reports = append(reports, report)
		delete(e.reports, id)
	}
	e.mu.Unlock()
	for _, report := range reports {
		report(jobbatch.Result{State: state})
	}
}

func awaitBatchCLIStopStarts(t *testing.T, runner *jobbatch.Runner, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cursor := uint64(1)
	started := make(map[jobbatch.ID]struct{}, count)
	for len(started) < count {
		page, err := runner.Events(ctx, cursor)
		require.NoError(t, err)
		for _, record := range page.Records {
			if record.Type == jobbatch.RecordStarted {
				started[record.Node] = struct{}{}
			}
		}
		cursor = page.Next
	}
}

// subprocess CLI's shared-host probe permits a durable control request.
func seedLiveBatchCLIProcessFixture(t *testing.T) batchCLIAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	conn, err := db.Connect(ctx, dir)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dir, os.Getpid(), "batch-cli-runtime-acceptance")
	t.Cleanup(func() {
		if err := store.Close(context.Background()); err != nil {
			t.Errorf("close subprocess fixture store: %v", err)
		}
		if err := db.ReleaseConn(conn); err != nil {
			t.Errorf("release subprocess fixture database: %v", err)
		}
	})
	queries := db.New(conn)
	_, err = queries.CreateSession(ctx, db.CreateSessionParams{ID: "batch-cli-owner", Title: "batch CLI acceptance", Origin: "cli"})
	require.NoError(t, err)
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	return batchCLIAcceptanceFixture{dir: dir, host: host, conn: conn, store: store}
}
