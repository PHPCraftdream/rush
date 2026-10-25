package agent

// ROUND4 FINAL task #1462 — timeout-regression net for the call-credential
// stamping launch path (batch_credentials_marker.go stampBatchCallCredentials).
//
// Production entry points exercised:
//   - stampBatchCallCredentials directly (unit tests): the Round 3 fix copies
//     the WHOLE TaskSpec (task := *spec.Task) and replaces only Payload, so
//     Timeout — and any future TaskSpec field — must survive stamping.
//   - installedBatchModelControl.Run (batch_model_control.go) over the REAL
//     install (InstallBatchRuntime, real jobbatch engine, real
//     batchRunnerAdapter.submitBatch stamping + escrow registration) for the
//     end-to-end batch_run "agent"-item path: the model-visible timeout
//     (batch_model_tools.go modelBatchSpecAt → TaskSpec.Timeout) must reach
//     the executor's launch untouched, with the marker set and the secret
//     escrowed (never serialized into the payload).
//
// The single mutation each test catches is stated at the test site. The
// headline regression this file pins: a stamping implementation that rebuilt
// the TaskSpec from scratch (dropping Timeout) is caught by the reflect-based
// field loop (unit) and by the executor-observed launch Timeout (e2e).

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// markerCaptureSpec is the batch_run raw spec under test: ONE agent
// item with an explicit timeout.
func markerCaptureSpec() batchModelSpec {
	return batchModelSpec{
		Mode: "parallel",
		Items: []batchModelItem{{
			Tool:    "agent",
			Input:   json.RawMessage(`{"prompt":"work"}`),
			Timeout: "90s",
		}},
	}
}

func markerCaptureInvocation(env *markerModelControlEnv) BatchInvocation {
	return BatchInvocation{
		SessionID: env.sessionID, MessageID: "message", RegistryID: env.registryID,
		Origin: "coordinator", CallID: "call-mc",
		WaiterRank: batchGroupRanks[batchGroupAgentDriver],
		Delivery:   batchDeliveryChoice{Mode: batchDeliveryDetached},
		Policy: batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
			MessageID: "message",
		},
	}
}

// markerCaptureWaitEscrowRootGone polls until NO escrow entry names the root
// anymore. The Done watcher installed by Run (batchRootLeaseStore.adopt →
// batchRunnerAdapter.Release → releaseRoot) runs on its own goroutine; this
// bounded poll joins it deterministically on the escrow's own observable
// state, so no goroutine outlives the test (and later the DB release).
func markerCaptureWaitEscrowRootGone(t *testing.T, escrow *batchCredentialEscrow, root jobbatch.ID) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		escrow.mu.Lock()
		found := false
		for _, entry := range escrow.entries {
			if entry.root == string(root) {
				found = true
				break
			}
		}
		escrow.mu.Unlock()
		if !found {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("escrow entries for root %q were never released", root)
		}
	}
}

// markerCaptureExecutor is the executor seam of the e2e fixture: the REAL
// engine, admission, commit hook and runner-adapter stamping/escrow all run;
// only the agent driver is replaced by a capture-and-park stand-in so the
// launch (Task.Timeout, payload marker) and the escrow entry are observed
// deterministically, before any consumer can race the escrow.
type markerCaptureExecutor struct {
	release  chan struct{}
	captured chan struct{}
	wg       sync.WaitGroup

	mu       sync.Mutex
	launches []jobbatch.Launch
}

func newMarkerCaptureExecutor() *markerCaptureExecutor {
	return &markerCaptureExecutor{release: make(chan struct{}), captured: make(chan struct{}, 1)}
}

func (e *markerCaptureExecutor) Start(_ context.Context, launch jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	captured := launch
	captured.Task.Payload = append(json.RawMessage(nil), launch.Task.Payload...)
	e.mu.Lock()
	e.launches = append(e.launches, captured)
	e.mu.Unlock()
	select {
	case e.captured <- struct{}{}:
	default:
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		<-e.release
		report(jobbatch.Result{State: jobbatch.StateCompleted, Details: batchEnvelope(fantasy.NewTextResponse("done"))})
	}()
	return markerCaptureExecution{}, nil
}

func (e *markerCaptureExecutor) snapshotLaunches() []jobbatch.Launch {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]jobbatch.Launch(nil), e.launches...)
}

func (e *markerCaptureExecutor) letGo() {
	select {
	case <-e.release:
	default:
		close(e.release)
	}
}

func (e *markerCaptureExecutor) waitCaptured(t *testing.T) {
	t.Helper()
	select {
	case <-e.captured:
	case <-time.After(5 * time.Second):
		t.Fatal("the executor never captured the agent launch")
	}
}

type markerCaptureExecution struct{}

func (markerCaptureExecution) Stop(context.Context) error { return nil }

