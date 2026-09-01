package sdk

// R2-2 (P1) — Close-vs-in-flight admission tests (round-3 verification
// matrix). The old Client checked a bare closed flag on method entry:
// correct for calls that start strictly after Close returns, but it left
// the check-then-act race open — a call reads closed==false, is
// descheduled, and then enters the App while (or after) Close tears it
// down, reaching a released DB on the ephemeral path. The admission state
// machine in sdk.go closes that: a call either registers in-flight before
// closing starts (and Close waits for it before releasing anything), or
// it is rejected with ErrClientClosed / an already-closed channel.
//
// The tests below park each public call at a gate BETWEEN its admission
// and its first touch of the real App (gated coordinator and gated
// message/session service wrappers, the same spirit as
// close_conns_test.go's spy coordinators), start Close concurrently, and
// pin the invariant from the review matrix: the admitted call finishes
// against a fully live App BEFORE the shutdown's first act (CancelAll),
// and a call that arrives after closing began is refused. All
// synchronization is channels — no sleeps. These tests live inside
// package sdk because they must swap the unexported client.app service
// fields and call beginShutdown directly.
//
// Regression scope: client_closed_test.go (post-Close rejections),
// close_conns_test.go (forced/graceful handle policy), and
// close_internal_test.go (idempotency, nil-safety) must keep passing —
// Close stays idempotent (closeOnce) and nil-safe under the new model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seqRecorder records labelled steps from concurrent goroutines so a
// test can assert their ORDER after both sides have joined — instead of
// guessing with sleeps while they run.
type seqRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (s *seqRecorder) record(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, step)
}

func (s *seqRecorder) indexOf(step string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, got := range s.steps {
		if got == step {
			return i
		}
	}
	return -1
}

// admissionGate parks a call between its admission and its first touch
// of the real App, with channel synchronization only.
type admissionGate struct {
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func newAdmissionGate() *admissionGate {
	return &admissionGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *admissionGate) signalEntered() { g.once.Do(func() { close(g.entered) }) }

func (g *admissionGate) waitEntered() { <-g.entered }

func (g *admissionGate) open() { close(g.release) }

// credRunner mirrors internal/app's unexported credentialsRunner
// capability: RunWithCredentials lives on the concrete coordinator, not
// on the agent.Coordinator interface, so the gated wrapper has to carry
// it explicitly to stay passable to ExecuteRun's type assertion.
type credRunner interface {
	RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *agent.CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error)
}

// gatedRunCoordinator parks Run/RunWithCredentials before delegating
// them, and records CancelAll — the full ordering probe for the drain
// tests.
type gatedRunCoordinator struct {
	agent.Coordinator
	credRunner
	seq  *seqRecorder
	gate *admissionGate
	kind string
}

func (c *gatedRunCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.seq.record(c.kind + ":entered")
	c.gate.signalEntered()
	<-c.gate.release
	c.seq.record(c.kind + ":delegating")
	return c.Coordinator.Run(ctx, sessionID, prompt, attachments...)
}

func (c *gatedRunCoordinator) RunWithCredentials(ctx context.Context, sessionID, prompt string, creds *agent.CredentialSet, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.seq.record(c.kind + ":entered")
	c.gate.signalEntered()
	<-c.gate.release
	c.seq.record(c.kind + ":delegating")
	return c.credRunner.RunWithCredentials(ctx, sessionID, prompt, creds, attachments...)
}

func (c *gatedRunCoordinator) CancelAll() bool {
	c.seq.record("shutdown:CancelAll")
	return c.Coordinator.CancelAll()
}

// recordingCoordinator records CancelAll — the first thing an App
// shutdown performs — and delegates everything else to the real
// coordinator.
type recordingCoordinator struct {
	agent.Coordinator
	seq *seqRecorder
}

func (c *recordingCoordinator) CancelAll() bool {
	c.seq.record("shutdown:CancelAll")
	return c.Coordinator.CancelAll()
}

