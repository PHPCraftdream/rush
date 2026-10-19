package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
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

// gatingInstallExecutor keeps the one-item batch LIVE (the node stays
// running, so the reserved root stays a live batch root) until the test
// releases it — the same gating pattern as gatingLeaf. Without it the
// instant completion settles the root asynchronously and the fence probes
// race the settlement: once the root is Final, GetLiveBatchRoot returns no
// rows and every probe legitimately reports attached=false, err=nil.
type gatingInstallExecutor struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	content string
}

type gatingInstallExecution struct{ content string }

func (e gatingInstallExecution) Stop(context.Context) error { return nil }
func (e gatingInstallExecution) ReadOutput(context.Context, uint64) (jobbatch.OutputChunk, error) {
	return jobbatch.OutputChunk{Data: []byte(e.content), Next: uint64(len(e.content)), EOF: true}, nil
}

func newGatingInstallExecutor(content string) *gatingInstallExecutor {
	return &gatingInstallExecutor{release: make(chan struct{}), content: content}
}

func (e *gatingInstallExecutor) Start(ctx context.Context, _ jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if e.started != nil {
		e.started <- struct{}{}
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return gatingInstallExecution{}, ctx.Err()
	}
	report(jobbatch.Result{State: jobbatch.StateCompleted, Details: batchEnvelope(fantasy.NewTextResponse(e.content))})
	return gatingInstallExecution{content: e.content}, nil
}

func (e *gatingInstallExecutor) letGo() { e.once.Do(func() { close(e.release) }) }

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
		DiskBindings: NewDiskProviderRegistry(),
		Store:        store, Messages: message.NewService(db.New(conn)),
		Executor: installFakeExecutor{content: content}, DonePoll: time.Millisecond,
		// R11F round-2 (P1-7 item 6): jobbatch.NewRunner fails closed on zero
		// budgets AND on zero limits (engine.go validate), so every adapter
		// test needs positive limits and budgets or it aborts before its
		// assertions.
		Limits:        jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		CommitTimeout: time.Second, ControlTimeout: time.Second, RecordRetention: 64,
	}
}

func installOneItemSpec(t *testing.T, content string) jobbatch.Spec {
	// The content rides in the canonical input so two specs with different
	// contents carry different hashes (the mismatch probes rely on that).
	payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-1", SessionID: "owner", Input: json.RawMessage(`{"path":"` + content + `"}`), RegistryID: "reg-1"})
	require.NoError(t, err)
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}
}

func installSubmit(t *testing.T, install *BatchInstall, spec jobbatch.Spec) jobbatch.ID {
	return installSubmitAs(t, install, spec, "call-1")
}

func installSubmitAs(t *testing.T, install *BatchInstall, spec jobbatch.Spec, ref string) jobbatch.ID {
	t.Helper()
	root := jobbatch.ID(uuid.NewString())
	got, err := install.RunnerHandle.Submit(context.Background(), jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: ref})
	require.NoError(t, err)
	require.Equal(t, root, got, "runner root equals requested root")
	return root
}

// installRunnerForTest installs the real Runner and registers its cleanup:
// the checked Close drains the Runner; Stop alone does not.
func installRunnerForTest(t *testing.T, ctx context.Context, deps BatchInstallDeps) *BatchInstall {
	t.Helper()
	install, err := InstallBatchRuntime(ctx, deps)
	require.NoError(t, err)
	if install.Runner != nil {
		t.Cleanup(func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, install.Runner.Close(closeCtx))
		})
	}
	return install
}

func TestBatchRunnerAdapterPreservesExplicitNestedBatch(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
	explicit := jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyEach, MaxParallel: 3,
		Items: []jobbatch.Spec{{Batch: &jobbatch.BatchSpec{
			Mode: jobbatch.ModeSequential, MaxParallel: 1,
			Items: []jobbatch.Spec{installOneItemSpec(t, "")},
		}}},
	}}
	root := installSubmit(t, install, explicit)
	view, err := install.RunnerHandle.Snapshot(ctx, root)
	require.NoError(t, err)
	require.Equal(t, jobbatch.ModeParallel, view.Node.Mode)
	require.Equal(t, jobbatch.NotifyEach, view.Node.Notify)
	require.Equal(t, 3, view.Node.MaxParallel)
	require.Len(t, view.Descendants, 2)
	require.Equal(t, jobbatch.KindBatch, view.Descendants[0].Kind)
	require.Equal(t, jobbatch.ModeSequential, view.Descendants[0].Mode)
	require.Equal(t, 1, view.Descendants[0].MaxParallel)
}

func TestBatchRunnerAdapterRejectsMalformedSpecShapes(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
	for _, spec := range []jobbatch.Spec{
		{},
		{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric}, Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel}},
	} {
		_, err := install.RunnerHandle.Submit(ctx, jobbatch.SubmitRequest{Root: jobbatch.ID(uuid.NewString()), Spec: spec})
		require.ErrorIs(t, err, jobbatch.ErrInvalidSpec)
	}
}