// markerModelControlEnv is one real install whose pinned "agent" leaf is the
// escrow-wrapped escrowTestTool, driven through the REAL
// installedBatchModelControl.Run. The executor parks each launch so the test
// observes the stamped spec and the escrow entry without racing settlement.
type markerModelControlEnv struct {
	t           *testing.T
	install     *BatchInstall
	control     BatchModelControl
	executor    *markerCaptureExecutor
	registries  *BatchLeafRegistryStore
	registryID  string
	fingerprint string
	sessionID   string
}

const markerModelRegistryID = "marker-model-reg"

func newMarkerModelControlEnv(t *testing.T) *markerModelControlEnv {
	t.Helper()
	store, conn, _ := newTestBatchStore(t)
	tool := &escrowTestTool{started: make(chan context.Context, 1), release: make(chan struct{})}
	tools := []fantasy.AgentTool{tool}
	fingerprint := batchToolsetFingerprint(tools)
	executor := newMarkerCaptureExecutor()
	deps := installTestDeps(t, store, conn, "unused")
	deps.Executor = executor
	registries := deps.Registries
	require.NoError(t, registries.Register(markerModelRegistryID, newPolicyToolBuild(tools).leaves))
	pinTestConstruction(t, deps.DiskBindings, markerModelRegistryID)
	install, err := InstallBatchRuntime(context.Background(), deps)
	require.NoError(t, err)
	// LIFO teardown: (1) registered first → runs SECOND: runner close + join
	// of every executor goroutine; (2) registered last → runs FIRST: unblock
	// the parked execution so Close never waits behind a parked launch. Both
	// run BEFORE the DB release registered by newTestBatchStore.
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, install.Runner.Close(closeCtx))
		executor.wg.Wait()
	})
	t.Cleanup(executor.letGo)
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	// The REAL installed control over the real runner/delivery/root-lease
	// seams; sessions stays nil (only Status/Output reads need it).
	control := NewInstalledBatchModelControl(
		install.RunnerHandle, install.Delivery, store, nil, nil,
		install.Runtime.cfg.Host, install.Runtime.cfg.RunnerID,
		install.Runtime.cfg.Profile, nil, install.RootLeases,
	)
	return &markerModelControlEnv{
		t: t, install: install, control: control, executor: executor,
		registries: registries, registryID: markerModelRegistryID,
		fingerprint: fingerprint, sessionID: "session",
	}
}

// TestStampBatchCallCredentialsPreservesEveryTaskSpecField pins the Round 3
// fix at the unit level: stamping copies the WHOLE TaskSpec and replaces ONLY
// Payload. The reflect loop over jobbatch.TaskSpec fields makes any FUTURE
// field (not just Timeout) automatically covered.
//
// Mutation caught: a stamping path that rebuilds TaskSpec{Kind, Payload}
// from scratch — silently dropping Timeout (the Round 3 regression) or any
// other non-Payload field — fails the field loop.
func TestStampBatchCallCredentialsPreservesEveryTaskSpecField(t *testing.T) {
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-stamp", SessionID: "session", RegistryID: "registry",
		Input: json.RawMessage(`{"prompt":"work"}`),
	})
	require.NoError(t, err)
	spec := jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind: batchKindAgent, Group: "some-group", Payload: payload, Timeout: 90 * time.Second,
	}}
	stamped, err := stampBatchCallCredentials(
		withCallCredentials(context.Background(), escrowTestCreds("topsecret")), spec)
	require.NoError(t, err)
	require.NotNil(t, stamped.Task)
	original := *spec.Task
	stampedTask := *stamped.Task
	taskType := reflect.TypeOf(original)
	for i := 0; i < taskType.NumField(); i++ {
		name := taskType.Field(i).Name
		if name == "Payload" {
			continue
		}
		require.True(t,
			reflect.DeepEqual(reflect.ValueOf(original).Field(i).Interface(), reflect.ValueOf(stampedTask).Field(i).Interface()),
			"TaskSpec field %s must survive credential stamping", name)
	}
	var marked batchLeafPayload
	require.NoError(t, json.Unmarshal(stamped.Task.Payload, &marked))
	require.True(t, marked.CallCredentials, "the stamped payload carries the call-credentials marker")
	require.NotContains(t, string(stamped.Task.Payload), "topsecret", "no secret may be serialized into the payload")
}

