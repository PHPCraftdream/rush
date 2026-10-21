// Ordinary batch completion wake bridge (package W): a committed selected
// completion for an idle owner must queue exactly one coordinator wake
// through the bounded timeout-wake dispatcher, without losing the wake when
// the completion commits before the detached started acknowledgement, and
// with wake=0 on shutdown/closing paths.
package app

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pwWakeRecord is one observed coordinator wake delivery.
type pwWakeRecord struct {
	owner string
	fact  bool
}

// pwWakeTimeout bounds every wait on asynchronous dispatcher delivery.
const pwWakeTimeout = 2 * time.Second

// pwAttachWakeRecorder attaches a fake coordinator that records every wake
// delivery into the returned channel and never blocks the dispatcher.
func pwAttachWakeRecorder(d *batchTimeoutWakeDispatcher) chan pwWakeRecord {
	wakes := make(chan pwWakeRecord, 8)
	d.Attach(wakeDispatchTestCoordinator{wake: func(_ context.Context, owner string, fact bool) error {
		wakes <- pwWakeRecord{owner: owner, fact: fact}
		return nil
	}})
	return wakes
}

// pwReserveCompletionRoot reserves a constraint-valid batch root with one
// task child (the same Reserve-based shape the timeout check-in fixtures
// use) and returns the root id.
func pwReserveCompletionRoot(t *testing.T, a *App, owner, callID string) string {
	t.Helper()
	res, err := session.NewBatchStore(a.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: "pw-host", RunnerID: "pw-runner", ToolCallID: callID,
		InputHash: "pw-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: "running", ControlState: "none",
			},
			Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true},
				State: "running", ControlState: "none",
			}}},
		},
	})
	require.NoError(t, err)
	return res.Snapshot.Root.RootID
}

// TestBatchCompletionWake_CommittedSelectedCompletionWakesDurableOwner: a
// completion committed for an idle owner resolves the owner from the
// DURABLE ROW (batch_nodes.owner_session_id), never from host/runner
// identity, and delivers exactly one fact wake.
//
// Mutation caught: resolving the owner from the commit hook identity (the
// dormant owner or the host UUID) instead of the durable row would deliver
// a wake for a wrong or nonexistent session; the first failing assertion is
// the pwReserveCompletionRoot require.NoError (a constraint-invalid seed
// hides everything), then the owner equality below.
func TestBatchCompletionWake_CommittedSelectedCompletionWakesDurableOwner(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	owner, err := a.Sessions.Create(context.Background(), "pw durable owner")
	require.NoError(t, err)
	root := pwReserveCompletionRoot(t, a, owner.ID, "pw-durable-call")

	d := newBatchTimeoutWakeDispatcher()
	d.rootOwner = pwBatchCompletionRootOwner(a)
	t.Cleanup(d.Stop)
	wakes := pwAttachWakeRecorder(d)
	d.WakeCompletion(context.Background(), root)
	select {
	case rec := <-wakes:
		require.Equal(t, owner.ID, rec.owner)
		require.True(t, rec.fact)
	case <-time.After(pwWakeTimeout):
		t.Fatal("no wake delivered for the committed completion root")
	}
	select {
	case rec := <-wakes:
		t.Fatalf("extra wake delivered: %+v", rec)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestBatchCompletionWake_RepeatedCommitWhileInFlightWakesOnce: a repeated
// commit of the same root while the first wake is still being dispatched
// must not queue a second wake (the durable debt is level-readable; the
// coordinator's gates are the real authority).
//
// Mutation caught: losing the dispatcher's in-flight dedupe (releasing the
// queue key before the dispatch completes) would deliver two wakes for one
// committed completion; the first failing assertion is the seed's
// require.NoError, then the count == 1 check on the recorded wakes.
func TestBatchCompletionWake_RepeatedCommitWhileInFlightWakesOnce(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	owner, err := a.Sessions.Create(context.Background(), "pw dedupe owner")
	require.NoError(t, err)
	root := pwReserveCompletionRoot(t, a, owner.ID, "pw-dedupe-call")

	d := newBatchTimeoutWakeDispatcher()
	d.rootOwner = pwBatchCompletionRootOwner(a)
	t.Cleanup(d.Stop)
	entered := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int32
	d.Attach(wakeDispatchTestCoordinator{wake: func(_ context.Context, _ string, _ bool) error {
		count.Add(1)
		close(entered)
		<-release
		return nil
	}})
	d.WakeCompletion(context.Background(), root)
	select {
	case <-entered:
	case <-time.After(pwWakeTimeout):
		t.Fatal("first wake was not dispatched")
	}
	// Second commit of the same root while the first dispatch is in flight.
	d.WakeCompletion(context.Background(), root)
	close(release)
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.queued) == 0 && len(d.pend) == 0
	}, pwWakeTimeout, 10*time.Millisecond)
	require.EqualValues(t, 1, count.Load(), "the repeated commit must not wake twice")
}

