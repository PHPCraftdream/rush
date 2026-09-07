package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type retryCandidate struct {
	server    *mcp.Server
	http      *httptest.Server
	started   chan struct{}
	release   chan struct{}
	closed    chan struct{}
	tokens    chan string
	startOnce sync.Once
	closeOnce sync.Once
	count     atomic.Int32
}

func newRetryCandidate(t *testing.T, toolName string, blocked bool) *retryCandidate {
	t.Helper()
	candidate := &retryCandidate{
		server:  mcp.NewServer(&mcp.Implementation{Name: toolName}, nil),
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
		tokens:  make(chan string, 16),
	}
	mcp.AddTool(candidate.server, &mcp.Tool{Name: toolName}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	candidate.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if blocked && method == "tools/list" {
				go func() {
					select {
					case <-ctx.Done():
						candidate.closeOnce.Do(func() { close(candidate.closed) })
					case <-time.After(5 * time.Second):
					}
				}()
			}
			if method == "initialize" {
				candidate.count.Add(1)
			}
			if blocked && method == "tools/list" {
				candidate.startOnce.Do(func() { close(candidate.started) })
				select {
				case <-candidate.release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next(ctx, method, request)
		}
	})
	delegate := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return candidate.server
	}, nil)
	candidate.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get("X-Retry-Token"); token != "" {
			select {
			case candidate.tokens <- token:
			default:
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	cleanupTestMCPServer(t, candidate.server, candidate.http)
	return candidate
}

func TestInitializeCrossStoreStaleAdmissionRetriesFreshDiskSnapshot(t *testing.T) {
	runCrossStoreAdmissionRetry(t, false)
}

func TestInitializeSingleCrossStoreStaleAdmissionRetriesFreshDiskSnapshot(t *testing.T) {
	runCrossStoreAdmissionRetry(t, true)
}

func TestInitializeSlowCandidateSurvivesNoOpReload(t *testing.T) {
	const name = "no-op-reload-admission"
	candidate := newRetryCandidate(t, "stable-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(done)
	}()
	awaitMCPSignal(t, candidate.started)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize did not finish after no-op reload")
	}
	require.Equal(t, int32(1), candidate.count.Load())
	require.Equal(t, []string{"stable-tool"}, GetServerToolNames(name))
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestInitializeSingleSlowCandidateSurvivesNoOpReload(t *testing.T) {
	const name = "single-no-op-reload-admission"
	candidate := newRetryCandidate(t, "single-stable-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	go func() { done <- InitializeSingle(context.Background(), name, store) }()
	awaitMCPSignal(t, candidate.started)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	require.NoError(t, awaitMCPError(t, done))
	require.Equal(t, int32(1), candidate.count.Load())
	require.Equal(t, []string{"single-stable-tool"}, GetServerToolNames(name))
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestInitializeRejectsSemanticResolverChangeAndRetriesFreshCandidate(t *testing.T) {
	runSemanticResolverRetry(t, false)
}

func TestInitializeSingleRejectsSemanticResolverChangeAndRetriesFreshCandidate(t *testing.T) {
	runSemanticResolverRetry(t, true)
}

func runSemanticResolverRetry(t *testing.T, single bool) {
	const name = "resolver-change-admission"
	t.Setenv("RUSH_MCP_RETRY_HEADER", "old")
	candidate := newRetryCandidate(t, "resolver-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name+".headers", map[string]string{
		"X-Retry-Token": "$RUSH_MCP_RETRY_HEADER",
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	if single {
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
	} else {
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			done <- nil
		}()
	}
	awaitMCPSignal(t, candidate.started)
	t.Setenv("RUSH_MCP_RETRY_HEADER", "new")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("initialization did not finish after resolver change")
	}
	seenOld, seenNew := false, false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenOld || !seenNew {
		select {
		case token := <-candidate.tokens:
			seenOld = seenOld || token == "old"
			seenNew = seenNew || token == "new"
		case <-deadline.C:
			t.Fatal("did not observe both old and new resolver headers")
		}
	}
	require.Equal(t, int32(2), candidate.count.Load(), "the resolver change must reject the old candidate")
	require.Eventually(t, func() bool {
		select {
		case <-candidate.closed:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "the stale resolver candidate was not closed")
	require.Equal(t, []string{"resolver-tool"}, GetServerToolNames(name))
}

func runCrossStoreAdmissionRetry(t *testing.T, single bool) {
	t.Helper()
	const name = "cross-store-admission-retry"
	a := newRetryCandidate(t, "a-tool", true)
	b := newRetryCandidate(t, "b-tool", false)
	store := persistedMCPStore(t, name, a.http.URL, false)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	if single {
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
	} else {
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			done <- nil
		}()
	}
	select {
	case <-a.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial candidate did not reach tools/list")
	}
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": b.http.URL,
	}))
	close(a.release)
	require.NoError(t, awaitMCPError(t, done))

	require.Eventually(t, func() bool {
		state, ok := GetState(name)
		return ok && state.State == StateConnected && hasSession(name)
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, int32(1), b.count.Load(), "fresh B candidate must publish exactly once")
	require.Equal(t, []string{"b-tool"}, GetServerToolNames(name))
	require.Eventually(t, func() bool {
		select {
		case <-a.closed:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "stale A candidate was not closed")
	require.Equal(t, int32(1), a.count.Load(), "A must never be initialized again after the stale pin")
}
