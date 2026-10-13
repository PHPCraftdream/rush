package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// installFakeExecutor completes every launch with a full envelope.
type installFakeExecutor struct{ content string }

func (e installFakeExecutor) Start(_ context.Context, _ jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	report(jobbatch.Result{State: jobbatch.StateCompleted, Details: batchEnvelope(fantasy.NewTextResponse(e.content))})
	return installFakeExecution{}, nil
}

type installFakeExecution struct{}

func (installFakeExecution) Stop(context.Context) error { return nil }

// newTestBatchStore builds a BatchStore on a fresh temp SQLite database with
// foreign keys off, matching the isolated ledger test convention.
func newTestBatchStore(t *testing.T) (*session.BatchStore, *sql.DB, context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	return session.NewBatchStore(conn), conn, ctx
}

func installTestDeps(t *testing.T, store *session.BatchStore, conn *sql.DB, content string) BatchInstallDeps {
	t.Helper()
	profile, err := NewBatchProfile(BatchProfileCaps{IO: 4, CLIDriver: 4, AgentDriver: 4, Default: 4, Global: 16})
	require.NoError(t, err)
	return BatchInstallDeps{
		Host: "host", RunnerID: "runner-1", Owner: "owner",
		Profile: profile, Registries: NewBatchLeafRegistryStore(),
		Store: store, Messages: message.NewService(db.New(conn)),
		Executor: installFakeExecutor{content: content}, DonePoll: time.Millisecond,
	}
}

func installOneItemSpec(t *testing.T, content string) jobbatch.Spec {
	payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-1", SessionID: "owner", Input: json.RawMessage(`{"path":"x"}`), RegistryID: "reg-1"})
	require.NoError(t, err)
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}
}

func installSubmit(t *testing.T, install *BatchInstall, spec jobbatch.Spec) jobbatch.ID {
	t.Helper()
	root := jobbatch.ID(uuid.NewString())
	got, err := install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: "call-1"})
	require.NoError(t, err)
	require.Equal(t, root, got, "runner root equals requested root")
	return root
}

func TestBatchRunnerAdapterSubmitsOneItemAndSettles(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	spec := installOneItemSpec(t, "")
	root := installSubmit(t, install, spec)
	require.NotNil(t, install.RunnerHandle.batchFor(root), "the started root is registered")
	resp, ok, err := install.Done.AwaitDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ok", resp.Content)
	require.False(t, resp.IsError)
}

// A probe whose hash is the canonical one-item TASK spec hash attaches to the
// live root the engine Submit reserved via the commit hook.
func TestBatchAttachAdapterProbesLiveRootWithFences(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	spec := installOneItemSpec(t, "")
	root := installSubmit(t, install, spec)
	hash := batchInvocationHash(spec)
	got, attached, err := install.Attach.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: hash})
	require.NoError(t, err)
	require.True(t, attached)
	require.Equal(t, root, got)
	for name, p := range map[string]batchAttachProbe{
		"hash mismatch":   {Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: batchInvocationHash(installOneItemSpec(t, "other"))},
		"foreign host":    {Owner: "owner", Host: "elsewhere", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: hash},
		"runner mismatch": {Owner: "owner", Host: "host", RunnerID: "runner-2", ToolCallID: "call-1", InputHash: hash},
	} {
		_, attached, err := install.Attach.ProbeLive(ctx, p)
		require.Error(t, err, name, "mismatch is an error, not a resubmission")
		require.False(t, attached, name)
	}
}

// LoadDone reads a persisted completion without waiting; a settled row
// without envelope details is projected from its stored summary, never
// invented.
func TestBatchStoreDoneSourceLoadDoneProjection(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	// Unsettled: no completion is reported.
	_, ok, err := install.Done.LoadDone(ctx, "owner", "root-none")
	require.NoError(t, err)
	require.False(t, ok)
	root := installSubmit(t, install, installOneItemSpec(t, ""))
	_, ok, err = install.Done.LoadDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "settled root reports its envelope")
}

func TestBatchStoreDoneSourceForeignRootWaitsDurably(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, ok, err := install.Done.AwaitDone(pollCtx, "owner", "root-foreign")
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, ok)
	}()
	// The poll cadence keeps the wait alive until the caller goes away.
	select {
	case <-done:
		cancel()
		require.FailNow(t, "foreign root must wait durably")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-done
}

