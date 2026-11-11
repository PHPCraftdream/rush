package agent_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

type supervisionNoticeBindingDiagnostic struct {
	stage            string
	caseName         string
	storeBound       bool
	generation       uint64
	ticks            int
	paused           bool
	noticeCount      int
	readErr          bool
	gotKind          string
	gotWake          int64
	gotDrain         bool
	gotRuns          int32
	helperErrorClass string
	httpRequests     atomic.Int32
}

func TestAppSupervisionNoticeStoreBinding(t *testing.T) {
	cases := []struct {
		name       string
		fireGen    uint64
		reserve    bool
		busy       bool
		expectWake bool
	}{
		{name: "positive", fireGen: 1, reserve: true, expectWake: true},
		{name: "stale", fireGen: 2, reserve: true},
		{name: "closed", fireGen: 1},
		{name: "busy", fireGen: 1, reserve: true, busy: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diag := &supervisionNoticeBindingDiagnostic{
				stage:            "config",
				caseName:         tc.name,
				noticeCount:      -1,
				gotKind:          "none",
				helperErrorClass: "none",
			}
			// Register first so any setup/assertion failure gets the same
			// bounded, redacted diagnostics as a behavioral failure.
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("supervision notice diagnostic case=%s stage=%s store_bound=%t generation=%d ticks=%d paused=%t notice_count=%d read_err=%t got_kind=%s got_wake=%d got_drain=%t got_runs=%d helper_error_class=%s provider_requests=%d",
						diag.caseName, diag.stage, diag.storeBound, diag.generation,
						diag.ticks, diag.paused, diag.noticeCount, diag.readErr,
						diag.gotKind, diag.gotWake, diag.gotDrain, diag.gotRuns,
						diag.helperErrorClass, diag.httpRequests.Load())
				}
			})

			tctx := context.Background()
			isolationDir := t.TempDir()
			t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(isolationDir, "global-data"))
			t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(isolationDir, "global-config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(isolationDir, "xdg-data"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolationDir, "xdg-config"))
			t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				diag.httpRequests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			// Registered before the first assertion/setup result is checked.
			t.Cleanup(srv.Close)

			diag.stage = "config"
			dataDir := t.TempDir()
			store, err := config.Init(dataDir, dataDir, false)
			if err != nil {
				diag.helperErrorClass = "config-init"
				t.Fatalf("isolated config initialization failed")
			}
			if store.Config().Options == nil {
				diag.helperErrorClass = "config-options"
				t.Fatalf("configured app options are unavailable")
			}
			store.Config().Options.NoRealWorkspace = true
			store.Config().Options.DisableProviderAutoUpdate = true
			store.Config().Providers.Set("openaicompat", config.ProviderConfig{
				ID: "openaicompat", Type: openaicompat.Name,
				BaseURL: srv.URL, APIKey: "offline-probe",
				Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
			})
			store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
			store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
			store.SetupAgents()

			diag.stage = "connect"
			conn, err := db.Connect(tctx, dataDir)
			if err != nil {
				diag.helperErrorClass = "db-connect"
				t.Fatalf("isolated database initialization failed")
			}
			// App shutdown is registered later and therefore runs before this
			// connection release.
			t.Cleanup(func() { _ = db.ReleaseConn(conn) })

			diag.stage = "app-new"
			a, err := app.New(tctx, conn, store, app.SkipMCP())
			if err != nil {
				diag.helperErrorClass = "app-new"
				t.Fatalf("configured application construction failed")
			}
			t.Cleanup(func() {
				a.Shutdown()
				if got := diag.httpRequests.Load(); got != 0 {
					if !t.Failed() {
						diag.stage = "post-shutdown-provider-check"
					}
					t.Errorf("provider endpoint unexpectedly received %d requests", got)
				}
			})

			diag.stage = "createowner"
			owner, err := a.Sessions.Create(tctx, "supervision notice binding fixture")
			if err != nil {
				diag.helperErrorClass = "session-create"
				t.Fatalf("owner session creation failed")
			}
			if tc.reserve {
				diag.stage = "reserve"
				if a.BatchHostStore() == nil {
					diag.helperErrorClass = "batch-host-store"
					t.Fatalf("production app has no batch host store")
				}
				callID := "supervision-binding-" + tc.name
				_, err = session.NewBatchStore(a.DB()).Reserve(tctx, session.BatchReservation{
					Owner: owner.ID, Host: a.BatchHostStore().HostID(), RunnerID: "supervision-binding-runner",
					ToolCallID: callID, InputHash: "supervision-binding-hash-" + tc.name, Sequence: 1,
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
				if err != nil {
					diag.helperErrorClass = "batch-reserve"
					t.Fatalf("valid live batch fixture reservation failed")
				}
			}

			diag.stage = "store-binding"
			selectedStore := agent.SupervisionNoticeStoreForTest(a.AgentCoordinator)
			diag.storeBound = selectedStore != nil && selectedStore == a.BatchNoticeStore()
			if !diag.storeBound {
				t.Fatalf("coordinator supervision store is not the app's selected notice store")
			}

			diag.stage = "probe"
			probe, err := agent.NewSupervisionNoticeProbeForTest(a.AgentCoordinator, owner.ID, conn, tc.busy)
			if err != nil {
				diag.helperErrorClass = "probe-register"
				t.Fatalf("supervision probe registration failed")
			}
			beforeExists, beforeTicks, beforePaused := probe.State()

			// Wait is consumed exactly once; always unregister even on timeout.
			t.Cleanup(func() {
				if !t.Failed() {
					diag.stage = "callback-completion"
				}
				if !probe.Wait() {
					diag.helperErrorClass = "callback-timeout"
					t.Errorf("supervision probe completion timed out")
				}
				diag.gotRuns = probe.Runs()
				probe.Close()
			})

			diag.stage = "fire"
			diag.generation = tc.fireGen
			probe.Fire(tc.fireGen)
			exists, ticks, paused := probe.State()
			diag.ticks, diag.paused = ticks, paused

			if tc.expectWake {
				diag.stage = "wake-observation"
				var got agent.SupervisionNoticeWakeForTest
				select {
				case got = <-probe.Wakes:
				case <-time.After(5 * time.Second):
					t.Errorf("no supervision driver wake observed before timeout")
				}
				diag.noticeCount, diag.readErr = got.NoticeCount, got.ReadErr
				diag.gotKind = supervisionNoticeKindLabel(got.Kind)
				diag.gotWake, diag.gotDrain = got.Wake, got.IsDrain
				diag.gotRuns = probe.Runs()
				if got.SessionID != owner.ID {
					t.Errorf("supervision wake targeted an unexpected session")
				}
				if !got.IsDrain {
					t.Errorf("supervision wake was not a drain")
				}
				if got.NoticeCount != 1 || got.Kind != session.NoticeKindSupervision || got.Wake != 1 || got.ReadErr {
					t.Errorf("persisted supervision notice snapshot did not match the expected single wake notice")
				}
				if gotRuns := probe.Runs(); gotRuns != 1 {
					diag.gotRuns = gotRuns
					t.Errorf("positive case ran the registered driver %d times", gotRuns)
				}
				if !exists || ticks != 1 || !paused {
					t.Errorf("positive supervision state did not advance once")
				}
			} else {
				diag.stage = "negative-state-check"
				count, readErr := supervisionNoticeOwnerCount(conn, owner.ID)
				diag.noticeCount = count
				diag.readErr = readErr
				if readErr {
					t.Errorf("could not read owner notice count")
				} else if count != 0 {
					t.Errorf("negative case persisted %d owner notices", count)
				}
				if gotRuns := probe.Runs(); gotRuns != 0 {
					diag.gotRuns = gotRuns
					t.Errorf("negative case ran the registered driver %d times", gotRuns)
				}
				switch tc.name {
				case "closed":
					if exists {
						t.Errorf("closed supervision scope retained its state")
					}
				case "stale", "busy":
					if exists != beforeExists || ticks != beforeTicks || paused != beforePaused {
						t.Errorf("stale or busy supervision fire changed its state")
					}
				}
			}
			if !t.Failed() {
				diag.stage = "complete"
			}
		})
	}
}

func supervisionNoticeKindLabel(kind string) string {
	if kind == string(session.NoticeKindSupervision) {
		return "supervision"
	}
	if kind == "" {
		return "none"
	}
	return "other"
}

func supervisionNoticeOwnerCount(conn *sql.DB, owner string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM session_notices WHERE owner=?", owner).Scan(&count); err != nil {
		return 0, true
	}
	return count, false
}
