// Dead-host recovery scenarios, doc sec.6's "Сквозные в internal/app (два
// App на одном каталоге данных)" list, step 5:
//
//	(а) A starts an async bash and announces it, then "crashes" (releases
//	    its lock without a clean close); `rush run --continue` in B gets
//	    exactly one "interrupted" in its first prompt and finishes.
//	(б) a job finished and delivered in A -- B adds nothing.
//	(в) Ctrl-C of `rush run` with live tasks -- no new turn, and after a
//	    restart one "interrupted" per task.
//
// Two *App instances share one on-disk data dir (two independent config.Init
// + db.Connect calls, refcounted by internal/db onto the SAME sqlite file) --
// the closest a single test process gets to two `rush run` invocations
// against the same data directory. Each App gets its own probe HTTP
// provider so request counts are independent and easy to assert on.
package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// configureRecoveryProbeProvider mirrors newAdmissionRaceApp's provider
// wiring, factored out so this file's two-App harness can call it once per
// App with a distinct base URL.
func configureRecoveryProbeProvider(store *config.ConfigStore, baseURL string) {
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: baseURL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()
}

// newRecoveryTwoAppHarness builds App A and App B against the SAME data
// dir, each with its own probe provider server. DB connection refcounting
// (internal/db) is released exactly twice by t.Cleanup regardless of what
// each test does with the App objects themselves (a crash-simulating test
// never calls Shutdown on App A) -- see SimulateCrashForTest's own doc for
// why a real Shutdown() is deliberately NOT used here.
func newRecoveryTwoAppHarness(t *testing.T, handlerA, handlerB http.HandlerFunc) (appA, appB *App, sessionID string) {
	t.Helper()

	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	srvA := httptest.NewServer(handlerA)
	t.Cleanup(srvA.Close)
	storeA, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	configureRecoveryProbeProvider(storeA, srvA.URL)
	connA, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// A standalone MCP owner per App: the process-wide owner slot
	// (mcp.Acquire, App's default) is a SINGLETON per OS process -- two real
	// `rush run` processes each get their own, but two *App instances in
	// ONE test process contend for the same slot without this (CLAUDE.md's
	// "Multi-App isolation is an API problem" -- AcquireStandalone is that
	// API).
	ownerA, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	appA, err = New(context.Background(), connA, storeA, WithMCPOwner(ownerA))
	require.NoError(t, err)
	// A14 seam: the scenario scripts match the literal "Async ... started"
	// text and count pulled notices -- the flow the inline window replaces.
	agent.SetInlineWindowForTest(appA.AgentCoordinator, false)
	// App.Shutdown is the ONLY thing that correctly tears down every
	// resource New()/InitCoderAgent opened (the run queue pump goroutine,
	// New()'s own internal ConnectRead on top of this Connect, the MCP
	// owner, ...) -- deferred to cleanup so it runs AFTER this test's own
	// crash/graceful-exit simulation on App A's ASYNC JOB STORE specifically
	// (a distinct, narrower resource Shutdown also closes, idempotently: see
	// AsyncJobStore.Close's nil-host early return).
	t.Cleanup(appA.Shutdown)

	sess, err := appA.Sessions.Create(context.Background(), "two-app-recovery-scenario")
	require.NoError(t, err)
	_, err = appA.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	sessionID = sess.ID

	srvB := httptest.NewServer(handlerB)
	t.Cleanup(srvB.Close)
	storeB, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	configureRecoveryProbeProvider(storeB, srvB.URL)
	connB, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	ownerB, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	appB, err = New(context.Background(), connB, storeB, WithMCPOwner(ownerB))
	require.NoError(t, err)
	agent.SetInlineWindowForTest(appB.AgentCoordinator, false)
	t.Cleanup(appB.Shutdown)

	return appA, appB, sessionID
}

// recoveryBuildCommand is shortBuildCommand's own cross-platform technique
// (app_run_subagent_async_result_test.go): outlives the brief window between
// the tool call resolving and this test's own crash-simulation call.
func recoveryBuildCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 4 127.0.0.1"
	}
	return "sleep 3"
}