// Each election commits exactly once: an unannounced settled root elects its
// inline completion; a started ack elects the started history. The two
// elections exclude each other (inline requires an unannounced root), so
// they run on separate roots.
func TestBatchStoreElectorCASExactlyOneWinner(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	root := installSubmit(t, install, installOneItemSpec(t, ""))
	won, err := install.Elector.ElectStarted(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, won, "first started election wins")
	won, err = install.Elector.ElectStarted(ctx, "owner", root)
	require.NoError(t, err)
	require.False(t, won, "second started election loses without error")

	inlineRoot := installSubmit(t, install, installOneItemSpec(t, ""))
	resp, ok, err := install.Done.AwaitDone(ctx, "owner", inlineRoot)
	require.NoError(t, err)
	require.True(t, ok)
	won, err = install.Elector.ElectInline(ctx, "owner", inlineRoot, resp)
	require.NoError(t, err)
	require.True(t, won, "first inline election wins")
	won, err = install.Elector.ElectInline(ctx, "owner", inlineRoot, resp)
	require.NoError(t, err)
	require.False(t, won, "second inline election loses without error")
}

func TestBatchStoreNoticePullSelectedOnly(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install, err := InstallBatchRuntime(ctx, installTestDeps(t, store, conn, "ok"))
	require.NoError(t, err)
	root := installSubmit(t, install, installOneItemSpec(t, ""))
	_, ok, err := install.Done.AwaitDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
	// Nothing announced yet: nothing is pullable, and Pull returns promptly.
	n, err := install.NoticePull.Pull(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	won, err := install.Elector.ElectStarted(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, won)
	_, ok, err = install.Done.AwaitDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
	n, err = install.NoticePull.Pull(ctx)
	require.NoError(t, err)
	require.Positive(t, n, "the selected completion notice is pulled")
	// Drained: a second pull is empty and never waits for running work.
	n, err = install.NoticePull.Pull(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestBatchLeafRegistryStoreFailsClosed(t *testing.T) {
	t.Parallel()
	s := NewBatchLeafRegistryStore()
	_, err := s.Resolve("unknown")
	require.Error(t, err, "unknown identity fails closed")
	require.Error(t, s.Register("", nil), "empty identity is refused")
	require.Error(t, s.Register("reg-1", nil), "nil registry is refused")
	registry := newPolicyToolBuild(nil).leaves
	require.NoError(t, s.Register("reg-1", registry))
	got, err := s.Resolve("reg-1")
	require.NoError(t, err)
	require.Same(t, registry, got)
	require.Error(t, s.Register("reg-1", registry), "pinned-once: no replacement")
}

func TestInstallBatchRuntimeFailsClosed(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	deps := installTestDeps(t, store, conn, "ok")
	for name, mutate := range map[string]func(*BatchInstallDeps){
		"missing host":     func(d *BatchInstallDeps) { d.Host = "" },
		"missing runner":   func(d *BatchInstallDeps) { d.RunnerID = "" },
		"missing owner":    func(d *BatchInstallDeps) { d.Owner = "" },
		"missing profile":  func(d *BatchInstallDeps) { d.Profile = nil },
		"missing registry": func(d *BatchInstallDeps) { d.Registries = nil },
		"missing store":    func(d *BatchInstallDeps) { d.Store = nil },
		"missing messages": func(d *BatchInstallDeps) { d.Messages = nil },
	} {
		broken := deps
		mutate(&broken)
		_, err := InstallBatchRuntime(ctx, broken)
		require.Error(t, err, name)
	}
}

func TestNewBatchExecutorFamiliesRegistersAllKinds(t *testing.T) {
	t.Parallel()
	families := newBatchExecutorFamilies(batchAgentDeps{})
	for _, kind := range []string{batchKindGeneric, batchKindMCP, batchKindShell, batchKindArgv, batchKindAgent} {
		require.Contains(t, families, kind, kind)
	}
	require.Same(t, families[batchKindGeneric], families[batchKindMCP], "MCP leaves run through the pinned generic leaf path")
}