// gatedMessagesService parks List before delegating to the real service.
type gatedMessagesService struct {
	message.Service
	seq  *seqRecorder
	gate *admissionGate
}

func (g *gatedMessagesService) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	g.seq.record("messages:entered")
	g.gate.signalEntered()
	<-g.gate.release
	g.seq.record("messages:delegating")
	return g.Service.List(ctx, sessionID)
}

// gatedSessionService parks Get before delegating to the real service.
type gatedSessionService struct {
	session.Service
	seq  *seqRecorder
	gate *admissionGate
}

func (g *gatedSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	g.seq.record("session:entered")
	g.gate.signalEntered()
	<-g.gate.release
	g.seq.record("session:delegating")
	return g.Service.Get(ctx, id)
}

// gatedMessagesSubscription parks Subscribe before delegating.
type gatedMessagesSubscription struct {
	message.Service
	seq  *seqRecorder
	gate *admissionGate
}

func (g *gatedMessagesSubscription) Subscribe(ctx context.Context) <-chan pubsub.Event[message.Message] {
	g.seq.record("subscribeMessages:entered")
	g.gate.signalEntered()
	<-g.gate.release
	g.seq.record("subscribeMessages:delegating")
	return g.Service.Subscribe(ctx)
}

// gatedSessionsSubscription parks Subscribe before delegating.
type gatedSessionsSubscription struct {
	session.Service
	seq  *seqRecorder
	gate *admissionGate
}

func (g *gatedSessionsSubscription) Subscribe(ctx context.Context) <-chan pubsub.Event[session.Session] {
	g.seq.record("subscribeSessions:entered")
	g.gate.signalEntered()
	<-g.gate.release
	g.seq.record("subscribeSessions:delegating")
	return g.Service.Subscribe(ctx)
}