// countBackgroundNotices returns how many of sessionID's persisted messages
// are background-job notices, and how many of those carry noticeKind
// (pass "" to count every kind).
func countBackgroundNotices(t *testing.T, app *App, sessionID, noticeKind string) int {
	t.Helper()
	msgs, err := app.Messages.List(context.Background(), sessionID)
	require.NoError(t, err)
	n := 0
	for _, m := range msgs {
		if !m.BackgroundJobNotice {
			continue
		}
		if noticeKind == "" || m.NoticeKind == noticeKind {
			n++
		}
	}
	return n
}

// TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted is doc sec.6
// scenario (а): A claims and announces an async bash job, then "crashes"
// (SimulateCrashForTest -- the OS lock is released, but nothing is
// transitioned and the host's own-id marking is forgotten, exactly like a
// killed process). `rush run --continue` in B must recover that row to
// 'interrupted' and deliver it in its OWN FIRST turn, then finish cleanly.
func TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch requestsA.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash",
					`{"command":`+jsonString(recoveryBuildCommand())+`,"description":"long job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		case 2:
			require.Contains(t, lastTool, "Async bash job call-1 started")
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		default:
			http.Error(w, "unexpected model call on A", http.StatusBadRequest)
		}
	}
	var sawInterrupted atomic.Bool
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch requestsB.Add(1) {
		case 1:
			// The interrupted notice is a user-role message inserted BEFORE
			// this turn's own new "continue please" user prompt -- search
			// the whole body, not just the LAST user message.
			if strings.Contains(string(body), "was interrupted") {
				sawInterrupted.Store(true)
			}
			admissionWriteSSE(w, []string{admissionSSEText("continue", "continuing"), admissionSSEStop("continue", "stop")})
		default:
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resA, err := appA.ExecuteRun(ctx, RunRequest{
		Prompt: "start a long job", Origin: message.OriginCLI, Overrides: RunOverrides{Origin: message.OriginCLI},
		Mode: RunModeJSON, ContinueSessionID: sessionID, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, resA)
	require.EqualValues(t, 2, requestsA.Load())

	row, err := appA.asyncJobStore.Get(ctx, sessionID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "the job must still be running when A crashes")
	require.Equal(t, appA.asyncJobStore.HostID(), row.HostID)

	// "падает" (crashes): the OS lock is released, nothing transitioned, no
	// clean Close ever ran.
	require.NoError(t, appA.asyncJobStore.SimulateCrashForTest())

	ctxB, cancelB := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelB()
	resB, err := appB.RunNonInteractiveWithResult(ctxB, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	require.EqualValues(t, 1, requestsB.Load(), "the interrupted notice must be visible in B's FIRST turn -- no extra turn needed")
	require.True(t, sawInterrupted.Load(), "B's first prompt must contain the interrupted notice")

	require.Equal(t, 1, countBackgroundNotices(t, appB, sessionID, "interrupted"), "exactly one interrupted notice")
	rowAfter, err := appB.asyncJobStore.Get(ctx, sessionID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", rowAfter.State)
	require.EqualValues(t, 0, rowAfter.Wake)
}

// TestTwoAppScenarioB_DeliveredInADoesNotResurfaceInB is doc sec.6 scenario
// (б): a job finishes and is fully delivered (pulled + reacted) inside A's
// own run -- B's continuation must not add any extra notice.
func TestTwoAppScenarioB_DeliveredInADoesNotResurfaceInB(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastTool, "Async bash job call-1 started"):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		case strings.Contains(lastUser, "call-1 (bash)"):
			admissionWriteSSE(w, []string{admissionSSEText("reacted", "job finished, all good"), admissionSSEStop("reacted", "stop")})
		default:
			requestsA.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash", `{"command":"echo hi","description":"quick job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		switch requestsB.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{admissionSSEText("continue", "still fine"), admissionSSEStop("continue", "stop")})
		default:
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resA, err := appA.RunNonInteractiveWithResult(ctx, io.Discard, "run a quick job", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resA)
	require.Equal(t, "end_turn", resA.ExitReason, "warnings=%v", resA.Warnings)
	require.Equal(t, 1, countBackgroundNotices(t, appA, sessionID, ""), "A's own run must have delivered exactly one notice")

	before := countBackgroundNotices(t, appA, sessionID, "")

	// A clean exit (unlike scenario а): release the host lock via the real
	// Close path, matching a `rush run` process that finished normally.
	require.NoError(t, appA.asyncJobStore.Close(ctx))

	resB, err := appB.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason)
	require.EqualValues(t, 1, requestsB.Load(), "B must need only its own plain continuation turn")

	after := countBackgroundNotices(t, appB, sessionID, "")
	require.Equal(t, before, after, "B must add NO extra notice over an already-delivered job")
}

// TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask is doc sec.6
// scenario (в): cancelling A's run (Ctrl-C) while two tasks are still
// running must produce no further turn; a graceful exit (Close, the real
// production shutdown path for the host lock) still leaves both rows
// 'running'; B's restart delivers exactly one "interrupted" per task.
func TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	bothStarted := make(chan struct{})
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		// Two tool calls are dispatched across two sequential steps of the
		// SAME turn (never two tool calls in one streamed response -- a
		// proper multi-index SSE delta stream is unnecessary complexity this
		// test does not need) so each stays a simple, well-trodden
		// single-tool-call chunk.
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch requestsA.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start1", "call-1", "bash", `{"command":`+jsonString(recoveryBuildCommand())+`,"description":"job 1"}`),
				admissionSSEStop("start1", "tool_calls"),
			})
		case 2:
			require.Contains(t, lastTool, "Async bash job call-1 started")
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start2", "call-2", "bash", `{"command":`+jsonString(recoveryBuildCommand())+`,"description":"job 2"}`),
				admissionSSEStop("start2", "tool_calls"),
			})
		case 3:
			require.Contains(t, lastTool, "Async bash job call-2 started")
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
			close(bothStarted)
		default:
			t.Errorf("no further turn may reach A's provider after Ctrl-C (got request %d)", requestsA.Load())
			http.Error(w, "no further turns allowed", http.StatusBadRequest)
		}
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch requestsB.Add(1) {
		case 1:
			// Both interrupted notices are user-role messages inserted
			// BEFORE this turn's own new "continue please" user prompt --
			// search the whole body, not just the LAST user message.
			require.Contains(t, string(body), "call-1")
			require.Contains(t, string(body), "call-2")
			admissionWriteSSE(w, []string{admissionSSEText("continue", "continuing"), admissionSSEStop("continue", "stop")})
		default:
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	ctxA, cancelA := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = appA.RunNonInteractiveWithResult(ctxA, io.Discard, "start two long jobs", RunOverrides{
			Origin: message.OriginCLI,
		}, true, RunModeJSON, sessionID, false)
	}()

	select {
	case <-bothStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("both jobs never reached the provider")
	}
	require.Eventually(t, func() bool {
		r1, err1 := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
		r2, err2 := appA.asyncJobStore.Get(context.Background(), sessionID, "call-2")
		return err1 == nil && err2 == nil && r1.State == "running" && r2.State == "running"
	}, 5*time.Second, 20*time.Millisecond, "both rows must be claimed and running before Ctrl-C")

	// Ctrl-C: cancel the ctx the CLI loop is blocked on.
	cancelA()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after ctx cancellation")
	}
	require.Error(t, runErr)
	requestsAtCancel := requestsA.Load()
	require.EqualValues(t, 3, requestsAtCancel, "Ctrl-C must not have produced any further turn")

	// Graceful exit through the REAL App shutdown path (W-DRAIN B4 fix,
	// docs/reviews/2026-09-29-async-phase4-round1.md "Tests (B-b, C-b)": the
	// old version called appA.asyncJobStore.Close directly here, bypassing
	// AgentCoordinator.CancelAll entirely. CancelAll -- via
	// coordinator.CancelAll's `c.asyncJobs.close()` -- is what actually
	// cancels the STILL-RUNNING async bash job's own executor context (the
	// real `ping`/`sleep` subprocess two jobs above are running for real,
	// independent of ctxA, which only governs the CLI loop's own turn); a
	// bare store.Close leaves that real OS process running to completion in
	// the background, unkilled. Shutdown() runs CancelAgents() (== CancelAll)
	// before releasing the host lock, then closes the async job store itself
	// (idempotent with the harness's own deferred appA.Shutdown cleanup, via
	// app.shutdownOnce) -- exactly what a real `rush run` process does on
	// exit.
	appA.Shutdown()
	row1, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row1.State, "a graceful exit must never transition a live row")
	row2, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-2")
	require.NoError(t, err)
	require.Equal(t, "running", row2.State)

	// The Never window must OUTLAST the jobs' own real duration
	// (recoveryBuildCommand's ~3s sleep/ping): the original 300ms window
	// could never distinguish "CancelAll genuinely killed the subprocess"
	// from "the job simply hasn't finished naturally yet" -- only a window
	// past the job's natural completion proves the KILL, not the clock, is
	// what keeps A silent (and that the rows above stayed 'running' because
	// the executor was actually cancelled, not because we didn't wait long
	// enough to see it finish and transition on its own).
	require.Never(t, func() bool { return requestsA.Load() > requestsAtCancel }, 5*time.Second, 50*time.Millisecond,
		"no request may reach A's provider after a real CancelAll, even once the killed job's own natural duration has fully elapsed")

	ctxB, cancelB := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelB()
	resB, err := appB.RunNonInteractiveWithResult(ctxB, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	require.EqualValues(t, 1, requestsB.Load(), "both interrupted notices must be visible in B's FIRST turn")
	require.Equal(t, 2, countBackgroundNotices(t, appB, sessionID, "interrupted"), "one interrupted notice PER task")

	for _, id := range []string{"call-1", "call-2"} {
		row, err := appB.asyncJobStore.Get(context.Background(), sessionID, id)
		require.NoError(t, err)
		require.Equal(t, "interrupted", row.State, "job %s must be interrupted", id)
	}
}

