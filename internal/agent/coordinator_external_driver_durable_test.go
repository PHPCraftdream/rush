// Durable external-driver marker, agent layer (docs/reviews/2026-09-29-async-
// phase4-round1-rerun-design.md, Problem 2): while a `rush run` loop in
// ANOTHER process drives a session (its session_drivers row names a live
// host) this process never starts a reaction turn for it -- neither a
// submitted Drain (wakeSession) nor an already-admitted one at its turn start
// (decideDrainTurn: it only transfers notices) -- and parks the session in
// the 60s recheck set while debt exists. Real SQLite, real session agent.
package agent

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// foreignDriverFx is a wakeDebtFixture whose store shares its database and
// data dir with a second store, cli, standing in for the driving `rush run`
// process.
type foreignDriverFx struct {
	*wakeDebtFixture
	cli     *session.AsyncJobStore
	conn    *sql.DB
	dataDir string
}

func newForeignDriverFx(t *testing.T, title string) *foreignDriverFx {
	t.Helper()
	f := &wakeDebtFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		_, _ = io.ReadAll(r.Body)
		textFinishResponse(w, "reacted")
	}))
	t.Cleanup(f.srv.Close)
	f.model = newProbeModel(t, f.srv)

	env := testEnv(t)
	sess, err := env.sessions.Create(context.Background(), title)
	require.NoError(t, err)
	f.sessID = sess.ID

	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	f.store = store
	f.coord = &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
	f.ledger.store = f.store
	f.ledger.coord = f.coord
	f.coord.asyncJobs = f.ledger

	f.messages = &failingCreateTxMessages{Service: env.messages}
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: f.model, FastModel: f.model, SystemPrompt: "you are a probe",
		Sessions: env.sessions, Messages: f.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: f.ledger,
		OnSessionIdle: f.coord.onSessionIdleHook,
	})
	f.sa = sa.(*sessionAgent)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})

	cli := session.NewAsyncJobStore(conn, dataDir, os.Getpid()+1, "cli")
	t.Cleanup(func() { _ = cli.Close(context.Background()) })
	return &foreignDriverFx{wakeDebtFixture: f, cli: cli, conn: conn, dataDir: dataDir}
}

func (f *foreignDriverFx) inRecheckSet(id string) bool {
	f.coord.recheckMu.Lock()
	defer f.coord.recheckMu.Unlock()
	_, ok := f.coord.recheckSet[id]
	return ok
}

// TestSessionDrainPolicy_ForeignDriver_NoDrainParkedWhileDebt: with a live
// foreign driver the policy refuses, no provider call happens, and the session
// is parked in the recheck set exactly when reaction debt exists.
//
// Revert-check: remove the foreign-driver branch from drainPolicy --
// the wake submits a Drain and the provider is called.
func TestSessionDrainPolicy_ForeignDriver_NoDrainParkedWhileDebt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no_debt_not_parked", func(t *testing.T) {
		t.Parallel()
		f := newForeignDriverFx(t, "foreign-no-debt")
		require.NoError(t, f.cli.ClaimSessionDriver(ctx, f.sessID))

		allowed, err := policyAllowed(f.coord, ctx, f.sessID)
		require.NoError(t, err)
		require.False(t, allowed)
		require.False(t, f.inRecheckSet(f.sessID), "without debt there is nothing to retry later")
	})

	t.Run("debt_parked_and_no_turn", func(t *testing.T) {
		t.Parallel()
		f := newForeignDriverFx(t, "foreign-debt")
		f.claimAndFinish(t, ctx, "call-1") // pending, wake=1
		require.NoError(t, f.cli.ClaimSessionDriver(ctx, f.sessID))

		require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true))

		require.Zero(t, f.requests.Load(), "no reaction turn may start in a process that does not drive the session")
		require.True(t, f.inRecheckSet(f.sessID), "debt must keep the session in the 60s recheck set")
		row, err := f.store.Get(ctx, f.sessID, "call-1")
		require.NoError(t, err)
		require.Equal(t, "pending", row.Delivery, "the notice is left for the driver")
	})
}

// TestDecideDrainTurn_ForeignDriver_TransferOnly: a Drain admitted before the
// driver claimed (or by a race) still pulls the notice into history at its
// turn start, but decideDrainTurn refuses the provider turn: notices are
// transferred, nothing is reacted to, and the debt stays for the driver.
//
// Revert-check: remove the foreign-driver branch from drainPolicy --
// the Drain calls the provider and marks the debt reacted.
func TestDecideDrainTurn_ForeignDriver_TransferOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForeignDriverFx(t, "foreign-transfer-only")
	f.claimAndFinish(t, ctx, "call-1")
	require.NoError(t, f.cli.ClaimSessionDriver(ctx, f.sessID))

	call, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	_, _ = f.sa.Run(ctx, call) // an already-admitted Drain reaching its turn start

	row, err := f.store.Get(ctx, f.sessID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery, "the admitted Drain transfers the notice into history")
	require.EqualValues(t, 0, row.Reacted, "but never reacts to it: the driver does")
	require.Zero(t, f.requests.Load(), "the provider must not be called")
	visible, err := f.store.VisibleReactionDebtExists(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, visible, "the debt stays visible for the driving loop")
}