// TestBatchCompletionWake_CompletionBeforeCoordinatorAttachIsNotLost: a
// completion may commit BEFORE the detached started acknowledgement attaches
// the coordinator; the early wake must be buffered and replayed on Attach,
// not dropped.
//
// Mutation caught: dropping the wake when no coordinator is attached would
// permanently lose the reaction for an early completion; the first failing
// assertion is the seed's require.NoError, then the delivery timeout below.
func TestBatchCompletionWake_CompletionBeforeCoordinatorAttachIsNotLost(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	owner, err := a.Sessions.Create(context.Background(), "pw early owner")
	require.NoError(t, err)
	root := pwReserveCompletionRoot(t, a, owner.ID, "pw-early-call")

	d := newBatchTimeoutWakeDispatcher()
	d.rootOwner = pwBatchCompletionRootOwner(a)
	t.Cleanup(d.Stop)
	// No coordinator attached yet: the completion commits first.
	d.WakeCompletion(context.Background(), root)
	wakes := pwAttachWakeRecorder(d)
	select {
	case rec := <-wakes:
		require.Equal(t, owner.ID, rec.owner)
	case <-time.After(pwWakeTimeout):
		t.Fatal("the early completion wake was lost: Attach did not replay it")
	}
}

// TestBatchCompletionWake_UnknownRootNeverWakes: a wake request whose root
// has no durable row (never a committed completion) resolves to nothing and
// delivers no wake.
//
// Mutation caught: waking on root-resolution failure (e.g. falling back to
// hook identity) would fabricate an owner out of nothing; the first failing
// assertion is the seed's require.NoError, then the empty-channel check.
func TestBatchCompletionWake_UnknownRootNeverWakes(t *testing.T) {
	a, _, _ := newBatchMaintenanceApp(t)
	d := newBatchTimeoutWakeDispatcher()
	d.rootOwner = pwBatchCompletionRootOwner(a)
	t.Cleanup(d.Stop)
	wakes := pwAttachWakeRecorder(d)
	d.WakeCompletion(context.Background(), "pw-missing-root")
	select {
	case rec := <-wakes:
		t.Fatalf("unknown root produced a wake: %+v", rec)
	case <-time.After(300 * time.Millisecond):
	}
}

// pwInstallWakeApp installs the flagged batch runtime (RUSH_BATCH_RUNTIME=1)
// over a bare maintenance App, captures the install deps (for the Runner
// identity) and attaches a wake-recording coordinator. It returns the app,
// the raw conn and the recorded wakes channel.
func pwInstallWakeApp(t *testing.T) (*App, *sql.DB, agent.BatchInstallDeps, chan pwWakeRecord) {
	t.Helper()
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, conn, _ := newBatchMaintenanceApp(t)
	var deps agent.BatchInstallDeps
	batchInstallDepsHook = func(d *agent.BatchInstallDeps) { deps = *d }
	t.Cleanup(func() { batchInstallDepsHook = nil })
	// DORMANT install: these tests drive the REAL install commit hook with
	// hand-built transitions, so a LIVE Runner must never exist — at
	// shutdown it would commit its own HostClosing/final transitions
	// through the same hook, and their engine sequences (starting at 1)
	// lose the projection fences against the hand-committed higher
	// sequences (AdvanceBatchRootSequence / ProjectBatchNode require a
	// strictly greater sequence), latching a Runner commit fault whose
	// forced shutdown retains the host lock and breaks TempDir cleanup.
	// Runnerless also keeps DrainAndRelease on the graceful releaseAll
	// path (Forced=false on every OS).
	batchRuntimeConfigHook = func(cfg *BatchRuntimeConfig) {
		cfg.RunnerFactory = func() (batchRunner, error) {
			return nil, errors.New("pw completion wake test: dormant install")
		}
	}
	t.Cleanup(func() { batchRuntimeConfigHook = nil })
	require.NoError(t, a.installBatchRuntime(context.Background()))
	a.batchRuntime.cfg.ReleaseWriter = func(context.Context) error { return nil }
	require.NotNil(t, a.batchTimeoutWake, "the flagged install must arm the wake dispatcher")
	require.Nil(t, a.batchRuntime.Runner(), "the pw install must stay runnerless")
	wakes := pwAttachWakeRecorder(a.batchTimeoutWake)
	return a, conn, deps, wakes
}

