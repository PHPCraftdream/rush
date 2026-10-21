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
//
// Dual mode: flag OFF keeps every legacy read and literal byte-for-byte;
// flag ON (RUSH_BATCH_RUNTIME) reads batch_nodes and the real HostStore
// through the pp4 helpers, matches the real started/completion texts, uses
// unique call ids per turn, and asserts the design-authorized graceful
// lifecycle (confirmed interruption committed during HostClosing) separately
// from the hard-crash lifecycle. Handler goroutines never call require:
// they report through t.Errorf + an HTTP error and return.
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

// phase4ScenarioDeadline bounds one two-app scenario run. A scenario takes
// ~0.5 s; 30 s flaked in a full-package run on a machine starved by other
// builds (the run took 45 s), and a real hang is still caught -- just later.
const phase4ScenarioDeadline = 90 * time.Second

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
	t.Cleanup(func() {
		if f6BatchOn() {
			require.NoError(t, os.RemoveAll(dataDir))
		}
	})

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
	// crash/graceful-exit simulation on the App's ASYNC JOB STORE / HOST
	// STORE specifically (a distinct, narrower resource Shutdown also
	// closes, idempotently: see AsyncJobStore.Close's and HostStore.Close's
	// nil-host early returns).
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

// recoveryJobCommand returns a bash-tool command that stays "running" until
// the test creates the file at the returned release path, plus that path.
// The previous wall-clock command (ping -n 4 / sleep 3, ~3s) raced every
// assertion about the job's state: under load the provider round-trips took
// long enough that the row was already "completed" by the time the test
// looked at it. The sentinel-blocked loop makes the state deterministic in
// both directions: "running" until released, finished within one poll
// iteration after release, so no orphan process can outlive the test and
// hold its working directory open on Windows through the TempDir cleanup.
// ping is the poll delay on Windows because the interpreter's Go coreutils
// middleware provides no sleep there; the other CI operating systems poll
// with sleep 0.1.
func recoveryJobCommand(t *testing.T) (command, release string) {
	t.Helper()
	release = filepath.ToSlash(filepath.Join(t.TempDir(), "recovery-job-release"))
	poll := "sleep 0.1"
	if runtime.GOOS == "windows" {
		poll = "ping -n 2 127.0.0.1"
	}
	return `while [ ! -f "` + release + `" ]; do ` + poll + `; done`, release
}