// TestSessionDrainPolicy_OwnLoopNotBlocked: this process's OWN durable marker
// (its loop's claim) is never foreign, even with the in-memory marker
// cleared: the lookup must not refuse the loop's own turns.
//
// Revert-check: drop the own-host check from ForeignLiveDriver -- the own
// host reads as an alive foreign one and the policy refuses.
func TestSessionDrainPolicy_OwnLoopNotBlocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForeignDriverFx(t, "own-loop")
	require.NoError(t, f.store.ClaimSessionDriver(ctx, f.sessID)) // durable row, own host
	require.False(t, f.ledger.isExternalDriver(f.sessID), "precondition: in-memory marker not set")

	allowed, err := policyAllowed(f.coord, ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, allowed, "the loop's own durable marker must not block its own turns")
}

// TestSessionDrainPolicy_DriverMarkerUnreadable: a marker read error fails
// CLOSED for every coordinator (web and CLI alike -- there is no persistentMode
// split any more): the policy refuses, names the read error and asks for a
// re-check tick, and a wake with debt parks the session in the recheck set.
//
// Revert-check: answering "allow" on the marker read error (the old CLI
// fail-open) turns the cli case red.
func TestSessionDrainPolicy_DriverMarkerUnreadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	broken := func(t *testing.T) *foreignDriverFx {
		f := newForeignDriverFx(t, "unreadable-marker")
		_, err := f.conn.ExecContext(ctx, `DROP TABLE session_drivers`)
		require.NoError(t, err)
		return f
	}

	for _, mode := range []string{"web", "cli"} {
		t.Run(mode+"_fails_closed", func(t *testing.T) {
			t.Parallel()
			f := broken(t)
			f.coord.persistentMode.Store(mode == "web")
			f.claimAndFinish(t, ctx, "call-1")

			v := f.coord.drainPolicy(ctx, f.sessID)
			require.Equal(t, drainDeferred, v.kind)
			require.True(t, v.recheck)
			require.Error(t, v.err)

			require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true))
			require.Zero(t, f.requests.Load(), "an unreadable policy must never start a turn")
			require.True(t, f.inRecheckSet(f.sessID), "the tick must retry")
		})
	}
}

// TestClaimExternalDriver_DurableClaimAndRefusal: the claim writes the durable
// marker and sets the in-memory one; a session driven by a live foreign loop
// refuses with the typed error and sets nothing.
func TestClaimExternalDriver_DurableClaimAndRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForeignDriverFx(t, "claim-refusal")

	require.NoError(t, f.cli.ClaimSessionDriver(ctx, f.sessID))
	err := f.coord.ClaimExternalDriver(ctx, f.sessID)
	var elsewhere *session.ErrSessionDrivenElsewhere
	require.ErrorAs(t, err, &elsewhere)
	require.False(t, f.ledger.isExternalDriver(f.sessID), "a refused claim must not set the in-memory marker")

	require.NoError(t, f.cli.ReleaseSessionDriver(ctx, f.sessID))
	require.NoError(t, f.coord.ClaimExternalDriver(ctx, f.sessID))
	require.True(t, f.ledger.isExternalDriver(f.sessID))
	_, foreign, err := f.cli.ForeignLiveDriver(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, foreign, "the durable marker now names the coordinator's own host")
}

// TestClaimExternalDriver_MarkerUnavailableFallsBackToMemory: a data dir that
// cannot host a lock file keeps the old in-memory-only behaviour instead of
// failing the run.
func TestClaimExternalDriver_MarkerUnavailableFallsBackToMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForeignDriverFx(t, "claim-unavailable")
	// A file where the hosts directory must be: no lock file can be created.
	require.NoError(t, os.RemoveAll(session.HostsDir(f.dataDir)))
	require.NoError(t, os.WriteFile(session.HostsDir(f.dataDir), []byte("x"), 0o644))

	require.NoError(t, f.coord.ClaimExternalDriver(ctx, f.sessID))
	require.True(t, f.ledger.isExternalDriver(f.sessID), "the in-memory marker must still be set")
}

// TestReleaseExternalDriver_DurableReleasedEvenWhenNonPersistent: C4 keeps a
// non-persistent (CLI) coordinator's IN-MEMORY marker for the process
// lifetime, but the DURABLE row must be released on loop exit regardless --
// it is what the web process consults.
//
// Revert-check: put the durable release behind the persistentMode guard.
func TestReleaseExternalDriver_DurableReleasedEvenWhenNonPersistent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newForeignDriverFx(t, "release-durable")
	require.NoError(t, f.coord.ClaimExternalDriver(ctx, f.sessID))

	f.coord.ReleaseExternalDriver(ctx, f.sessID)

	_, foreign, err := f.cli.ForeignLiveDriver(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, foreign, "the durable marker must be gone")
	require.True(t, f.ledger.isExternalDriver(f.sessID), "C4: the in-memory marker of a non-persistent coordinator stays")
}

// TestClaimExternalDriver_StartsRecheckTicker: a `rush run` claims its session
// through ClaimExternalDriver, which also starts the same 60s pass the web
// process runs (recheck set, parked delegations, maintenance): the CLI root
// itself is a hint-only no-op for it, but its delegated children's retries
// ride it. CancelAll stops it.
//
// Revert-check: dropping StartRecheckTicker from ClaimExternalDriver leaves
// recheckStop nil and this test red.
func TestClaimExternalDriver_StartsRecheckTicker(t *testing.T) {
	ctx := context.Background()
	f := newForeignDriverFx(t, "claim-starts-ticker")
	f.coord.currentAgent = &mockSessionAgent{}
	require.Nil(t, f.coord.recheckStop, "precondition: no ticker before the claim")

	require.NoError(t, f.coord.ClaimExternalDriver(ctx, f.sessID))
	require.NotNil(t, f.coord.recheckStop, "the claim starts the recheck pass")
	done := f.coord.recheckDone

	f.coord.CancelAll()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CancelAll must stop the ticker the claim started")
	}
}