// pwCommitSettledNotice drives the REAL install commit hook through one
// settled transition that selects the task completion notice (exactly what
// the Runner commits when a leaf settles).
func pwCommitSettledNotice(t *testing.T, a *App, deps agent.BatchInstallDeps, root, node string, sequence uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pwWakeTimeout)
	defer cancel()
	require.NoError(t, a.batchInstall.CommitHook(ctx, jobbatch.Transition{
		Sequence: sequence,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID(node),
			Result: jobbatch.Result{State: jobbatch.StateCompleted},
		},
		Changed: []jobbatch.NodeView{{
			ID: jobbatch.ID(node), Root: jobbatch.ID(root), Kind: jobbatch.KindTask,
			State: jobbatch.StateCompleted, Token: 1,
			Outcome: jobbatch.Outcome{State: jobbatch.StateCompleted},
		}},
		Notices: []jobbatch.Notice{{
			Root: jobbatch.ID(root), Node: jobbatch.ID(node), Kind: jobbatch.KindTask,
			State: jobbatch.StateCompleted, Counts: jobbatch.Counts{Completed: 1},
		}},
	}))
}

// TestBatchCompletionWake_CommitHookSettledNoticeWakesOwner: end to end
// through the production commit hook — a settled transition that selects a
// completion notice wakes the durable owner exactly once, through the same
// bounded dispatcher the timeout check-ins use.
//
// Mutation caught: removing the wrapper wiring (deps.CompletionWake, or the
// pwCompletionWakeHook wrap of NewBatchCommitHook) would leave the durable
// selected notice without any scheduled reaction — the exact reviewer-
// observed "expected 1 got 0" child consumption; the first failing
// assertion is installBatchRuntime's require.NoError, then the wake
// delivery timeout below.
func TestBatchCompletionWake_CommitHookSettledNoticeWakesOwner(t *testing.T) {
	a, _, deps, wakes := pwInstallWakeApp(t)
	defer func() { require.False(t, a.ShutdownWithResult().Forced) }()
	owner, err := a.Sessions.Create(context.Background(), "pw hook owner")
	require.NoError(t, err)
	root, node, _ := pwReserveWakeDelegation(t, a, owner.ID, "pw-hook-call", deps)
	pwCommitSettledNotice(t, a, deps, root, node, 2)
	select {
	case rec := <-wakes:
		require.Equal(t, owner.ID, rec.owner)
		require.True(t, rec.fact)
	case <-time.After(pwWakeTimeout):
		t.Fatal("no wake delivered after the settled completion commit")
	}

	// A second pull of the SAME durable debt never queues another wake:
	// pull consumes the notice into history and dispatches nothing.
	pulled, err := session.NewBatchStore(a.DB()).BatchPullNotices(
		context.Background(), a.Messages, owner.ID,
		func(session.BatchNoticeRow) message.CreateMessageParams {
			return message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "pw pulled completion"}},
			}
		})
	require.NoError(t, err)
	require.NotEmpty(t, pulled)
	select {
	case rec := <-wakes:
		t.Fatalf("the second pull produced another wake: %+v", rec)
	case <-time.After(300 * time.Millisecond):
	}
}