// newAdmissionTestClient opens a real ephemeral library-mode client
// (in-memory DB, migrations, full app wiring) against the given provider
// base URL, with the same global-config isolation the sdk_test helpers
// use.
func newAdmissionTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	client, err := openLibrary(context.Background(), Options{
		Mode: ModeLibrary,
		LibraryConfig: &LibraryConfig{
			Credentials: []Credential{{
				Provider: "library-provider",
				Type:     ProviderTypeOpenAICompat,
				APIKey:   "sk-admission",
				BaseURL:  baseURL,
				Models: []CredentialModel{
					{ID: "library-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
				},
			}},
			Models: map[Role]ModelChoice{
				RoleSmart:    {Provider: "library-provider", Model: "library-model"},
				RoleFast:     {Provider: "library-provider", Model: "library-model"},
				RoleWorker:   {Provider: "library-provider", Model: "library-model"},
				RoleReviewer: {Provider: "library-provider", Model: "library-model"},
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newDrainTestClient opens the ephemeral client and wires the CancelAll
// recorder in front of the real coordinator.
func newDrainTestClient(t *testing.T, baseURL string) (*Client, *seqRecorder) {
	t.Helper()
	client := newAdmissionTestClient(t, baseURL)
	seq := &seqRecorder{}
	client.app.AgentCoordinator = &recordingCoordinator{Coordinator: client.app.AgentCoordinator, seq: seq}
	return client, seq
}

// newMarkerProvider serves openai-compat SSE turns answering marker text
// (repeatable: background title generation hits it too).
func newMarkerProvider(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk := func(chunk map[string]any) {
			b, err := json.Marshal(chunk)
			if err != nil {
				panic(err)
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		writeChunk(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "library-model",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": marker},
				"finish_reason": nil,
			}},
		})
		writeChunk(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "library-model",
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 3, "total_tokens": 6},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// admissionCreds builds a per-call CredentialSet over the given provider
// (smart + fast coverage, the strict default).
func admissionCreds(baseURL string) CredentialSet {
	return CredentialSet{
		Credentials: []Credential{{
			Provider: "library-provider",
			Type:     ProviderTypeOpenAICompat,
			APIKey:   "sk-admission",
			BaseURL:  baseURL,
			Models: []CredentialModel{
				{ID: "library-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
			},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "library-provider", Model: "library-model"},
			RoleFast:  {Provider: "library-provider", Model: "library-model"},
		},
	}
}

// TestCloseWaitsForAdmittedRunBeforeShutdown pins the drain ordering for
// the run paths: a Run/RunWithCredentials admitted BEFORE Close started
// must run to completion against the fully live App, and only then may
// the shutdown's first act (CancelAll) happen. Under the old bare-flag
// check Close tore the App down while the run sat parked at the gate, and
// the run then failed against a closed DB — this test fails on that code.
func TestCloseWaitsForAdmittedRunBeforeShutdown(t *testing.T) {
	provider := newMarkerProvider(t, "ADMISSION_DRAIN_RUN_OK")

	t.Run("Run", func(t *testing.T) {
		client := newAdmissionTestClient(t, provider.URL)
		gate := newAdmissionGate()
		seq := &seqRecorder{}
		client.app.AgentCoordinator = &gatedRunCoordinator{
			Coordinator: client.app.AgentCoordinator,
			seq:         seq,
			gate:        gate,
			kind:        "run",
		}

		type runOutcome struct {
			res *RunResult
			err error
		}
		runDone := make(chan runOutcome, 1)
		go func() {
			res, err := client.Run(context.Background(), RunRequest{
				Prompt:      "reply with exactly the marker text and nothing else",
				Mode:        RunModeJSON,
				HideSpinner: true,
			})
			runDone <- runOutcome{res: res, err: err}
		}()
		gate.waitEntered() // admitted and parked inside the run call

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open() // let the admitted run finish while Close is draining
		outcome := <-runDone
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.res)
		require.Equal(t, "end_turn", outcome.res.ExitReason, "warnings=%v", outcome.res.Warnings)
		require.Equal(t, "ADMISSION_DRAIN_RUN_OK", outcome.res.FinalText)

		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("run:delegating"),
			"the shutdown must start only after the admitted run returned")
	})

	t.Run("RunWithCredentials", func(t *testing.T) {
		client := newAdmissionTestClient(t, provider.URL)
		gate := newAdmissionGate()
		seq := &seqRecorder{}
		runner, ok := client.app.AgentCoordinator.(credRunner)
		require.True(t, ok, "the real coordinator must support per-call credentials")
		client.app.AgentCoordinator = &gatedRunCoordinator{
			Coordinator: client.app.AgentCoordinator,
			credRunner:  runner,
			seq:         seq,
			gate:        gate,
			kind:        "runwc",
		}

		type runOutcome struct {
			res *RunResult
			err error
		}
		runDone := make(chan runOutcome, 1)
		go func() {
			res, err := client.RunWithCredentials(context.Background(), RunRequest{
				Prompt:      "reply with exactly the marker text and nothing else",
				Mode:        RunModeJSON,
				HideSpinner: true,
			}, admissionCreds(provider.URL))
			runDone <- runOutcome{res: res, err: err}
		}()
		gate.waitEntered()

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open()
		outcome := <-runDone
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.res)
		require.Equal(t, "ADMISSION_DRAIN_RUN_OK", outcome.res.FinalText)

		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("runwc:delegating"),
			"the shutdown must start only after the admitted run returned")
	})
}

// TestCloseWaitsForAdmittedReadsBeforeShutdown is the Messages / Session
// / Subscribe counterpart of the drain test: each admitted read completes
// against the live store or broker strictly before the shutdown starts,
// and an admitted subscription comes back LIVE (never the rejection
// channel) and is not orphaned — cancelling its ctx closes it.
func TestCloseWaitsForAdmittedReadsBeforeShutdown(t *testing.T) {
	provider := newMarkerProvider(t, "ADMISSION_DRAIN_READ_OK")

	t.Run("Messages", func(t *testing.T) {
		client, seq := newDrainTestClient(t, provider.URL)
		sess, err := client.app.Sessions.Create(context.Background(), "admission-messages")
		require.NoError(t, err)

		gate := newAdmissionGate()
		client.app.Messages = &gatedMessagesService{Service: client.app.Messages, seq: seq, gate: gate}

		readDone := make(chan error, 1)
		go func() {
			_, err := client.Messages(context.Background(), sess.ID)
			readDone <- err
		}()
		gate.waitEntered()

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open()
		require.NoError(t, <-readDone)
		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("messages:delegating"),
			"the shutdown must start only after the admitted Messages call returned")
	})

	t.Run("Session", func(t *testing.T) {
		client, seq := newDrainTestClient(t, provider.URL)
		sess, err := client.app.Sessions.Create(context.Background(), "admission-session")
		require.NoError(t, err)

		gate := newAdmissionGate()
		client.app.Sessions = &gatedSessionService{Service: client.app.Sessions, seq: seq, gate: gate}

		readDone := make(chan error, 1)
		go func() {
			_, err := client.Session(context.Background(), sess.ID)
			readDone <- err
		}()
		gate.waitEntered()

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open()
		require.NoError(t, <-readDone)
		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("session:delegating"),
			"the shutdown must start only after the admitted Session call returned")
	})

	t.Run("SubscribeMessages", func(t *testing.T) {
		client, seq := newDrainTestClient(t, provider.URL)
		gate := newAdmissionGate()
		client.app.Messages = &gatedMessagesSubscription{Service: client.app.Messages, seq: seq, gate: gate}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		subDone := make(chan (<-chan MessageEvent), 1)
		go func() { subDone <- client.SubscribeMessages(ctx) }()
		gate.waitEntered()

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open()
		ch := <-subDone
		select {
		case _, ok := <-ch:
			require.True(t, ok, "an admitted Subscribe must return a live channel, not a closed one")
		default:
			// no event pending — the channel is live
		}

		// Not an orphan: the subscription ends with its ctx.
		cancel()
		select {
		case _, ok := <-ch:
			require.False(t, ok, "cancelling the subscription ctx must close the channel")
		case <-time.After(10 * time.Second):
			t.Fatal("subscription channel never closed after ctx cancel (orphaned subscription)")
		}

		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("subscribeMessages:delegating"),
			"the shutdown must start only after the admitted Subscribe call returned")
	})

	t.Run("SubscribeSessions", func(t *testing.T) {
		client, seq := newDrainTestClient(t, provider.URL)
		gate := newAdmissionGate()
		client.app.Sessions = &gatedSessionsSubscription{Service: client.app.Sessions, seq: seq, gate: gate}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		subDone := make(chan (<-chan SessionEvent), 1)
		go func() { subDone <- client.SubscribeSessions(ctx) }()
		gate.waitEntered()

		closeDone := make(chan CloseResult, 1)
		go func() { closeDone <- client.Close() }()

		gate.open()
		ch := <-subDone
		select {
		case _, ok := <-ch:
			require.True(t, ok, "an admitted Subscribe must return a live channel, not a closed one")
		default:
			// no event pending — the channel is live
		}

		cancel()
		select {
		case _, ok := <-ch:
			require.False(t, ok, "cancelling the subscription ctx must close the channel")
		case <-time.After(10 * time.Second):
			t.Fatal("subscription channel never closed after ctx cancel (orphaned subscription)")
		}

		res := <-closeDone
		require.False(t, res.Forced)
		require.Empty(t, res.CleanupErrors)
		require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("subscribeSessions:delegating"),
			"the shutdown must start only after the admitted Subscribe call returned")
	})
}

