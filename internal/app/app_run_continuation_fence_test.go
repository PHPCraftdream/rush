package app

// R12-H2b acceptance-14 tests (design §2.4): the durable run queue must
// carry the session execution fence across process restarts. Each test
// names the production mutation it catches: a pump rebuild that dropped
// the fence transfer would silently restart a fenced session on the OS
// disk, a purge that cleared the fence would strip the next process of
// its requirement, and an unmarked continuation row on a fenced session
// must be fenced by the persisted session metadata (TASK A's end-to-end
// path in RebuildSessionAgentCall).

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// countedLeakDisk adds a write counter to the package's reusable fake
// disk WITHOUT touching app_run_construction_leak_test.go: the live-pump
// test must prove the ORIGINAL provider object (not a fresh OS-disk
// registry) executed the continuation, and a write-count delta is the
// cheapest observable for that.
type countedLeakDisk struct {
	*leakFakeDisk
	writes atomic.Int64
}

func (d *countedLeakDisk) WriteFile(ctx context.Context, name string, b []byte, mode fs.FileMode) error {
	d.writes.Add(1)
	return d.leakFakeDisk.WriteFile(ctx, name, b, mode)
}

// snapshotKeys lists the provider's current keys for failure diagnostics.
func (d *countedLeakDisk) snapshotKeys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.files))
	for k := range d.files {
		keys = append(keys, k)
	}
	return keys
}

// newAdmissionRaceAppOnDir is newAdmissionRaceApp with the data dir
// taken as a parameter, so a test can open a SECOND app over the SAME
// dataDir and model a restart (no live capability bindings, the fence
// loaded from the persisted session metadata). Copied verbatim from
// newAdmissionRaceApp (which must stay untouched); only the dataDir
// source differs.
func newAdmissionRaceAppOnDir(t *testing.T, handler http.HandlerFunc, dataDir string) (*App, string) {
	t.Helper()

	// Both GlobalConfig() and GlobalConfigData() resolution paths must be
	// isolated separately (see the golden envelope test's identical block).
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Pair the pool's refCount: db.Release is the contractual counterpart of db.Connect (see common_test.go's testEnv cleanup).
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	// A14 seam: these e2e scripts match the literal "Async ... started" text
	// and count pulled notices -- the flow the inline window replaces.
	agent.SetInlineWindowForTest(application.AgentCoordinator, false)

	sess, err := application.Sessions.Create(context.Background(), "admission-race-title")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	return application, sess.ID
}

// fenceLoader adapts the app's session service to the fence-store seam:
// the H2 interim adapter exposes Store/LoadSessionExecutionRequirement via
// an interface assertion, not the wide Service interface.
func fenceLoader(t *testing.T, application *App) session.SessionExecutionRequirementStore {
	t.Helper()
	reqStore, ok := application.Sessions.(session.SessionExecutionRequirementStore)
	require.True(t, ok, "the app's session service must implement the fence store seam")
	return reqStore
}

