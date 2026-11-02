package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

const usageFlushInterval = time.Minute

var (
	serveAPIRoutes = func(app *appPkg.App) []APIRoute { return APIRoutesWithApp(app) }
	serveAPINow    = time.Now
)

// ServeAPI contains the authentication and REST wiring for one serve listener.
type ServeAPI struct {
	Auth        func(http.Handler) http.Handler
	RESTHandler http.Handler
	stop        func(context.Context) error
	stopOnce    sync.Once
	stopErr     error
}

var serveFlushers = struct {
	sync.Mutex
	stores map[*config.APITokenStore]*serveUsageFlusher
}{stores: make(map[*config.APITokenStore]*serveUsageFlusher)}

type serveUsageFlusher struct {
	cancel context.CancelFunc
	done   chan struct{}
	refs   int
}

// NewServeAPI builds the production authenticator and REST mount for a serve listener.
// Call Stop before shutting down the App; listeners sharing a store share one flusher.
func NewServeAPI(policy ListenerPolicy, app *appPkg.App, store *config.APITokenStore, bootstrapTokenIDs []string) (*ServeAPI, error) {
	if store == nil {
		return nil, errors.New("token verifier required")
	}
	logger := slog.Default()
	auth := Chain(ChainConfig{
		AuthConfig: AuthConfig{Verifier: store, AllowedTokenIDs: append([]string(nil), policy.TokenIDs...), BootstrapTokenIDs: append([]string(nil), bootstrapTokenIDs...)},
		RPS:        policy.Limits.RPS, Burst: policy.Limits.Burst, MaxConnsPerToken: policy.Limits.MaxConnsPerToken, MaxBody: policy.Limits.MaxBody,
		Logger: logger,
	})
	public, protected := NewAPIHandlerSplit(serveAPIRoutes(app))
	limits := policy.Limits
	rps, burst := limits.RPS, limits.Burst
	if rps == 0 {
		rps = 20
	}
	if burst == 0 {
		burst = 40
	}
	healthLimit := newRateLimiter(rps, burst, serveAPINow)
	health := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retry := healthLimit.Allow("ip:"+sourceIP(r.RemoteAddr), serveAPINow()); retry > 0 {
			retryResponse(w, retry)
			return
		}
		public.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/health", health)
	mux.Handle("/api/v1/", auth(protected))
	rest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			idResponse := &apiTrackingWriter{ResponseWriter: w}
			setAPIResponseHeaders(idResponse, r)
			idRequest := r.Clone(r.Context())
			idRequest.Header = r.Header.Clone()
			idRequest.Header.Set("X-Request-Id", idResponse.Header().Get("X-Request-Id"))
			mux.ServeHTTP(idResponse, idRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
	flusher := retainServeFlusher(store)
	return &ServeAPI{Auth: auth, RESTHandler: rest, stop: func(ctx context.Context) error { return releaseServeFlusher(store, flusher, ctx) }}, nil
}

func retainServeFlusher(store *config.APITokenStore) *serveUsageFlusher {
	serveFlushers.Lock()
	defer serveFlushers.Unlock()
	if existing := serveFlushers.stores[store]; existing != nil {
		existing.refs++
		return existing
	}
	ctx, cancel := context.WithCancel(context.Background())
	flusher := &serveUsageFlusher{cancel: cancel, done: make(chan struct{}), refs: 1}
	serveFlushers.stores[store] = flusher
	go func() {
		defer close(flusher.done)
		RunUsageFlusher(ctx, store, usageFlushInterval)
	}()
	return flusher
}

func releaseServeFlusher(store *config.APITokenStore, flusher *serveUsageFlusher, ctx context.Context) error {
	serveFlushers.Lock()
	flusher.refs--
	last := flusher.refs == 0
	if last {
		delete(serveFlushers.stores, store)
		flusher.cancel()
	}
	serveFlushers.Unlock()
	if !last {
		return nil
	}
	select {
	case <-flusher.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return context.DeadlineExceeded
	}
}

// Configure attaches this builder's authenticator and REST routes to a listener policy.
func (api *ServeAPI) Configure(policy *ListenerPolicy) {
	if api == nil || policy == nil {
		return
	}
	policy.Auth = api.Auth
	policy.RESTHandler = api.RESTHandler
}

// Stop stops and joins this builder's shared usage flusher. The wait is capped at five seconds.
func (api *ServeAPI) Stop(ctx context.Context) error {
	if api == nil || api.stop == nil {
		return nil
	}
	api.stopOnce.Do(func() { api.stopErr = api.stop(ctx) })
	return api.stopErr
}