// pwReserveWakeDelegation reserves the fixture root under the INSTALLED
// runner identity so the real commit hook's projection fences match, with
// TWO task children so two distinct transitions can each select one notice
// (SelectBatchNodeNotice refuses a node whose notice is already selected),
// and returns the root and the two task node ids.
func pwReserveWakeDelegation(t *testing.T, a *App, owner, callID string, deps agent.BatchInstallDeps) (string, string, string) {
	t.Helper()
	host := a.BatchHostStore().HostID()
	res, err := session.NewBatchStore(a.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: deps.RunnerID, ToolCallID: callID,
		InputHash: "pw-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{
				Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
				State: "running", ControlState: "none",
			},
			Children: []session.BatchReservationNode{
				{Values: db.ReserveBatchNodeParams{
					Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true},
					State: "running", ControlState: "none",
				}},
				{Values: db.ReserveBatchNodeParams{
					Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true},
					State: "running", ControlState: "none",
				}},
			},
		},
	})
	require.NoError(t, err)
	root, node1, node2 := res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID, res.Snapshot.Nodes[2].NodeID
	// Announce the started root: the pull predicate only surfaces notices
	// whose root is announced (batchOutboxPendingVoidAware).
	start, err := session.NewBatchStore(a.DB()).BatchAnnounceStarted(
		context.Background(), a.Messages, owner, root,
		message.CreateMessageParams{
			Role:                message.User,
			Parts:               []message.ContentPart{message.TextContent{Text: "pw fixture delegation started"}},
			BackgroundJobNotice: true,
		})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, node1, node2
}

// TestBatchCompletionWake_ShutdownClosingCommitsWakeZero: a HostClosing
// commit — and every later interrupted settlement after it — must wake=0:
// shutdown and recovery launch no paid reaction.
//
// Mutation caught: dropping the wrapper's sticky closing mirror would wake
// the owner from closing settlements (a paid turn launched by shutdown or
// recovery); the first failing assertion is installBatchRuntime's
// require.NoError, then the empty-wake checks below.
func TestBatchCompletionWake_ShutdownClosingCommitsWakeZero(t *testing.T) {
	a, _, deps, wakes := pwInstallWakeApp(t)
	defer func() { require.False(t, a.ShutdownWithResult().Forced) }()
	owner, err := a.Sessions.Create(context.Background(), "pw closing owner")
	require.NoError(t, err)
	root, nodeClosing, nodeAfter := pwReserveWakeDelegation(t, a, owner.ID, "pw-closing-call", deps)

	ctx, cancel := context.WithTimeout(context.Background(), pwWakeTimeout)
	defer cancel()
	// The host-closing commit itself selects notices: wake=0.
	require.NoError(t, a.batchInstall.CommitHook(ctx, jobbatch.Transition{
		Sequence: 2,
		Event:    jobbatch.Event{Type: jobbatch.EventHostClosing},
		Changed: []jobbatch.NodeView{{
			ID: jobbatch.ID(nodeClosing), Root: jobbatch.ID(root), Kind: jobbatch.KindTask,
			State: jobbatch.StateInterrupted, Token: 1,
			Outcome: jobbatch.Outcome{State: jobbatch.StateInterrupted},
		}},
		Notices: []jobbatch.Notice{{
			Root: jobbatch.ID(root), Node: jobbatch.ID(nodeClosing), Kind: jobbatch.KindTask,
			State: jobbatch.StateInterrupted, Counts: jobbatch.Counts{Interrupted: 1},
		}},
	}))
	// A later interrupted settlement after closing must also wake=0. It
	// selects the SECOND child's notice: re-selecting the SAME node is a
	// projection conflict (SelectBatchNodeNotice requires notice_selected=0).
	require.NoError(t, a.batchInstall.CommitHook(ctx, jobbatch.Transition{
		Sequence: 3,
		Event: jobbatch.Event{
			Type: jobbatch.EventSettled, Node: jobbatch.ID(nodeAfter),
			Result: jobbatch.Result{State: jobbatch.StateInterrupted},
		},
		Changed: []jobbatch.NodeView{{
			ID: jobbatch.ID(nodeAfter), Root: jobbatch.ID(root), Kind: jobbatch.KindTask,
			State: jobbatch.StateInterrupted, Token: 2,
			Outcome: jobbatch.Outcome{State: jobbatch.StateInterrupted},
		}},
		Notices: []jobbatch.Notice{{
			Root: jobbatch.ID(root), Node: jobbatch.ID(nodeAfter), Kind: jobbatch.KindTask,
			State: jobbatch.StateInterrupted, Counts: jobbatch.Counts{Interrupted: 1},
		}},
	}))
	select {
	case rec := <-wakes:
		t.Fatalf("shutdown/recovery path launched a wake: %+v", rec)
	case <-time.After(300 * time.Millisecond):
	}
}