// runLiveProviderTurn drives one provider-backed ExecuteRun exactly like
// the construction-leak test: the fake disk writes the probe file and the
// admission fence write lands the session execution requirement in the
// durable store. Returns the app, the session id, the fence envelope and
// the counted fake disk.
//
// The scripted provider has four branches, matched in request order:
//
//  1. Initial request (prompt "write probe", no tool result in the
//     history) → default branch: fs_write(initialTarget, "live-value")
//     with finish_reason tool_calls.
//  2. Initial follow-up (history carries the tool RESULT id "call_write")
//     → text+stop: the initial turn ENDS (no re-emitted write, the loop
//     detector never fires).
//  3. Continuation request (prompt carries the marker) → marker branch:
//     fs_write(continuationTarget, "continuation-value") with finish
//     reason tool_calls.
//  4. Continuation follow-up (tool RESULT id "call_cont") → text+stop:
//     the continuation turn ends.
//
// The marker branch is matched BEFORE the call_write branch: the
// continuation request's history already contains the initial turn's
// call_write tool call and result, so the ids alone cannot sequence the
// rounds — the marker identifies the continuation request; the ids only
// identify result rounds (call_cont is matched first for the same reason).
func runLiveProviderTurn(t *testing.T, marker string, continuationTarget func() string) (*App, string, *session.SessionExecutionRequirement, *countedLeakDisk) {
	t.Helper()
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	// Assigned below, once the app (and its working dir) exists; the handler
	// only reads it after ExecuteRun has issued the first request.
	var initialTarget string
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"call_cont"`):
			// The continuation follow-up carries the tool RESULT (id call_cont):
			// end the turn instead of issuing the write again.
			admissionWriteSSE(w, []string{admissionSSEText("c9", "continuation done"), admissionSSEStop("c9", "stop")})
		case strings.Contains(string(body), marker):
			input, err := json.Marshal(map[string]any{"items": []map[string]any{{
				"path": filepath.ToSlash(continuationTarget()), "content": "continuation-value",
			}}})
			require.NoError(t, err)
			admissionWriteSSE(w, []string{admissionSSEToolCall("c9", "call_cont", "fs_write", string(input)), admissionSSEStop("c9", "tool_calls")})
		case strings.Contains(string(body), `"call_write"`):
			// matched after the marker branch — the continuation request's
			// history contains this id too. The initial follow-up carries the tool RESULT of the INITIAL
			// write (id call_write): end the initial turn instead of
			// replaying the write forever (the loop-detector failure on PR #82).
			admissionWriteSSE(w, []string{admissionSSEText("c1", "initial write done"), admissionSSEStop("c1", "stop")})
		default:
			input, err := json.Marshal(map[string]any{"items": []map[string]any{{
				"path": filepath.ToSlash(initialTarget), "content": "live-value",
			}}})
			require.NoError(t, err)
			admissionWriteSSE(w, []string{admissionSSEToolCall("c1", "call_write", "fs_write", string(input)), admissionSSEStop("c1", "tool_calls")})
		}
	}
	application, sessionID := newAdmissionRaceApp(t, handler)
	require.NotNil(t, application.batchInstall)
	cutoverWriter := application.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(application.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(cutoverWriter), application.batchLiveReader, cutoverWriter))

	workDir := application.config.WorkingDir()
	dir := filepath.Join(workDir, "scope")
	// The initial write targets the EXACT absolute path inside the granted
	// scope, and the REAL OS file at that path is pre-seeded with a sentinel:
	// if provider routing is ever dropped (the fence binding lost and the
	// call silently normalised to the OS disk), the provider-content and
	// untouched-sentinel assertions below fail BEFORE any continuation test
	// can be credited. The old fixture requested a RELATIVE path outside the
	// granted scope; production denied the write and the scripted model
	// re-emitted it forever (loop detector, CI run on PR #82).
	const providerInitialSentinel = "OS-SENTINEL-MUST-NOT-CHANGE\n"
	initialTarget = filepath.Join(dir, "probe-live.txt")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(initialTarget, []byte(providerInitialSentinel), 0o644))
	fake := &countedLeakDisk{leakFakeDisk: newLeakFakeDisk()}
	fake.seedDir(dir)
	fake.seedDir(filepath.ToSlash(dir))

	res, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt: "write probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
			DiskProvider: fake,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	// The provider must have received the write for the EXACT absolute
	// scoped target (native spelling first; the slashed spelling is the
	// only tolerated alias — relative-path guessing is what broke the old
	// fixture). Precedent: app_run_construction_leak_test.go's two-spelling
	// lookup, minus its relative-path fallback.
	fake.mu.Lock()
	got, ok := fake.files[initialTarget]
	if !ok {
		got, ok = fake.files[filepath.ToSlash(initialTarget)]
	}
	fake.mu.Unlock()
	require.True(t, ok, "the provider run never wrote the exact scoped absolute target %s (provider keys: %v)", initialTarget, fake.snapshotKeys())
	require.Equal(t, "live-value", got, "the provider run must persist its write through the fake disk at the exact target")
	sentinel, err := os.ReadFile(initialTarget)
	require.NoError(t, err)
	require.Equal(t, providerInitialSentinel, string(sentinel), "the REAL OS file at the exact target must be untouched: the provider run must not reach the OS disk")

	fence, err := fenceLoader(t, application).LoadSessionExecutionRequirement(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, fence, "a provider-backed run must land the session execution fence")
	require.NotEmpty(t, fence.BindingID, "the fence must name a binding")
	require.Greater(t, fence.Epoch, int64(0), "the fence must carry a positive epoch")
	return application, sessionID, fence, fake
}

// enqueueSessionAgentCall serializes a SessionAgentCall the same way the
// durable producers do and enqueues it; the idempotency key IS the row id
// (EnqueueRunQueueEntry uses ON CONFLICT(id)).
func enqueueSessionAgentCall(t *testing.T, application *App, call agent.SessionAgentCall) string {
	t.Helper()
	data := agent.ToSessionAgentCallData(call)
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	rowID := "row-" + call.LogicalCallID
	require.NoError(t, application.Sessions.EnqueueRunQueueEntry(
		context.Background(), rowID, call.SessionID, raw))
	return rowID
}

// TestContinuationQueuedRow_LivePumpRebuildInvokesOriginalProvider
// catches the rebuild dropping the fence transfer: a marked durable row
// leased by the LIVE pump (same process, real RunQueuePump tick) must
// resolve its envelope back to the SAME provider object that ran the
// original turn — the continuation write lands in the fake disk (not the
// OS disk) and the provider's write count increases, proving the original
// binding was restored rather than a fresh OS-disk registry.
func TestContinuationQueuedRow_LivePumpRebuildInvokesOriginalProvider(t *testing.T) {
	var continuationTarget string
	application, sessionID, fence, fake := runLiveProviderTurn(t,
		"CONTINUATION_LIVE_MARKER", func() string { return continuationTarget })
	workDir := application.config.WorkingDir()
	dir := filepath.Join(workDir, "scope")
	continuationTarget = filepath.Join(dir, "probe-continuation.txt")
	writesBefore := fake.writes.Load()

	rowID := enqueueSessionAgentCall(t, application, agent.SessionAgentCall{
		SessionID:     sessionID,
		LogicalCallID: "cont-live-1",
		Prompt:        "CONTINUATION_LIVE_MARKER",
		// Restricted run: the pump-rebuilt turn must re-arm auto-approval (R4-3).
		RunAllowlistSpec: &permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"fs_write"}},
		FolderScopeSpec:  &permission.FolderScopeSpec{Entries: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}}},
		DiskRequirement:  fence,
	})

	require.Eventually(t, func() bool {
		entry, err := application.Sessions.GetRunQueueEntry(context.Background(), rowID)
		if err != nil || entry != nil {
			return false
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		contents, ok := fake.files[continuationTarget]
		if !ok {
			contents = fake.files[filepath.ToSlash(continuationTarget)]
		}
		return contents == "continuation-value"
	}, 60*time.Second, 100*time.Millisecond,
		"the live pump must lease the marked row, rebuild it onto the ORIGINAL provider, and let the continuation write land in the fake disk")

	require.Greater(t, fake.writes.Load(), writesBefore,
		"the continuation must have gone through the SAME provider object, not a fresh registry")
}

// TestContinuationQueuedRow_AfterRestartYieldsTypedTerminalUnavailable
// catches the rebuild resurrecting a dead binding after a restart: the
// second app over the same dataDir has NO live capability bindings, so a
// marked row whose envelope names the lost binding must be refused
// TERMINALLY by the pump (row deleted) WITHOUT the marker ever reaching
// the model — a retryable failure would spin, a silent fallback would
// restart the turn on the OS disk.
func TestContinuationQueuedRow_AfterRestartYieldsTypedTerminalUnavailable(t *testing.T) {
	var handlerRequests atomic.Int64
	application, sessionID, fence, _ := runLiveProviderTurn(t,
		"CONTINUATION_RESTART_MARKER", func() string { return "unused" })
	application.Shutdown()
	// The duplicate shutdown via t.Cleanup is safe (Shutdown is
	// idempotent); the fence stays in the durable store. The persisted
	// session row keeps ITS id, so the restart row below targets sessionID.
	dataDir := application.config.WorkingDir()

	second, _ := newAdmissionRaceAppOnDir(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		handlerRequests.Add(1)
		if strings.Contains(string(body), "CONTINUATION_RESTART_MARKER") {
			t.Error("the restart marker reached the model: the fenced row must be refused before any provider traffic")
		}
		admissionWriteSSE(w, []string{admissionSSEText("cX", "IGNORED"), admissionSSEStop("cX", "stop")})
	}, dataDir)

	// Baseline BEFORE the row exists: nothing but the app's own
	// construction can have talked to the mock provider.
	baseline := handlerRequests.Load()

	rowID := enqueueSessionAgentCall(t, second, agent.SessionAgentCall{
		SessionID:       sessionID,
		LogicalCallID:   "cont-restart-1",
		Prompt:          "CONTINUATION_RESTART_MARKER",
		DiskRequirement: fence,
	})

	require.Eventually(t, func() bool {
		entry, err := second.Sessions.GetRunQueueEntry(context.Background(), rowID)
		return err == nil && entry == nil
	}, 60*time.Second, 100*time.Millisecond,
		"the pump must terminally refuse and delete the marked row whose binding died with the first process")
	require.Equal(t, baseline, handlerRequests.Load(),
		"no provider request may carry the marker: the refusal must happen at rebuild time")
}

// TestContinuationUnmarkedRow_FencedBySessionMetadata is TASK A's
// end-to-end acceptance (design §2.4, acceptance 14): an UNMARKED row
// (no envelope, no HostDiskProvider mark) on a session whose persisted
// metadata carries a fence must be fenced by that metadata after a
// restart — the empty capability manager cannot resolve the binding, so
// the pump refuses the row terminally and the marker never reaches the
// model. Deleting the nil-source fence consult in RebuildSessionAgentCall
// would let this row rebuild onto the OS disk silently.
func TestContinuationUnmarkedRow_FencedBySessionMetadata(t *testing.T) {
	handlerRequests := atomic.Int64{}
	application, sessionID, _, _ := runLiveProviderTurn(t,
		"CONTINUATION_UNMARKED_MARKER", func() string { return "unused" })
	application.Shutdown()
	// The unmarked row must name the FENCED session (the first app's
	// persisted session row), not the restart app's freshly seeded one.
	dataDir := application.config.WorkingDir()

	second, _ := newAdmissionRaceAppOnDir(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		handlerRequests.Add(1)
		if strings.Contains(string(body), "CONTINUATION_UNMARKED_MARKER") {
			t.Error("the unmarked continuation reached the model on a fenced session: the session-metadata fence must refuse it")
		}
		admissionWriteSSE(w, []string{admissionSSEText("cX", "IGNORED"), admissionSSEStop("cX", "stop")})
	}, dataDir)
	baseline := handlerRequests.Load()

	rowID := enqueueSessionAgentCall(t, second, agent.SessionAgentCall{
		SessionID:     sessionID,
		LogicalCallID: "cont-unmarked-1",
		Prompt:        "CONTINUATION_UNMARKED_MARKER",
		// Deliberately NO DiskRequirement and NO HostDiskProvider mark.
	})

	require.Eventually(t, func() bool {
		entry, err := second.Sessions.GetRunQueueEntry(context.Background(), rowID)
		return err == nil && entry == nil
	}, 60*time.Second, 100*time.Millisecond,
		"the unmarked row on a fenced session must be fenced by the persisted metadata and terminally refused")
	require.Equal(t, baseline, handlerRequests.Load(),
		"the fenced marker must never reach the model")
}

// TestContinuationFence_SurvivesQueuePurgeAndReset catches the purge
// clearing the fence: PurgeQueuedWorkForSession must remove only the
// session's queued CALL rows — the execution-requirement fence row is
// acked/terminal and must SURVIVE, because the next process would
// otherwise lose the requirement and could rebuild ordinary turns on the
// OS disk.
func TestContinuationFence_SurvivesQueuePurgeAndReset(t *testing.T) {
	application, sessionID, fence, _ := runLiveProviderTurn(t,
		"CONTINUATION_PURGE_MARKER", func() string { return "unused" })

	_, err := application.Sessions.PurgeQueuedWorkForSession(context.Background(), sessionID)
	require.NoError(t, err)

	after, err := fenceLoader(t, application).LoadSessionExecutionRequirement(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, after, "the fence must survive a queued-work purge")
	require.Equal(t, fence.BindingID, after.BindingID,
		"the surviving fence must name the SAME binding the run established")

	outstanding, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, outstanding, "the purge must leave no outstanding queued calls behind")
}