// TestStampBatchCallCredentialsPreservesBatchSpec pins the nested-batch
// stamping path: batch-level Mode/OnFail/Notify/MaxParallel survive, the
// item slice is copied (the original batch is never aliased), item Timeouts
// survive, and markers are set ONLY on agent leaves.
//
// Mutation caught: a batch stamping path that rebuilt the BatchSpec header
// (dropping OnFail/Notify/MaxParallel) or that stamped generic (non-agent)
// leaves as credential-carrying.
func TestStampBatchCallCredentialsPreservesBatchSpec(t *testing.T) {
	agentPayload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-nested-agent", SessionID: "session", RegistryID: "registry",
		Input: json.RawMessage(`{"prompt":"work"}`),
	})
	require.NoError(t, err)
	genericPayload, err := json.Marshal(batchLeafPayload{
		Tool: "view", ToolCallID: "call-nested-view", SessionID: "session", RegistryID: "registry",
		Input: json.RawMessage(`{"path":"x"}`),
	})
	require.NoError(t, err)
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode: jobbatch.ModeSequential, OnFail: jobbatch.OnFailContinue, Notify: jobbatch.NotifyEach,
		MaxParallel: 3,
		Items: []jobbatch.Spec{
			{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: agentPayload, Timeout: 90 * time.Second}},
			{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: genericPayload, Timeout: 5 * time.Second}},
		},
	}}
	stamped, err := stampBatchCallCredentials(
		withCallCredentials(context.Background(), escrowTestCreds("topsecret")), spec)
	require.NoError(t, err)
	require.NotNil(t, stamped.Batch)
	original := *spec.Batch
	stampedBatch := *stamped.Batch
	batchType := reflect.TypeOf(original)
	for i := 0; i < batchType.NumField(); i++ {
		name := batchType.Field(i).Name
		if name == "Items" {
			continue
		}
		require.True(t,
			reflect.DeepEqual(reflect.ValueOf(original).Field(i).Interface(), reflect.ValueOf(stampedBatch).Field(i).Interface()),
			"BatchSpec field %s must survive credential stamping", name)
	}
	require.Len(t, stamped.Batch.Items, 2)
	var agentLeaf batchLeafPayload
	require.NoError(t, json.Unmarshal(stamped.Batch.Items[0].Task.Payload, &agentLeaf))
	require.True(t, agentLeaf.CallCredentials, "the agent leaf is marked")
	require.Equal(t, 90*time.Second, stamped.Batch.Items[0].Task.Timeout, "agent item timeout survives")
	var genericLeaf batchLeafPayload
	require.NoError(t, json.Unmarshal(stamped.Batch.Items[1].Task.Payload, &genericLeaf))
	require.False(t, genericLeaf.CallCredentials, "the generic leaf stays unmarked")
	require.NotContains(t, string(stamped.Batch.Items[1].Task.Payload), "call_credentials")
	require.Equal(t, 5*time.Second, stamped.Batch.Items[1].Task.Timeout, "generic item timeout survives")
}

// TestStampBatchCallCredentialsRequiresCredentialsForMarkedPayload pins the
// fail-closed half of the marker: a payload pre-marked call_credentials:true
// cannot be submitted WITHOUT launch credentials, and an unmarked payload
// stays byte-unmarked when no credentials are present.
//
// Mutation caught: a stamping path that marked payloads unconditionally (or
// dropped the require-credentials guard) would let an uncredentialed host
// context smuggle the marker into the durable payload.
func TestStampBatchCallCredentialsRequiresCredentialsForMarkedPayload(t *testing.T) {
	marked, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-marked", SessionID: "session", RegistryID: "registry",
		CallCredentials: true, Input: json.RawMessage(`{"prompt":"work"}`),
	})
	require.NoError(t, err)
	_, err = stampBatchCallCredentials(context.Background(), jobbatch.Spec{
		Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: marked, Timeout: 90 * time.Second},
	})
	require.ErrorIs(t, err, errBatchCallCredentialsRequired)
	unmarked, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-unmarked", SessionID: "session", RegistryID: "registry",
		Input: json.RawMessage(`{"prompt":"work"}`),
	})
	require.NoError(t, err)
	stamped, err := stampBatchCallCredentials(context.Background(), jobbatch.Spec{
		Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: unmarked, Timeout: 90 * time.Second},
	})
	require.NoError(t, err)
	require.NotContains(t, string(stamped.Task.Payload), "call_credentials", "an unmarked payload stays unmarked without credentials")
	require.Equal(t, 90*time.Second, stamped.Task.Timeout)
}