// TestClosingClientRejectsNewAdmissionsWhileDraining pins, fully
// deterministically (no goroutine scheduling involved), the other half of
// the admission contract: from the instant closing begins — even while an
// admitted call is still parked mid-flight — EVERY new call is refused,
// while the already-admitted call keeps its live App and completes
// normally.
func TestClosingClientRejectsNewAdmissionsWhileDraining(t *testing.T) {
	provider := newMarkerProvider(t, "ADMISSION_REJECT_RUN_OK")
	client := newAdmissionTestClient(t, provider.URL)
	gate := newAdmissionGate()
	seq := &seqRecorder{}
	client.app.AgentCoordinator = &gatedRunCoordinator{
		Coordinator: client.app.AgentCoordinator,
		seq:         seq,
		gate:        gate,
		kind:        "run",
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), RunRequest{
			Prompt:      "reply with exactly the marker text and nothing else",
			Mode:        RunModeJSON,
			HideSpinner: true,
		})
		runDone <- err
	}()
	gate.waitEntered() // run admitted and parked

	// Flip into closing WITHOUT running the shutdown yet: the admitted
	// run keeps its live App behind the drain.
	drained := client.beginShutdown()

	// Every new admission is refused from this instant.
	_, err := client.Run(context.Background(), RunRequest{Prompt: "rejected", HideSpinner: true})
	require.ErrorIs(t, err, ErrClientClosed)
	_, err = client.RunWithCredentials(context.Background(),
		RunRequest{Prompt: "rejected", HideSpinner: true}, admissionCreds(provider.URL))
	require.ErrorIs(t, err, ErrClientClosed)
	_, err = client.Messages(context.Background(), "no-such-session")
	require.ErrorIs(t, err, ErrClientClosed)
	_, err = client.Session(context.Background(), "no-such-session")
	require.ErrorIs(t, err, ErrClientClosed)
	_, ok := <-client.SubscribeMessages(context.Background())
	require.False(t, ok, "SubscribeMessages must return an already-closed channel once closing began")
	_, ok = <-client.SubscribeSessions(context.Background())
	require.False(t, ok, "SubscribeSessions must return an already-closed channel once closing began")

	// The run admitted BEFORE closing still completes against the live App.
	gate.open()
	require.NoError(t, <-runDone)
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("drained never closed after the last admitted call returned")
	}

	// The shutdown itself still runs exactly once via the normal path.
	closeRes := client.Close()
	require.False(t, closeRes.Forced)
	require.Empty(t, closeRes.CleanupErrors)
	require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("run:delegating"),
		"the shutdown must start only after the admitted run returned")
}