// releaseRecoveryJob unblocks a recoveryJobCommand loop so the job finishes
// deterministically instead of at a wall-clock estimate.
func releaseRecoveryJob(t *testing.T, release string) {
	t.Helper()
	require.NoError(t, os.WriteFile(release, nil, 0o644))
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
// (the host lock is released, but nothing is transitioned and the host's
// own-id marking is forgotten, exactly like a killed process).
// `rush run --continue` in B must recover that work to 'interrupted'
// (host_lost) and deliver it in its OWN FIRST turn, then finish cleanly.
// The interrupted state itself is also the no-side-effect-rerun oracle: the
// command is sentinel-blocked and only released in cleanup, so a rerun
// would leave the root running, never interrupted.
func TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted(t *testing.T) {
	if f6BatchOn() {
		pcrRunCrashScenario(t)
		return
	}
	var requestsA, requestsB atomic.Int32
	jobCommand, jobRelease := recoveryJobCommand(t)
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch requestsA.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash",
					`{"command":`+jsonString(jobCommand)+`,"description":"long job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		case 2:
			if !pp4StartedSeen(lastTool, "call-1") {
				t.Errorf("A request 2: no started tool result for call-1: %q", lastTool)
				http.Error(w, "no started tool result", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		default:
			t.Errorf("unexpected model call on A (request %d)", requestsA.Load())
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
			t.Errorf("unexpected model call on B (request %d)", requestsB.Load())
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)
	// Registered AFTER the harness's own cleanups, so it runs BEFORE them
	// (and before the TempDir removals): the release lets the blocked loop
	// exit on its own, and the idempotent Shutdown kills it outright if the
	// release somehow does not take effect -- either way the interpreter and
	// its poll child are gone before their working directory is removed.
	t.Cleanup(func() {
		releaseRecoveryJob(t, jobRelease)
		appA.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resA, err := appA.ExecuteRun(ctx, RunRequest{
		Prompt: "start a long job", Origin: message.OriginCLI, Overrides: RunOverrides{Origin: message.OriginCLI},
		Mode: RunModeJSON, ContinueSessionID: sessionID, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, resA)
	require.EqualValues(t, 2, requestsA.Load())

	fact := pp4JobFactByCall(t, appA, sessionID, "call-1")
	require.Equal(t, "running", fact.State, "the job must still be running when A crashes")
	require.Equal(t, pp4DriverHostID(t, appA), fact.HostID)

	// "падает" (crashes): the OS lock is released, nothing transitioned, no
	// clean Close ever ran -- through the selected authority's crash seam.
	pp4CrashSimulate(t, appA)

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

	// Exactly one recovered notice. The interruption is the notice's
	// STATE/outcome/text under batch authority (the selected kind is the
	// root's, not a legacy per-state message kind), so ON counts the SELECTED
	// notices only: every root also carries its persisted STARTED
	// announcement (a background-job message with an empty notice kind) that
	// must not be counted as a second recovered notice. OFF keeps the legacy
	// "interrupted" kind count.
	if f6BatchOn() {
		require.Equal(t, 1, pp4SelectedNoticeCount(t, appB, sessionID), "exactly one recovered root notice")
	} else {
		require.Equal(t, 1, countBackgroundNotices(t, appB, sessionID, "interrupted"), "exactly one interrupted notice")
	}
	factAfter := pp4JobFactByCall(t, appB, sessionID, "call-1")
	require.Equal(t, "interrupted", factAfter.State)
	require.EqualValues(t, 0, factAfter.Wake)
	if f6BatchOn() {
		require.Equal(t, "host_lost", factAfter.OutcomeCode, "a dead-host recovery commits the host_lost outcome")
	}
}

// TestTwoAppScenarioB_DeliveredInADoesNotResurfaceInB is doc sec.6 scenario
// (б): a job finishes and is fully delivered (pulled + reacted) inside A's
// own run -- B's continuation must not add any extra notice. The quick echo
// job is a REAL detached job (the inline window is disabled by the harness);
// under batch authority the quick command may instead be elected INLINE,
// which consumes the completion delivery -- that shape is asserted
// explicitly (no selected notice, root completed, still nothing for B).
func TestTwoAppScenarioB_DeliveredInADoesNotResurfaceInB(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		completionSeen := strings.Contains(lastUser, "call-1 (bash)") || pp4CompletionSeen(lastUser, "call-1")
		startedSeen := strings.Contains(lastTool, "Async bash job call-1 started") || pp4StartedSeen(lastTool, "call-1")
		// The started tool result stays the LAST tool-role message forever,
		// so once a completion exists it must win the match. OFF keeps the
		// legacy branch order byte-for-byte; ON checks the completion first.
		switch {
		case completionSeen && (f6BatchOn() || !startedSeen):
			admissionWriteSSE(w, []string{admissionSSEText("reacted", "job finished, all good"), admissionSSEStop("reacted", "stop")})
		case startedSeen && !f6BatchOn():
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		case startedSeen:
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		case completionSeen:
			admissionWriteSSE(w, []string{admissionSSEText("reacted", "job finished, all good"), admissionSSEStop("reacted", "stop")})
		case f6BatchOn() && strings.TrimSpace(lastTool) != "":
			// ON: the quick echo can be elected INLINE, so the last tool
			// result is the command's plain output, not the started text and
			// not a completion notice. Any tool result for the delegated call
			// ends the turn with the answer; falling through to the default
			// branch re-issued the call until the loop detector stopped the
			// run on a tool_use finish.
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
			t.Errorf("unexpected model call on B (request %d)", requestsB.Load())
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	ctx, cancel := context.WithTimeout(context.Background(), phase4ScenarioDeadline)
	defer cancel()
	resA, err := appA.RunNonInteractiveWithResult(ctx, io.Discard, "run a quick job", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resA)
	require.Equal(t, "end_turn", resA.ExitReason, "warnings=%v", resA.Warnings)

	fact := pp4JobFactByCall(t, appA, sessionID, "call-1")
	require.Equal(t, "completed", fact.State, "A's own run must have finished the quick job")
	before := countBackgroundNotices(t, appA, sessionID, "")
	if f6BatchOn() {
		// Count SELECTED notices only: a detached root also persists its
		// STARTED announcement (empty notice kind), which is not a delivery.
		before = pp4SelectedNoticeCount(t, appA, sessionID)
	}
	if !f6BatchOn() {
		require.Equal(t, 1, before, "A's own run must have delivered exactly one notice")
	} else if before == 0 {
		// The inline-elected shape: the completion was the tool result
		// itself; a later selected notice must never exist.
		require.Zero(t, fact.Wake, "an inline-elected completion must not select a later notice")
	} else {
		require.Equal(t, 1, before, "detached: exactly one selected notice delivered in A's own run")
	}

	// A clean exit (unlike scenario а): release the host lock via the real
	// Close path, matching a `rush run` process that finished normally.
	pp4GracefulClose(t, appA)

	resB, err := appB.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason)
	require.EqualValues(t, 1, requestsB.Load(), "B must need only its own plain continuation turn")

	after := countBackgroundNotices(t, appB, sessionID, "")
	if f6BatchOn() {
		after = pp4SelectedNoticeCount(t, appB, sessionID)
	}
	require.Equal(t, before, after, "B must add NO extra notice over an already-delivered job")
}

// TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask is doc sec.6
// scenario (в): cancelling A's run (Ctrl-C) while two tasks are still
// running must produce no further turn; the Ctrl-C itself fabricates no
// terminal state. The GRACEFUL exit that follows (App.Shutdown, the real
// production HostClosing path) COMMITS the confirmed interruption of live
// work (batch design: interrupted + wake=0 during host closing) instead of
// leaving rows running -- the legacy rows-stayed-running invariant is
// deliberately replaced; B's restart then delivers exactly one
// "interrupted" per task and never duplicates one.
func TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	job1, release1 := recoveryJobCommand(t)
	job2, release2 := recoveryJobCommand(t)
	// The body's own appA.Shutdown() below is what kills both blocked
	// loops (CancelAll cancels each job's executor context and the
	// interpreter kill is synchronous); releasing the sentinels here as
	// well is belt and braces for any failure path that skips Shutdown.
	t.Cleanup(func() {
		releaseRecoveryJob(t, release1)
		releaseRecoveryJob(t, release2)
	})
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
				admissionSSEToolCall("start1", "call-1", "bash", `{"command":`+jsonString(job1)+`,"description":"job 1"}`),
				admissionSSEStop("start1", "tool_calls"),
			})
		case 2:
			if !pp4StartedSeen(lastTool, "call-1") {
				t.Errorf("A request 2: no started tool result for call-1: %q", lastTool)
				http.Error(w, "no started tool result", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start2", "call-2", "bash", `{"command":`+jsonString(job2)+`,"description":"job 2"}`),
				admissionSSEStop("start2", "tool_calls"),
			})
		case 3:
			if !pp4StartedSeen(lastTool, "call-2") {
				t.Errorf("A request 3: no started tool result for call-2: %q", lastTool)
				http.Error(w, "no started tool result", http.StatusBadRequest)
				return
			}
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
			if !strings.Contains(string(body), "call-1") || !strings.Contains(string(body), "call-2") {
				t.Errorf("B request 1: both interrupted notices must be in the first prompt")
				http.Error(w, "interrupted notices missing", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("continue", "continuing"), admissionSSEStop("continue", "stop")})
		default:
			t.Errorf("unexpected model call on B (request %d)", requestsB.Load())
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
		f1, ok1 := pp4JobFactRead(appA, sessionID, "call-1")
		f2, ok2 := pp4JobFactRead(appA, sessionID, "call-2")
		return ok1 && ok2 && f1.Found && f2.Found && f1.State == "running" && f2.State == "running"
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

	// Graceful exit through the REAL App shutdown path: CancelAll cancels
	// the still-running interpreters, then the host lock is released --
	// exactly what a real `rush run` process does on exit. Under batch
	// authority this HostClosing COMMITS the confirmed interruption of the
	// live roots (interrupted, wake=0) -- the deliberate lifecycle
	// replacement; the legacy store leaves the rows "running" for B's
	// recovery to interrupt.
	appA.Shutdown()
	if f6BatchOn() {
		for _, id := range []string{"call-1", "call-2"} {
			fact, ok := pp4JobFactRead(appA, sessionID, id)
			require.True(t, ok)
			require.True(t, fact.Found, "job %s row must survive the graceful close", id)
			require.Equal(t, "interrupted", fact.State, "graceful HostClosing must commit the confirmed interruption of %s", id)
			require.EqualValues(t, 0, fact.Wake, "host-closing notices never wake (%s)", id)
		}
	} else {
		row1, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-1")
		require.NoError(t, err)
		require.Equal(t, "running", row1.State, "a graceful exit must never transition a live row")
		row2, err := appA.asyncJobStore.Get(context.Background(), sessionID, "call-2")
		require.NoError(t, err)
		require.Equal(t, "running", row2.State)
	}

	// The sentinel-blocked jobs would keep the rows "running" forever if
	// only the clock passed; any request reaching A's provider in this
	// window would therefore have to come from a botched cancellation, not
	// from a job finishing on its own -- the clock can no longer explain A
	// staying silent.
	require.Never(t, func() bool { return requestsA.Load() > requestsAtCancel }, 5*time.Second, 50*time.Millisecond,
		"no request may reach A's provider after a real CancelAll")

	ctxB, cancelB := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelB()
	resB, err := appB.RunNonInteractiveWithResult(ctxB, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	require.EqualValues(t, 1, requestsB.Load(), "both interrupted notices must be visible in B's FIRST turn")
	if f6BatchOn() {
		require.Equal(t, 2, pp4SelectedNoticeCount(t, appB, sessionID), "one recovered notice PER task (the started announcements carry no notice kind and are not counted), and the restart must not duplicate one")
	} else {
		require.Equal(t, 2, countBackgroundNotices(t, appB, sessionID, "interrupted"), "one interrupted notice PER task")
	}

	for _, id := range []string{"call-1", "call-2"} {
		fact, ok := pp4JobFactRead(appB, sessionID, id)
		require.True(t, ok)
		require.True(t, fact.Found, "job %s row missing after restart", id)
		require.Equal(t, "interrupted", fact.State, "job %s must be interrupted", id)
	}
}

// TestTwoAppScenarioE_AnotherAppPullingDoesNotEraseDebt_RootStillReacts is
// the multi-process-shaped version of internal/agent's
// TestAnotherHolderPulling_DoesNotEraseDebt_RootStillReacts (W-DRAIN B5):
// that test is flagged "single-process" -- it proves the SQL-level fact (a
// pull only ever touches `delivery`, never `reacted`) within ONE process/
// connection. This proves the SAME property survives a GENUINELY SEPARATE
// process: App B, its own *sql.DB connection (refcounted onto the same file)
// and its own message.Service, independently pulls the session's selected
// notice into history -- matching doc sec.3.4's policy-table row "web-tab
// that opened the session: only pull, no turn" -- while App A (the CLI root,
// a DIFFERENT process/connection driving the SAME session) still reacts to
// it on its own next turn, and that reaction is visible in THAT turn's own
// JSON result ("gets it in the result").
func TestTwoAppScenarioE_AnotherAppPullingDoesNotEraseDebt_RootStillReacts(t *testing.T) {
	var requestsA, requestsB atomic.Int32
	jobCommand, jobRelease := recoveryJobCommand(t)
	toolStarted := make(chan struct{})
	var toolStartedOnce sync.Once
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(string(body), "call-1 (bash)") || pp4CompletionSeen(string(body), "call-1"):
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
		case pp4StartedSeen(lastTool, "call-1"):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
			toolStartedOnce.Do(func() { close(toolStarted) })
		default:
			id := pp4NextCallID("call-1", requestsA.Load()+1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", id, "bash", `{"command":`+jsonString(jobCommand)+`,"description":"job 1"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		requestsB.Add(1)
		http.Error(w, "App B must never need its own provider turn in this scenario (pull only)", http.StatusBadRequest)
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)

	// Ctrl-C A's OWN loop while the (sentinel-blocked) async job is still
	// running -- the job's executor is NOT tied to ctxA (doc sec.3.4/3.7:
	// async work survives the turn/loop that started it) and keeps running
	// for real in the background, unattended, exactly like scenario C's own
	// technique. This is what opens a genuine race window: nobody (not even
	// A) reacts to the job's own eventual completion until this test says
	// so.
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
		fact, ok := pp4JobFactRead(appA, sessionID, "call-1")
		return ok && fact.Found && fact.State == "running"
	}, 5*time.Second, 20*time.Millisecond, "the job must be claimed and running before cancellation")

	cancelA()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after ctx cancellation")
	}
	require.Error(t, runErr)

	// Finish the job on purpose (release the sentinel-blocked loop; the
	// executor is unattended -- A's own loop already exited) -- open,
	// unreacted debt nobody is watching yet. The completion itself is then
	// deterministic: one poll iteration after the release at most.
	releaseRecoveryJob(t, jobRelease)
	require.Eventually(t, func() bool {
		fact, ok := pp4JobFactRead(appA, sessionID, "call-1")
		return ok && fact.Found && fact.State == "completed" && fact.Wake != 0 && fact.Reacted == 0
	}, 10*time.Second, 20*time.Millisecond, "the job must finish naturally with open (unreacted) debt")

	// App B -- a GENUINELY SEPARATE process/connection on the same data dir
	// -- independently pulls the SAME notice into history. Must not erase
	// the debt, and must never itself provoke a provider turn.
	require.EqualValues(t, 1, pp4PullNotices(t, appB, sessionID), "App B's independent pull must actually move the notice into history")
	require.Zero(t, requestsB.Load(), "App B's own pull must never itself trigger a provider turn")

	require.True(t, f6a3ReactionDebtExists(t, appB, sessionID), "another process's independent pull must not erase the debt it just made visible")

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
	// requestsA only increments in the reacting branch -- the tool-call-
	// start and yield branches from the first (cancelled) call do not touch
	// it -- so exactly 1 here means exactly one real reacting turn happened,
	// on this second call, never during the first.
	require.EqualValues(t, 1, requestsA.Load(), "the root must have made exactly one real reacting turn")
	require.Contains(t, resA2.FinalText, "root reacted to the job", "the root's reaction must be visible in ITS OWN JSON result")

	require.False(t, f6a3ReactionDebtExists(t, appA, sessionID), "the root's own turn must clear the debt")
}

// recoveryScenarioEBuildJobNoticeParams is a minimal, App-package-local
// stand-in for internal/agent's unexported buildJobNoticeMessageParams:
// enough to prove "the row moved into history" on the legacy OFF path
// (pp4PullNotices routes ON through BatchPullNotices with the
// production-shaped builder instead).
func recoveryScenarioEBuildJobNoticeParams(row session.JobNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) finished: " + row.ResultContent}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}
