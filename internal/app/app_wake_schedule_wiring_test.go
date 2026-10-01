// Stage-4c wiring gap (task item 5): InitCoderAgent really STARTS the
// durable wake-schedule worker over the App's store, and the release path
// really STOPS it — proven by observable effects, not by poking internals:
// a schedule seeded due before New fires on its own once the worker is up
// (require.Eventually, no sleeps), and after StopWakeScheduler returns (a
// synchronous, deterministic handoff) a freshly due schedule stays
// unclaimed (require.Never).
package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newWakeWiringApp(t *testing.T, seed func(q *db.Queries, conn *sql.DB) error) *App {
	t.Helper()
	isolateAppNewTestEnv(t)

	// Minimal SSE chat-completion stub so InitCoderAgent's coordinator
	// construction validates (same fixture as
	// TestAppNew_DefaultPath_StillRunsRecoveryAndInitCoderAgent).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
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
		Provider: "openaicompat", Model: "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat", Model: "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	if seed != nil {
		require.NoError(t, seed(db.New(conn), conn))
	}
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() { _ = application.ShutdownWithResult() })
	require.NotNil(t, application.AgentCoordinator)
	return application
}

func TestWakeScheduleWorker_WiredByInitCoderAgentAndStoppedOnRelease(t *testing.T) {
	// Seed the owner session and a due once schedule BEFORE New: the
	// worker's startup NextDue read then arms directly onto it, so the
	// observable fire happens within seconds without any Notify hook.
	var owner, rowID string
	application := newWakeWiringApp(t, func(q *db.Queries, conn *sql.DB) error {
		sessions := session.NewService(q, conn)
		sess, err := sessions.Create(context.Background(), "wake-wiring-owner")
		if err != nil {
			return err
		}
		owner = sess.ID
		wake := session.NewWakeScheduleStore(conn)
		row, err := wake.CreateSchedule(context.Background(), session.CreateWakeScheduleParams{
			Owner: owner, Kind: session.WakeKindOnce, Message: "wiring probe",
			RunAt: time.Now().Add(session.MinWakeOnceDelay),
		}, time.Now())
		if err != nil {
			return err
		}
		rowID = row.ID
		return nil
	})
	require.NotEmpty(t, owner, "precondition: the seeded owner session id")
	require.NotEmpty(t, rowID, "precondition: the seeded schedule id")

	ctx := context.Background()
	q := db.New(application.DB())
	wake := session.NewWakeScheduleStore(application.DB())

	ctrl, ok := application.AgentCoordinator.(agent.WakeScheduleController)
	require.True(t, ok, "the coordinator must implement WakeScheduleController so releaseResources can stop the worker")

	// The worker runs: the due once schedule fires on its own and its
	// wake_fired notice lands in session_notices.
	require.Eventually(t, func() bool {
		got, err := q.GetWakeSchedule(ctx, rowID)
		if err != nil || got.State != "done" {
			return false
		}
		notices, err := q.ListSessionNoticesForOwner(ctx, owner)
		if err != nil {
			return false
		}
		for _, n := range notices {
			if n.Kind == session.NoticeKindWakeFired {
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond,
		"the worker started by InitCoderAgent must fire a due schedule and enqueue its wake_fired notice")

	// Stop is synchronous: once it returns, the worker goroutine is gone.
	// A freshly due schedule must therefore stay active (unclaimed,
	// unfired).
	ctrl.StopWakeScheduler()
	late, err := wake.CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: owner, Kind: session.WakeKindOnce, Message: "after stop",
		RunAt: time.Now().Add(session.MinWakeOnceDelay),
	}, time.Now())
	require.NoError(t, err)
	got, err := q.GetWakeSchedule(ctx, late.ID)
	require.NoError(t, err)
	require.Equal(t, "active", got.State)
	require.Never(t, func() bool {
		got, err := q.GetWakeSchedule(ctx, late.ID)
		return err == nil && got.State != "active"
	}, 500*time.Millisecond, 50*time.Millisecond,
		"after StopWakeScheduler the worker must never claim or fire anything again")
}