// TestRacingReadsAndClosesNeverTouchReleasedResources sweeps the whole
// admission surface under -race: reads admitted at any point relative to
// two concurrent Closes either succeed or are refused with
// ErrClientClosed — they NEVER surface a released-resource error ("sql:
// database is closed" et al.), which is exactly what the old bare-flag
// check allowed to slip through.
func TestRacingReadsAndClosesNeverTouchReleasedResources(t *testing.T) {
	provider := newMarkerProvider(t, "ADMISSION_HAMMER_OK")
	client := newAdmissionTestClient(t, provider.URL)
	ctx := context.Background()
	sess, err := client.app.Sessions.Create(ctx, "admission-hammer")
	require.NoError(t, err)

	const readers = 6
	const perReader = 20
	errs := make(chan error, readers*2)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perReader; j++ {
				if _, err := client.Messages(ctx, sess.ID); err != nil && !errors.Is(err, ErrClientClosed) {
					errs <- fmt.Errorf("Messages: %w", err)
					return
				}
				if _, err := client.Session(ctx, sess.ID); err != nil && !errors.Is(err, ErrClientClosed) {
					errs <- fmt.Errorf("Session: %w", err)
					return
				}
				sctx, scancel := context.WithCancel(ctx)
				_ = client.SubscribeMessages(sctx)
				scancel()
			}
		}()
	}

	closeRes1 := make(chan CloseResult, 1)
	closeRes2 := make(chan CloseResult, 1)
	go func() { closeRes1 <- client.Close() }()
	go func() { closeRes2 <- client.Close() }()

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("a read racing Close surfaced a released-resource error: %v", err)
	default:
	}
	r1 := <-closeRes1
	r2 := <-closeRes2
	require.False(t, r1.Forced)
	require.Equal(t, r1, r2, "concurrent Closes must return the same cached result")
}