func TestBatchRunnerAdapterSubmitsOneItemAndSettles(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
	spec := installOneItemSpec(t, "")
	root := installSubmit(t, install, spec)
	require.NotNil(t, install.RunnerHandle.batchFor(root), "the started root is registered")
	view, err := install.RunnerHandle.Snapshot(ctx, root)
	require.NoError(t, err)
	rootView, err := install.RunnerHandle.View(ctx, root)
	require.NoError(t, err)
	require.Equal(t, jobbatch.KindBatch, rootView.Kind, "ordinary one-item root is a batch node, distinct from root notices")
	require.Equal(t, jobbatch.NotifyAll, rootView.Notify, "direct task submissions use a NotifyAll root batch")
	require.Len(t, view.Descendants, 1)
	require.Equal(t, jobbatch.KindTask, view.Descendants[0].Kind)
	storeView, err := store.Read(ctx, "owner", string(root))
	require.NoError(t, err)
	require.Len(t, storeView.Nodes, 2)
	var gotPayload batchLeafPayload
	require.True(t, storeView.Nodes[1].TaskPayload.Valid)
	require.NoError(t, json.Unmarshal([]byte(storeView.Nodes[1].TaskPayload.String), &gotPayload))
	var wantPayload batchLeafPayload
	require.NoError(t, json.Unmarshal(spec.Task.Payload, &wantPayload))
	require.Equal(t, wantPayload, gotPayload, "task payload is preserved in the wrapped root")
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
	deps := installTestDeps(t, store, conn, "ok")
	// A GATED executor keeps the root genuinely live while every probe runs:
	// the engine settles the root asynchronously with the instant fake, so
	// the fence probes below would otherwise race the settlement and read
	// no live root at all (GetLiveBatchRoot: attached=false, err=nil).
	gate := newGatingInstallExecutor("ok")
	deps.Executor = gate
	install := installRunnerForTest(t, ctx, deps)
	// Registered AFTER the Close cleanup, so LIFO runs letGo first and the
	// gate is released before the Runner closes.
	t.Cleanup(gate.letGo)
	spec := installOneItemSpec(t, "")
	root := installSubmit(t, install, spec)
	hash := batchInvocationHash(spec)
	got, attached, err := install.Attach.ProbeLive(ctx, batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: hash})
	require.NoError(t, err)
	require.True(t, attached)
	require.Equal(t, root, got)
	for name, tc := range map[string]struct {
		probe batchAttachProbe
		want  error
	}{
		"hash mismatch":   {probe: batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: batchInvocationHash(installOneItemSpec(t, "other"))}, want: session.ErrBatchInputMismatch},
		"foreign host":    {probe: batchAttachProbe{Owner: "owner", Host: "elsewhere", RunnerID: "runner-1", ToolCallID: "call-1", InputHash: hash}, want: session.ErrBatchForeignLiveHost},
		"runner mismatch": {probe: batchAttachProbe{Owner: "owner", Host: "host", RunnerID: "runner-2", ToolCallID: "call-1", InputHash: hash}, want: session.ErrBatchRunnerMismatch},
	} {
		_, attached, err := install.Attach.ProbeLive(ctx, tc.probe)
		require.Error(t, err, name+": mismatch is an error, not a resubmission")
		require.ErrorIs(t, err, tc.want, name)
		require.False(t, attached, name)
	}
	// Release the gate: the root settles cleanly and the runner closes
	// without leaving engine goroutines behind.
	gate.letGo()
	_, ok, err := install.Done.AwaitDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
}

// LoadDone reads a persisted completion without waiting; a settled row
// without envelope details is projected from its stored summary, never
// invented.
func TestBatchStoreDoneSourceLoadDoneProjection(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
	// Unsettled/unknown root: no completion is reported (a missing root row
	// maps to ok=false, never sql.ErrNoRows — see
	// BatchDeliveryOneItemCompletion in internal/session/batch_adapters.go).
	_, ok, err := install.Done.LoadDone(ctx, "owner", "root-none")
	require.NoError(t, err)
	require.False(t, ok)
	root := installSubmit(t, install, installOneItemSpec(t, ""))
	// Submit acknowledgement does not await settlement (the runner applies
	// EventSettled asynchronously), so wait for the REAL completion first —
	// only then can LoadDone find the persisted envelope.
	resp, ok, err := install.Done.AwaitDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok)
	got, ok, err := install.Done.LoadDone(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, ok, "settled root reports its envelope")
	require.Equal(t, resp, got, "LoadDone projects the same settled envelope")
}

func TestBatchStoreDoneSourceForeignRootWaitsDurably(t *testing.T) {
	t.Parallel()
	store, conn, ctx := newTestBatchStore(t)
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
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
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
	// Distinct call identities (Ref and InputHash) for the two submissions:
	// the first root may still be live during the second Submit's
	// reservation, and a duplicated identity would conflict with the second
	// requested engine root instead of electing a fresh one.
	root := installSubmitAs(t, install, installOneItemSpec(t, "started"), "call-started")
	won, err := install.Elector.ElectStarted(ctx, "owner", root)
	require.NoError(t, err)
	require.True(t, won, "first started election wins")
	won, err = install.Elector.ElectStarted(ctx, "owner", root)
	require.NoError(t, err)
	require.False(t, won, "second started election loses without error")

	inlineRoot := installSubmitAs(t, install, installOneItemSpec(t, "inline"), "call-inline")
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
	install := installRunnerForTest(t, ctx, installTestDeps(t, store, conn, "ok"))
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
		"missing host":   func(d *BatchInstallDeps) { d.Host = "" },
		"missing runner": func(d *BatchInstallDeps) { d.RunnerID = "" },
		// R11F P1-1: a missing Owner is NO LONGER an error — the dormant
		// default owner (batchDormantInstallOwner) applies and the commit
		// hook resolves per-invocation owners, so that case is gone here.
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
	require.Equal(t, families[batchKindGeneric], families[batchKindMCP], "MCP leaves run through the pinned generic leaf path")
}
