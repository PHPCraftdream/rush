// Durable external-driver marker scenarios (docs/reviews/2026-09-29-async-
// phase4-round1-rerun-design.md, Problem 2), two *App instances on one data
// dir with real probe providers -- the closest one test process gets to a web
// server (W) and a `rush run` loop (C) on the same session:
//
//	(е) W owns an async bash on S; C loops `--continue S`. The job finishes
//	    in W: W must NOT react (its provider stays untouched); C's loop sees
//	    the debt and the root answers IN THE CLI.
//	(ж) the driving CLI dies (kill -9: no release): W's next pass sees the
//	    host dead and reacts itself.
//	(ж2) same, but the CLI dies BEFORE the job finishes: W reacts on the
//	    completion hint itself, without waiting for the 60s pass.
package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// driverScenarioHandlers builds W's provider (tool call, yield, and a counted
// reaction) and C's provider (plain continuation, or the reaction when the
// job notice is in the last user message). The returned release path
// unblocks the job's sentinel-blocked command (recoveryJobCommand) at the
// point in each scenario where the job is meant to finish; tests that never
// start the job ignore it.
func driverScenarioHandlers(t *testing.T, requestsW, requestsC *atomic.Int32) (w, c http.HandlerFunc, jobRelease string) {
	blockedCommand, jobRelease := recoveryJobCommand(t)
	w = func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		requestsW.Add(1)
		switch {
		case strings.Contains(lastTool, "Async bash job call-1 started"):
			admissionWriteSSE(rw, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		case strings.Contains(lastUser, "call-1 (bash)"):
			admissionWriteSSE(rw, []string{admissionSSEText("wreact", "web reacted to the job"), admissionSSEStop("wreact", "stop")})
		default:
			admissionWriteSSE(rw, []string{
				admissionSSEToolCall("start", "call-1", "bash",
					`{"command":`+jsonString(blockedCommand)+`,"description":"long job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}
	c = func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, _ := lastTurnParts(body)
		requestsC.Add(1)
		if strings.Contains(lastUser, "call-1 (bash)") {
			admissionWriteSSE(rw, []string{admissionSSEText("creact", "the job finished; answered in the CLI"), admissionSSEStop("creact", "stop")})
			return
		}
		admissionWriteSSE(rw, []string{admissionSSEText("cont", "continuing"), admissionSSEStop("cont", "stop")})
	}
	return w, c, jobRelease
}

// startWebJob runs W's first turn: an async bash that outlives it.
func startWebJob(t *testing.T, ctx context.Context, appW *App, sessionID string) {
	t.Helper()
	_, err := appW.ExecuteRun(ctx, RunRequest{
		Prompt: "start a long job", Origin: message.OriginCLI, Overrides: RunOverrides{Origin: message.OriginCLI},
		Mode: RunModeJSON, ContinueSessionID: sessionID, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err)
}

func jobRow(t *testing.T, app *App, sessionID string) db.AsyncJob {
	t.Helper()
	row, err := app.asyncJobStore.Get(context.Background(), sessionID, "call-1")
	require.NoError(t, err)
	return row
}

func driverSource(t *testing.T, app *App) agent.ReactionDebtSource {
	t.Helper()
	src, ok := app.AgentCoordinator.(agent.ReactionDebtSource)
	require.True(t, ok)
	return src
}

// TestTwoAppScenarioE_WebJobWhileCLIDrives_RootAnswersInCLI: W's job finishes
// while C's loop drives the session -- W must not react (its provider count
// stays at the two requests of its own turn), C's loop reacts in its own
// process and its JSON FinalText is that reaction.
//
// Revert-check: remove the foreign-driver branch from sessionDrainPolicy --
// W submits a Drain and reacts (W's provider gets a third request).
func TestTwoAppScenarioE_WebJobWhileCLIDrives_RootAnswersInCLI(t *testing.T) {
	var requestsW, requestsC atomic.Int32
	handlerW, handlerC, jobRelease := driverScenarioHandlers(t, &requestsW, &requestsC)
	appW, appC, sessionID := newRecoveryTwoAppHarness(t, handlerW, handlerC)
	// Release the sentinel before the harness's own deferred Shutdowns run
	// (t.Cleanup is LIFO): the job either exits on its own or is killed
	// outright, never left blocking past the TempDir removal.
	t.Cleanup(func() {
		releaseRecoveryJob(t, jobRelease)
		appW.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	startWebJob(t, ctx, appW, sessionID)
	require.EqualValues(t, 2, requestsW.Load(), "W's own turn: tool call + yield")
	require.Equal(t, "running", jobRow(t, appW, sessionID).State, "the job must outlive W's turn")
	// Let the sentinel-blocked job finish now; the completion is one poll
	// iteration away instead of a wall-clock estimate.
	releaseRecoveryJob(t, jobRelease)

	resC, err := appC.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resC)

	require.EqualValues(t, 2, requestsC.Load(), "C: its own continuation turn plus the reaction turn")
	require.Contains(t, resC.FinalText, "answered in the CLI", "the root's answer must come from the CLI loop")
	require.EqualValues(t, 2, requestsW.Load(), "W must not have reacted: a live CLI loop drives the session")
	row := jobRow(t, appW, sessionID)
	require.Equal(t, "completed", row.State)
	require.EqualValues(t, 1, row.Reacted, "the reaction was recorded")

	// The loop released its durable marker on exit.
	_, err = db.New(appC.DB()).GetSessionDriver(ctx, sessionID)
	require.ErrorIs(t, err, sql.ErrNoRows, "the exited loop must not leave a driver marker behind")
}

// TestTwoAppScenarioF_DriverCrash_WebTakesOver: the driving CLI is killed
// without releasing its marker after W's job finished (W refused to react
// meanwhile). W's next 60s pass finds the host dead and reacts itself.
//
// Revert-check: make ForeignLiveDriver treat every foreign marker as live
// AND drop purgeDeadSessionDrivers from PurgeExpired -- W never reacts.
func TestTwoAppScenarioF_DriverCrash_WebTakesOver(t *testing.T) {
	var requestsW, requestsC atomic.Int32
	handlerW, handlerC, jobRelease := driverScenarioHandlers(t, &requestsW, &requestsC)
	appW, appC, sessionID := newRecoveryTwoAppHarness(t, handlerW, handlerC)
	// Release the sentinel before the harness's own deferred Shutdowns run
	// (t.Cleanup is LIFO): the job either exits on its own or is killed
	// outright, never left blocking past the TempDir removal.
	t.Cleanup(func() {
		releaseRecoveryJob(t, jobRelease)
		appW.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	startWebJob(t, ctx, appW, sessionID)
	// C claims the session as its driver (the loop's first step) ...
	require.NoError(t, driverSource(t, appC).ClaimExternalDriver(ctx, sessionID))
	releaseRecoveryJob(t, jobRelease)

	// ... the job finishes while C drives: W parks the wake instead of reacting.
	require.Eventually(t, func() bool { return jobRow(t, appW, sessionID).State == "completed" },
		20*time.Second, 50*time.Millisecond, "the job must finish in W")
	require.Never(t, func() bool { return requestsW.Load() > 2 }, 500*time.Millisecond, 20*time.Millisecond,
		"W must not react while a live CLI drives the session")
	require.EqualValues(t, 0, jobRow(t, appW, sessionID).Reacted)

	// kill -9 of C: the host lock drops, the marker row stays.
	require.NoError(t, appC.asyncJobStore.SimulateCrashForTest())

	appW.AgentCoordinator.(interface{ RecheckPass(context.Context) }).RecheckPass(ctx)

	require.Eventually(t, func() bool { return jobRow(t, appW, sessionID).Reacted == 1 },
		20*time.Second, 50*time.Millisecond, "W must take the session over and react")
	require.EqualValues(t, 3, requestsW.Load(), "W's reaction turn")
	require.Zero(t, requestsC.Load(), "the dead CLI's provider is never called")
}

// TestTwoAppScenarioF2_DriverCrashBeforeCompletion_WebReactsOnHint: the CLI
// dies while the job is still running; when the job then finishes, W's own
// completion hint already sees the dead driver and reacts -- no waiting for
// the 60s pass.
//
// Revert-check: make ForeignLiveDriver treat a dead host as alive (skip the
// liveness probe) -- W parks the wake and never reacts on the hint.
func TestTwoAppScenarioF2_DriverCrashBeforeCompletion_WebReactsOnHint(t *testing.T) {
	var requestsW, requestsC atomic.Int32
	handlerW, handlerC, jobRelease := driverScenarioHandlers(t, &requestsW, &requestsC)
	appW, appC, sessionID := newRecoveryTwoAppHarness(t, handlerW, handlerC)
	// Release the sentinel before the harness's own deferred Shutdowns run
	// (t.Cleanup is LIFO): the job either exits on its own or is killed
	// outright, never left blocking past the TempDir removal.
	t.Cleanup(func() {
		releaseRecoveryJob(t, jobRelease)
		appW.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	startWebJob(t, ctx, appW, sessionID)
	require.NoError(t, driverSource(t, appC).ClaimExternalDriver(ctx, sessionID))
	require.Equal(t, "running", jobRow(t, appW, sessionID).State)

	require.NoError(t, appC.asyncJobStore.SimulateCrashForTest())
	releaseRecoveryJob(t, jobRelease)

	require.Eventually(t, func() bool { return jobRow(t, appW, sessionID).Reacted == 1 },
		30*time.Second, 50*time.Millisecond, "W must react on the completion hint: the driver is dead")
	require.EqualValues(t, 3, requestsW.Load())
}

// TestRunLoop_DrivenSession_FailsBeforeTurnOrMutation: a second `rush run` on
// a session another live loop drives fails fast with the typed error naming
// the driver, before any provider call and before ExecuteRun's session writes
// (system prompt, ended_reason) -- a refused claim changes nothing.
//
// Revert-check: ignore the claim error in the loop's onSessionResolved.
func TestRunLoop_DrivenSession_FailsBeforeTurnOrMutation(t *testing.T) {
	var requestsW, requestsC atomic.Int32
	handlerW, handlerC, jobRelease := driverScenarioHandlers(t, &requestsW, &requestsC)
	appDriver, appLate, sessionID := newRecoveryTwoAppHarness(t, handlerW, handlerC)
	// Release the sentinel before the harness's own deferred Shutdowns run
	// (t.Cleanup is LIFO): the job either exits on its own or is killed
	// outright, never left blocking past the TempDir removal.
	t.Cleanup(func() {
		releaseRecoveryJob(t, jobRelease)
		appDriver.Shutdown()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, driverSource(t, appDriver).ClaimExternalDriver(ctx, sessionID))
	before, err := appLate.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)

	res, err := appLate.RunNonInteractiveWithResult(ctx, io.Discard, "second loop", RunOverrides{
		Origin: message.OriginCLI, SystemPrompt: "a different system prompt",
	}, true, RunModeJSON, sessionID, false)

	var elsewhere *session.ErrSessionDrivenElsewhere
	require.ErrorAs(t, err, &elsewhere, "the second loop must be refused with the typed error")
	require.Equal(t, appDriver.asyncJobStore.HostID(), elsewhere.HostID)
	require.Nil(t, res, "no turn ran, so there is no result")
	require.Zero(t, requestsC.Load(), "the provider must not be called")
	after, err := appLate.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, before.SystemPrompt, after.SystemPrompt, "a refused claim must not persist the system prompt override")
	require.Equal(t, before.EndedReason, after.EndedReason, "a refused claim must not touch ended_reason")
	require.Equal(t, before.UpdatedAt, after.UpdatedAt)

	// The refused loop must not have disturbed the live driver's marker.
	row, err := db.New(appLate.DB()).GetSessionDriver(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, appDriver.asyncJobStore.HostID(), row.HostID)
}

// TestRunLoop_DurableDriverReleasedInNonPersistentMode: C4 keeps a
// non-persistent (CLI) coordinator's in-memory marker for its process
// lifetime, but the loop's exit must still delete the DURABLE marker --
// otherwise the row would keep naming a host that stays alive until the
// process really exits (a shutdown that may take a while).
//
// Revert-check: put the durable release behind the persistentMode guard.
func TestRunLoop_DurableDriverReleasedInNonPersistentMode(t *testing.T) {
	var requestsW, requestsC atomic.Int32
	handlerW, handlerC, _ := driverScenarioHandlers(t, &requestsW, &requestsC)
	_, appC, sessionID := newRecoveryTwoAppHarness(t, handlerW, handlerC)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := appC.RunNonInteractiveWithResult(ctx, io.Discard, "just answer", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1, requestsC.Load())

	_, err = db.New(appC.DB()).GetSessionDriver(ctx, sessionID)
	require.True(t, errors.Is(err, sql.ErrNoRows), "the loop's exit must delete the durable marker even in non-persistent mode; got %v", err)
}