// TestModelControlRunAgentLeafTimeoutReachesExecutor is the end-to-end
// timeout net: the REAL installedBatchModelControl.Run over the REAL install
// builds the batch_run spec (batch_model_tools.go modelBatchSpecAt: the
// model-visible timeout string becomes TaskSpec.Timeout), admission and the
// real engine accept it, and batchRunnerAdapter.submitBatch stamps the
// marker — the executor's launch must still carry the requested Timeout.
//
// First behavioral assertion: the captured launch's Task.Timeout.
//
// Mutation caught: a stamping path that rebuilt the TaskSpec and dropped
// Timeout would reach the executor with a zero Timeout even though the model
// requested 90s (the exact production defect the Round 3 fix removed).
func TestModelControlRunAgentLeafTimeoutReachesExecutor(t *testing.T) {
	env := newMarkerModelControlEnv(t)
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	ctx := withCallCredentials(context.Background(), escrowTestCreds("topsecret"))
	root, err := env.control.Run(ctx, markerCaptureInvocation(env), reg, markerCaptureSpec())
	require.NoError(t, err)
	require.NotEmpty(t, root)
	env.executor.waitCaptured(t)
	launches := env.executor.snapshotLaunches()
	require.Len(t, launches, 1)
	require.Equal(t, batchKindAgent, launches[0].Task.Kind)
	require.Equal(t, 90*time.Second, launches[0].Task.Timeout, "the model-requested timeout must reach the executor untouched")
	var payload batchLeafPayload
	require.NoError(t, json.Unmarshal(launches[0].Task.Payload, &payload))
	require.True(t, payload.CallCredentials, "the stamped payload carries the marker")
	require.NotContains(t, string(launches[0].Task.Payload), "topsecret", "the secret never rides in the payload")
	// The escrow entry was registered by the adapter BEFORE the engine ran:
	// the test consumes it like the agent driver would, then releases.
	creds, complete, err := env.install.RunnerHandle.credentials.consume(batchCredentialKey{
		registryID: env.registryID, sessionID: env.sessionID, messageID: "message",
		callID: "call-mc-run-0", tool: "agent",
	}, string(root))
	require.NoError(t, err, "the adapter registered the launch's escrow entry")
	require.Equal(t, "topsecret", creds.Credentials[0].APIKey)
	complete()
	// Settlement and full release: unblock the parked execution (cleanup runs
	// this first on failure too), wait for the real Done, then join the
	// adopt-installed release watcher on the escrow's observable state.
	env.executor.letGo()
	batch := env.install.RunnerHandle.batchFor(root)
	require.NotNil(t, batch)
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = batch.Wait(waitCtx)
	require.NoError(t, err)
	markerCaptureWaitEscrowRootGone(t, env.install.RunnerHandle.credentials, root)
}

// TestModelControlRunAgentLeafEscrowAudit is the corrected launch-path audit
// for batch_run agent items: with call credentials in ctx, the SAME
// installedBatchModelControl.Run path (1) marks the payload the executor
// sees, (2) registers the escrow entry in the install's registry-store
// escrow (the very instance the adapter stamps through), (3) clones the
// credentials at registration time (later caller-side mutation cannot
// retrofit the entry), and (4) preserves the requested Timeout.
//
// Mutation caught: an audit claimed the batch_run agent path never marked
// payloads or registered escrow entries; this test fails if either regresses
// (marker dropped, escrow registration skipped, clone removed) or if the
// Timeout is dropped in the same breath.
func TestModelControlRunAgentLeafEscrowAudit(t *testing.T) {
	env := newMarkerModelControlEnv(t)
	// The adapter MUST escrow through the install's registry-store escrow
	// (one instance), not a private one the pinned leaves never see.
	require.Same(t, env.registries.credentialEscrow(), env.install.RunnerHandle.credentials)
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	creds := escrowTestCreds("audit-secret")
	root, err := env.control.Run(withCallCredentials(context.Background(), creds), markerCaptureInvocation(env), reg, markerCaptureSpec())
	require.NoError(t, err)
	env.executor.waitCaptured(t)
	launches := env.executor.snapshotLaunches()
	require.Len(t, launches, 1)
	require.Equal(t, 90*time.Second, launches[0].Task.Timeout, "timeout preserved on the audited path")
	var payload batchLeafPayload
	require.NoError(t, json.Unmarshal(launches[0].Task.Payload, &payload))
	require.True(t, payload.CallCredentials, "the executor sees the marked payload")
	require.NotContains(t, string(launches[0].Task.Payload), "audit-secret")
	// The registered entry is a CLONE taken at registration: mutating the
	// caller's CredentialSet after Submit cannot change what a leaf observes.
	creds.Credentials[0].APIKey = "mutated-after-submit"
	entry, complete, err := env.install.RunnerHandle.credentials.consume(batchCredentialKeyFor(payload), string(root))
	require.NoError(t, err, "the escrow key derives exactly from the stamped payload")
	require.Equal(t, "audit-secret", entry.Credentials[0].APIKey, "the entry holds the registration-time clone")
	complete()
	env.executor.letGo()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	batch := env.install.RunnerHandle.batchFor(root)
	require.NotNil(t, batch)
	_, err = batch.Wait(waitCtx)
	require.NoError(t, err)
	markerCaptureWaitEscrowRootGone(t, env.install.RunnerHandle.credentials, root)
}
