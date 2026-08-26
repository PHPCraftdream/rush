package app

// Task #773 verification: App.New(..., SkipAgentSetup()) must skip
// recoverInterruptedTurns, mcp.Initialize, and InitCoderAgent entirely, and
// the default (no-opts) path must remain completely unchanged — still runs
// recovery and still builds the AgentCoordinator. This uses the SAME
// isolation pattern as p348_p0_1_ordering_race_test.go /
// p348_p0_1_pump_coordinator_wiring_test.go (the only other tests in this
// package that call the real App.New entry point) so it doesn't leak into
// the host's real global config/data dirs or make network calls.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateAppNewTestEnv sets up the same global-config/data isolation used
// by the other App.New callers in this package, so tests never touch the
// host's real ~/.config/rush or ~/.local/share/rush.
func isolateAppNewTestEnv(t *testing.T) {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
}

// TestAppNew_SkipAgentSetup_DoesNotInitAgentOrRecover proves that
// New(ctx, conn, store, SkipAgentSetup()) — the path setupAppLite in
// internal/cmd/root.go uses for config-only commands (`models`,
// `providers`, `mcp`, `login`, `queue add/list/show/rm/clear`) — builds an
// App with AgentCoordinator and RunQueuePump left nil, and never calls
// recoverInterruptedTurns: seeds an orphaned assistant message (tool call,
// no finish part) BEFORE calling New, same as
// TestRecoverInterruptedTurns_OrphanAssistant_GetsErrorFinish's fixture,
// and asserts it is untouched afterward, plus that recoverSessionListSeam
// (task #774's per-candidate seam) never fired.
func TestAppNew_SkipAgentSetup_DoesNotInitAgentOrRecover(t *testing.T) {
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	// Deliberately do NOT configure a provider/model — a config-only
	// command must not require one, and this also proves SkipAgentSetup
	// short-circuits before the `!cfg.IsConfigured()` branch would anyway.

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	// Seed an orphaned assistant message BEFORE New() — if recovery ran,
	// this would get a FinishReasonError stamped on it.
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	sess, err := sessions.Create(context.Background(), "skip-agent-setup-probe")
	require.NoError(t, err)
	orphan, err := messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: seeded message must be unfinished")

	var seamCalls int64
	recoverSessionListSeam = func() { atomic.AddInt64(&seamCalls, 1) }
	t.Cleanup(func() { recoverSessionListSeam = nil })

	application, err := New(context.Background(), conn, store, SkipAgentSetup())
	require.NoError(t, err)
	t.Cleanup(func() {
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	assert.Nil(t, application.AgentCoordinator,
		"SkipAgentSetup must leave AgentCoordinator nil — InitCoderAgent must not run")
	assert.Nil(t, application.RunQueuePump,
		"SkipAgentSetup must leave RunQueuePump nil — it's only started after InitCoderAgent")
	assert.Equal(t, int64(0), atomic.LoadInt64(&seamCalls),
		"SkipAgentSetup must not call recoverInterruptedTurns at all")

	got, err := messages.Get(context.Background(), orphan.ID)
	require.NoError(t, err)
	assert.False(t, got.IsFinished(),
		"SkipAgentSetup must not touch pre-existing orphaned messages — recovery must not have run")
}

// TestAppNew_DefaultPath_StillRunsRecoveryAndInitCoderAgent is the
// regression counterpart: proves the ordinary New(ctx, conn, store) call
// (no options — every (a)-classified command: `rush`/web, `rush run`,
// `rush system-prompt`) is completely unchanged by the addition of
// SkipAgentSetup. Still runs recoverInterruptedTurns (the seeded orphan
// gets recovered) and still builds a real, usable AgentCoordinator.
func TestAppNew_DefaultPath_StillRunsRecoveryAndInitCoderAgent(t *testing.T) {
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()

	// Minimal SSE chat-completion stub so InitCoderAgent's coordinator
	// construction has a provider to validate against, without hitting a
	// real network endpoint. The probe never needs to actually be called
	// by this test — its existence is enough to satisfy config validation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
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
	// See p348_p0_1_ordering_race_test.go's comment on SetupAgents for why
	// this direct call is required in an isolated-empty-dir test.
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)
	sess, err := sessions.Create(context.Background(), "default-path-probe")
	require.NoError(t, err)
	orphan, err := messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	// Backdate it well past the 30s default orphan-age threshold so the
	// production sweep (this test uses the real, non-zeroed threshold —
	// no recoveryOrphanAge override, since New() doesn't expose one) does
	// not skip it as "too fresh".
	staleTime := time.Now().Add(-time.Minute).Unix()
	_, err = conn.ExecContext(context.Background(),
		"UPDATE messages SET created_at = ? WHERE id = ?", staleTime, orphan.ID)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	assert.NotNil(t, application.AgentCoordinator,
		"default New() path must still build AgentCoordinator")

	got, err := messages.Get(context.Background(), orphan.ID)
	require.NoError(t, err)
	assert.True(t, got.IsFinished(),
		"default New() path must still run recoverInterruptedTurns and recover the orphan")
	assert.Equal(t, message.FinishReasonError, got.FinishReason())
}
