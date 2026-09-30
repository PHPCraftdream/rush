// Shared harness for the Drain-attempt tests (docs/reviews/2026-09-30-async-
// phase4-round2-attempts-design.md sec.5): a real coordinator + workLedger on
// a real SQLite store, a real *sessionAgent talking to an httptest provider,
// OnSessionIdle wired like production. Faults are injected with SQLite
// triggers on the shared connection, never with code seams.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type attemptFixture struct {
	t        *testing.T
	env      fakeEnv
	store    *session.AsyncJobStore
	coord    *coordinator
	ledger   *workLedger
	sa       *sessionAgent
	model    Model
	sessID   string
	requests atomic.Int32
	srv      *httptest.Server

	mu      sync.Mutex
	handler http.HandlerFunc
}

type attemptFixtureOpts struct {
	handler http.HandlerFunc // nil: a one-step text reply
	tools   []fantasy.AgentTool
	noIdle  bool // do not wire OnSessionIdle
	// streamIdle/streamTick shrink the stream watchdog (a stalled provider).
	streamIdle, streamTick time.Duration
	// clientTimeout is the provider HTTP client's Client.Timeout: a stalled
	// server then fails with a net/http timeout, not a cancelled context.
	clientTimeout time.Duration
}

func newAttemptFixture(t *testing.T, title string, o attemptFixtureOpts) *attemptFixture {
	t.Helper()
	f := &attemptFixture{t: t, handler: o.handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		h := f.handler
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		textFinishResponse(w, "reacted")
	}))
	t.Cleanup(f.srv.Close)
	if o.clientTimeout > 0 {
		f.model = newProbeModelClient(t, f.srv, &http.Client{Timeout: o.clientTimeout})
	} else {
		f.model = newProbeModel(t, f.srv)
	}

	f.env = testEnv(t)
	f.store = session.NewAsyncJobStore(f.env.conn, f.env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = f.store.Close(context.Background()) })
	f.coord = &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	f.ledger = newWorkLedger(f.coord.notifyAsyncCompletion)
	f.ledger.store = f.store
	f.ledger.coord = f.coord
	f.coord.asyncJobs = f.ledger

	tools := o.tools
	if tools == nil {
		tools = []fantasy.AgentTool{}
	}
	opts := SessionAgentOptions{
		SmartModel: f.model, FastModel: f.model, SystemPrompt: "you are a probe",
		DataDirectory: f.env.workingDir, Sessions: f.env.sessions, Messages: f.env.messages,
		Tools: tools, DisableAutoSummarize: true, AsyncJobs: f.ledger,
		StreamIdleTimeout: o.streamIdle, StreamWatchdogTick: o.streamTick,
	}
	if !o.noIdle {
		opts.OnSessionIdle = f.coord.onSessionIdleHook
	}
	f.sa = NewSessionAgent(opts).(*sessionAgent)
	sess, err := f.env.sessions.Create(context.Background(), title)
	require.NoError(t, err)
	f.sessID = sess.ID
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: f.sessID}})
	return f
}

func (f *attemptFixture) setHandler(h http.HandlerFunc) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// seedDebt claims toolCallID, announces it and terminal-transitions it with
// wake=1 (delivery='pending'); pulled additionally runs the notice pull so
// the row is delivery='done' (visible debt).
func (f *attemptFixture) seedDebt(ctx context.Context, toolCallID string, pulled bool) {
	f.t.Helper()
	_, existing, err := f.ledger.Start(f.sessID, toolCallID, toolCallID, "bash", "", false, false, nil, func() {})
	require.NoError(f.t, err)
	require.False(f.t, existing)
	require.NoError(f.t, f.store.MarkAnnounced(ctx, f.sessID, toolCallID))
	_, err = f.store.Transition(ctx, session.TransitionParams{
		Owner: f.sessID, ToolCallID: toolCallID, State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(f.t, err)
	if pulled {
		_, err = f.store.PullJobNotices(ctx, f.env.messages, f.sessID, buildJobNoticeMessageParams)
		require.NoError(f.t, err)
	}
}

func (f *attemptFixture) row(ctx context.Context, toolCallID string) db.AsyncJob {
	f.t.Helper()
	row, err := f.store.Get(ctx, f.sessID, toolCallID)
	require.NoError(f.t, err)
	return row
}

// markers counts the wake_failed markers written for the session.
func (f *attemptFixture) markers(ctx context.Context) int {
	f.t.Helper()
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(f.t, err)
	n := 0
	for _, no := range notices {
		if no.Kind == session.NoticeKindWakeFailed {
			n++
		}
	}
	return n
}

func (f *attemptFixture) exec(ctx context.Context, stmt string) {
	f.t.Helper()
	_, err := f.env.conn.ExecContext(ctx, stmt)
	require.NoError(f.t, err)
}

// blockReactions makes every real reaction write (reacted=1, reacted_failed=0)
// abort; a settle-by-failure (reacted_failed=1) still goes through.
func (f *attemptFixture) blockReactions(ctx context.Context) {
	f.exec(ctx, `CREATE TRIGGER fx_block_reaction BEFORE UPDATE OF reacted ON async_jobs
		WHEN NEW.reacted = 1 AND NEW.reacted_failed = 0
		BEGIN SELECT RAISE(ABORT, 'fx: reaction blocked'); END`)
}

// blockSettles additionally makes the settle-by-failure write abort.
func (f *attemptFixture) blockReactionsAndSettles(ctx context.Context) {
	f.exec(ctx, `CREATE TRIGGER fx_block_reaction BEFORE UPDATE OF reacted ON async_jobs
		WHEN NEW.reacted = 1
		BEGIN SELECT RAISE(ABORT, 'fx: reaction and settle blocked'); END`)
}

// drainRun runs one Drain call directly on the agent (one leg).
func (f *attemptFixture) drainRun(ctx context.Context) (*fantasy.AgentResult, error) {
	return f.sa.Run(ctx, newDrainCall(SessionAgentCall{SessionID: f.sessID}))
}

// inRecheckSet reports whether the session sits in the coordinator's
// re-check set.
func (f *attemptFixture) inRecheckSet() bool {
	f.coord.recheckMu.Lock()
	defer f.coord.recheckMu.Unlock()
	_, ok := f.coord.recheckSet[f.sessID]
	return ok
}