// TestTwoAppScenarioE_AnotherAppPullingDoesNotEraseDebt_RootStillReacts is
// the multi-process-shaped version of internal/agent's
// TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts (W-DRAIN B5,
// docs/reviews/2026-09-29-async-phase4-round1.md "Tests (B-b, C-b)": that
// test is flagged "single-process" -- it proves the SQL-level fact (a pull
// only ever touches `delivery`, never `reacted` -- see PullPendingAsyncJob
// Notice's own SQL) within ONE process/connection. This proves the SAME
// property survives a GENUINELY SEPARATE process: App B, its own *sql.DB
// connection (db.Connect, refcounted onto the same file, exactly like the
// other two-App scenarios in this file) and its own message.Service,
// independently pulls the session's notice into history -- matching doc
// sec.3.4's policy-table row "web-tab that opened the session: only pull, no
// turn" -- while App A (the CLI root, a DIFFERENT process/connection driving
// the SAME session) still reacts to it on its own next turn, and that
// reaction is visible in THAT turn's own JSON result ("gets it in the
// result").
func TestTwoAppScenarioE_AnotherAppPullingDoesNotEraseDebt_RootStillReacts(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	toolStarted := make(chan struct{})
	var toolStartedOnce sync.Once
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(string(body), "call-1 (bash)"):
			// The SECOND call's reacting turn is a REAL "continue please" user
			// turn (A's own loop already exited on Ctrl-C, so there is no
			// empty-prompt Drain to react via) -- the pulled notice text sits
			// BEFORE "continue please" chronologically, so it is NOT
			// lastTurnParts' lastUser (that's the fresh prompt); check the
			// whole body instead, matching scenario B's own handlerB
			// technique for the identical shape. Checked BEFORE lastTool
			// below: once the FIRST call's tool result ever lands in history,
			// it stays the LAST tool-role message forever (no new tool call
			// happens in the second call), so lastTool alone would keep
			// matching the first branch on every later request too.
			requestsA.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("reacted", "root reacted to the job"), admissionSSEStop("reacted", "stop")})
		case strings.Contains(lastTool, "Async bash job call-1 started"):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
			toolStartedOnce.Do(func() { close(toolStarted) })
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash", `{"command":`+jsonString(recoveryBuildCommand())+`,"description":"job 1"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		requestsB.Add(1)
		http.Error(w, "App B must never need its own provider turn in this scenario (pull only)", http.StatusBadRequest)
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	// Ctrl-C A's OWN loop before the (real, ~3s) async job finishes -- the
	// job's executor is NOT tied to ctxA (doc sec.3.4/3.7: async work
	// survives the turn/loop that started it) and keeps running for real in
	// the background, unattended, exactly like scenario C's own technique.
	// This is what opens a genuine race window: nobody (not even A) reacts
	// to the job's own eventual completion until this test says so.
	ctxA, cancelA := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = appA.RunNonInteractiveWithResult(ctxA, io.Discard, "run a job", RunOverrides{
			Origin: message.OriginCLI,
		}, true, RunModeJSON, sessionID, false)
	}()

	select {
	case <-toolStarted:
	case <-time.After(20 * time.Second):
		t.Fatal("the job never reached the provider")
	}
	require.Eventually(t, func() bool {
		row, getErr := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
		return getErr == nil && row.State == "running"
	}, 5*time.Second, 20*time.Millisecond, "the job must be claimed and running before cancellation")

	cancelA()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after ctx cancellation")
	}
	require.Error(t, runErr)

	// Wait for the job to finish NATURALLY (a real subprocess, unattended --
	// A's own loop already exited) -- open, unreacted debt nobody is
	// watching yet.
	require.Eventually(t, func() bool {
		row, getErr := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
		return getErr == nil && row.State == "completed" && row.Wake != 0 && row.Reacted == 0
	}, 10*time.Second, 20*time.Millisecond, "the job must finish naturally with open (unreacted) debt")

	// App B -- a GENUINELY SEPARATE process/connection on the same data dir
	// -- independently pulls the SAME notice into history. Must not erase
	// the debt, and must never itself provoke a provider turn.
	pulled, err := appB.asyncJobStore.PullJobNotices(context.Background(), appB.Messages, sessionID, recoveryScenarioEBuildJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "App B's independent pull must actually move the row into history")
	require.Zero(t, requestsB.Load(), "App B's own pull must never itself trigger a provider turn")

	debtStillOpen, err := appB.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, debtStillOpen, "another process's independent pull must not erase the debt it just made visible")

	// The CLI root (App A, a DIFFERENT process/connection from B) still
	// reacts to it on its own next turn, and the reaction is visible in
	// THAT turn's own JSON result.
	ctxA2, cancelA2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelA2()
	resA2, err := appA.RunNonInteractiveWithResult(ctxA2, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resA2)
	require.Equal(t, "end_turn", resA2.ExitReason, "warnings=%v", resA2.Warnings)
	// requestsA only increments in the "call-1 (bash)" (reacting) branch --
	// the tool-call-start and yield branches from the first (cancelled) call
	// do not touch it -- so exactly 1 here means exactly one real reacting
	// turn happened, on this second call, never during the first.
	require.EqualValues(t, 1, requestsA.Load(), "the root must have made exactly one real reacting turn")
	require.Contains(t, resA2.FinalText, "root reacted to the job", "the root's reaction must be visible in ITS OWN JSON result")

	debtAfter, err := appA.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
	require.NoError(t, err)
	require.False(t, debtAfter, "the root's own turn must clear the debt")
}

// recoveryScenarioEBuildJobNoticeParams is a minimal, App-package-local
// stand-in for internal/agent's unexported buildJobNoticeMessageParams:
// enough to prove "the row moved into history" without needing that
// package's exact formatting (agent.FormatAsyncCompletion is exported but
// this test only needs A's OWN later turn to be able to match the row's
// tool_call_id in a user-role message, which lastTurnParts already checks
// for via the literal "call-1 (bash)" substring app_run_admission_race_test.
// go's sibling scenarios rely on -- reproduced verbatim here).
func recoveryScenarioEBuildJobNoticeParams(row session.JobNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) finished: " + row.ResultContent}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}
