package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type cycle6RunApp struct {
	app     *App
	sess    session.Session
	service session.Service
}

func newCycle6RunApp(t *testing.T) cycle6RunApp {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"id":"cycle6","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"terminal text"},"finish_reason":null}]}`,
			`{"id":"cycle6","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
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
		Models:  []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
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
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	sess, err := application.Sessions.Create(context.Background(), "cycle6-run")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, messageCreateSeed())
	require.NoError(t, err)

	return cycle6RunApp{app: application, sess: sess, service: application.Sessions}
}

func messageCreateSeed() message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	}
}

func TestExecuteRunSubscribesBeforeImmediateTerminalPublishTerse(t *testing.T) {
	h := newCycle6RunApp(t)

	var terse bytes.Buffer
	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeTerse,
		ContinueSessionID: h.sess.ID,
		Stdout:            &terse,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, "terminal text\n", terse.String())
}

func TestExecuteRunSubscribesBeforeImmediateTerminalPublishJSON(t *testing.T) {
	h := newCycle6RunApp(t)
	probe := &cycle6MessageSubscriptionProbe{Service: h.app.Messages}
	h.app.Messages = probe
	executeRunBeforeTurnLaunchSeam = func() {
		require.True(t, probe.subscribed.Load(), "the live message subscription must exist before the turn launches")
	}
	t.Cleanup(func() { executeRunBeforeTurnLaunchSeam = nil })
	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "terminal text", result.FinalText)
	require.Equal(t, "end_turn", result.ExitReason)
	require.Empty(t, result.ToolCalls)
}

type cycle6MessageSubscriptionProbe struct {
	message.Service
	subscribed atomic.Bool
}

func (p *cycle6MessageSubscriptionProbe) Subscribe(ctx context.Context) <-chan pubsub.Event[message.Message] {
	p.subscribed.Store(true)
	return p.Service.Subscribe(ctx)
}

type cycle6GetFailureSessions struct {
	session.Service
	getCount   atomic.Int64
	failOn     int64
	failErr    error
	cancel     context.CancelFunc
	detachedOK *atomic.Bool
}

func (s *cycle6GetFailureSessions) Get(ctx context.Context, id string) (session.Session, error) {
	if s.getCount.Add(1) == s.failOn {
		if s.cancel != nil {
			s.cancel()
		}
		if s.failErr != nil {
			return session.Session{}, s.failErr
		}
		if err := ctx.Err(); err != nil {
			return session.Session{}, err
		}
		if s.detachedOK != nil {
			s.detachedOK.Store(true)
		}
	}
	return s.Service.Get(ctx, id)
}

func seedCycle6Usage(t *testing.T, h cycle6RunApp) {
	t.Helper()
	require.NoError(t, h.service.SetUsage(context.Background(), h.sess.ID, 100, 50))
	_, err := h.service.IncrementCost(context.Background(), h.sess.ID, 0.25)
	require.NoError(t, err)
}

func TestExecuteRunUsageReadSurvivesCallerCancellation(t *testing.T) {
	h := newCycle6RunApp(t)
	seedCycle6Usage(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	var detachedOK atomic.Bool
	h.app.Sessions = &cycle6GetFailureSessions{
		Service:    h.service,
		failOn:     2,
		cancel:     cancel,
		detachedOK: &detachedOK,
	}
	result, err := h.app.ExecuteRun(ctx, RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "terminal text", result.FinalText)
	require.GreaterOrEqual(t, result.Usage.DeltaTokens, int64(0))
	require.GreaterOrEqual(t, result.Usage.DeltaCostUSD, float64(0))
	require.True(t, detachedOK.Load(), "final usage read must use a context detached from caller cancellation")
}

func TestExecuteRunUsageLookupFailureUsesZeroDeltas(t *testing.T) {
	h := newCycle6RunApp(t)
	seedCycle6Usage(t, h)
	lookupErr := errors.New("transient session lookup failure")
	h.app.Sessions = &cycle6GetFailureSessions{
		Service: h.service,
		failOn:  2,
		failErr: lookupErr,
	}

	result, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "answer immediately",
		Mode:              RunModeJSON,
		ContinueSessionID: h.sess.ID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "terminal text", result.FinalText)
	require.Zero(t, result.Usage.DeltaTokens)
	require.Zero(t, result.Usage.DeltaCostUSD)
}
