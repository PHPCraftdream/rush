package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type Principal struct {
	TokenID         string
	Scopes          []string
	Permission      string
	PinnedSession   string
	TokenWorkspace  string
	CertFingerprint string
	// BatchBinding is the token's batch-intake binding (nil for other kinds).
	BatchBinding  *config.APITokenBatchBinding
	verifier      TokenVerifier
	expires       *time.Time
	bootstrap     bool
	tickerFactory func(time.Duration) Ticker
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type TokenVerifier interface {
	VerifyAPIToken(string, time.Time) (config.APIToken, bool, error)
	Touch(string)
	ShowAPIToken(string) (config.APIToken, error)
}

type (
	TokenFlusher interface{ Flush() error }
	Ticker       interface {
		C() <-chan time.Time
		Stop()
	}
)
type realTicker struct{ t *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.t.C }

func (t realTicker) Stop() { t.t.Stop() }

type AuthConfig struct {
	Verifier          TokenVerifier
	AllowedTokenIDs   []string
	Pins              map[string]string
	PinsDeclared      bool
	Workspace         string
	Clock             func() time.Time
	BootstrapTokenIDs []string
}

type ChainConfig struct {
	AuthConfig
	RPS              float64
	Burst            int
	MaxConnsPerToken int
	MaxBody          int64
	Logger           *slog.Logger
	TickerFactory    func(time.Duration) Ticker
	SlotReleased     chan<- struct{}
}

func (c ChainConfig) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

func (c ChainConfig) Validate() error {
	if c.Verifier == nil {
		return errors.New("token verifier required")
	}
	if c.RPS < 0 || c.Burst < 0 || c.MaxConnsPerToken < 0 || c.MaxBody < 0 {
		return errors.New("limits cannot be negative")
	}
	return nil
}

const (
	authFailureBody     = "{\"type\":\"about:blank\",\"title\":\"Unauthorized\",\"status\":401}\n"
	authUnavailableBody = "{\"type\":\"about:blank\",\"title\":\"Service Unavailable\",\"status\":503}\n"
	tooLargeBody        = "{\"type\":\"about:blank\",\"title\":\"Request Too Large\",\"status\":413}\n"
	authBusyBody        = "{\"type\":\"about:blank\",\"title\":\"Too Many Requests\",\"status\":429}\n"
)

var bearerPattern = regexp.MustCompile(`^rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}$`)

var (
	serveAuthTickerFactory func(time.Duration) Ticker
	serveAuthSlotReleased  chan struct{}
)

func Chain(cfg ChainConfig) func(http.Handler) http.Handler {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TickerFactory == nil {
		cfg.TickerFactory = serveAuthTickerFactory
	}
	if cfg.SlotReleased == nil {
		cfg.SlotReleased = serveAuthSlotReleased
	}
	if cfg.TickerFactory == nil {
		cfg.TickerFactory = func(d time.Duration) Ticker { return realTicker{t: time.NewTicker(d)} }
	}
	if cfg.RPS == 0 {
		cfg.RPS = 20
	}
	if cfg.Burst == 0 {
		cfg.Burst = 40
	}
	if cfg.MaxConnsPerToken == 0 {
		cfg.MaxConnsPerToken = 8
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 1 << 20
	}
	limits := newRateLimiter(cfg.RPS, cfg.Burst, cfg.now)
	backoff := newFailureBackoff(cfg.now)
	inflight := &tokenSlots{max: cfg.MaxConnsPerToken, active: make(map[string]int)}
	return func(next http.Handler) http.Handler {
		return auditMiddleware(cfg.Logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := sourceIP(r.RemoteAddr)
			if retry := backoff.Blocked(ip, cfg.now()); retry > 0 {
				retryResponse(w, retry)
				return
			}
			p, failureID, err, storeErr := authenticate(cfg, r)
			if err != nil {
				if errors.Is(err, errAmbiguousWebSocketCredentials) || errors.Is(err, errMissingWSProtocol) {
					writeProblem(w, http.StatusBadRequest, "{\"type\":\"about:blank\",\"title\":\"Bad Request\",\"status\":400}\n")
					return
				}
				if storeErr != nil {
					cfg.Logger.Error("API token verification failed", "error", Redact(storeErr.Error()))
					writeProblem(w, 503, authUnavailableBody)
					return
				}
				backoff.Failure(ip, cfg.now())
				setAuditToken(r, failureID)
				writeUnauthorized(w)
				return
			}
			setAuditPrincipal(r, p)
			cfg.Verifier.Touch(p.TokenID)
			ctx := WithPrincipal(r.Context(), p)
			if retry := limits.Allow("ip:"+ip, cfg.now()); retry > 0 {
				retryResponse(w, retry)
				return
			}
			if retry := limits.Allow("token:"+p.TokenID, cfg.now()); retry > 0 {
				retryResponse(w, retry)
				return
			}
			if p.TokenWorkspace != "" && !sameCanonicalWorkspace(p.TokenWorkspace, cfg.Workspace) {
				writeProblem(w, http.StatusForbidden, "{\"type\":\"token-not-for-this-workspace\",\"title\":\"Forbidden\",\"status\":403}\n")
				return
			}
			if !inflight.Acquire(p.TokenID) {
				retryResponse(w, time.Second)
				return
			}
			defer func() {
				inflight.Release(p.TokenID)
				if cfg.SlotReleased != nil {
					select {
					case cfg.SlotReleased <- struct{}{}:
					default:
					}
				}
			}()
			if r.ContentLength > cfg.MaxBody {
				writeProblem(w, 413, tooLargeBody)
				return
			}
			r.Body = &limitedBody{ReadCloser: http.MaxBytesReader(w, r.Body, cfg.MaxBody)}
			tracking := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(tracking, r.WithContext(ctx))
			if body, ok := r.Body.(*limitedBody); ok && body.tooLarge && !tracking.wroteHeader {
				tracking.Header().Set("Content-Type", "application/problem+json")
				writeProblem(w, 413, tooLargeBody)
			}
		}))
	}
}

var (
	errAmbiguousWebSocketCredentials = errors.New("ambiguous websocket credentials")
	errMissingWSProtocol             = errors.New("websocket credential missing rush.v1")
)

func authenticate(cfg ChainConfig, r *http.Request) (Principal, string, error, error) {
	if _, ok := r.URL.Query()["token"]; ok {
		return Principal{}, "", errors.New("bad credentials"), nil
	}
	if _, ok := r.URL.Query()["access_token"]; ok {
		return Principal{}, "", errors.New("bad credentials"), nil
	}
	values := r.Header.Values("Authorization")
	var token string
	if len(values) == 0 && r.URL.Path == "/ws" && websocketUpgrade(r) {
		var found []string
		for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
			for _, item := range strings.Split(header, ",") {
				item = strings.TrimSpace(item)
				if strings.HasPrefix(item, "rush.bearer.") {
					found = append(found, strings.TrimPrefix(item, "rush.bearer."))
				}
			}
		}
		if len(found) > 0 && !offeredSubprotocol(r, "rush.v1") {
			return Principal{}, "", errMissingWSProtocol, nil
		}
		if len(found) > 1 || (len(found) == 1 && !bearerPattern.MatchString(found[0])) {
			return Principal{}, "", errors.New("bad credentials"), nil
		}
		if len(found) == 1 {
			token = found[0]
		}
	} else if len(values) > 0 && r.URL.Path == "/ws" && websocketUpgrade(r) && hasBearerSubprotocol(r) {
		return Principal{}, "", errAmbiguousWebSocketCredentials, nil
	}
	if token == "" {
		if len(values) != 1 {
			return Principal{}, "", errors.New("bad credentials"), nil
		}
		v := values[0]
		parts := strings.Split(v, " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" || strings.TrimSpace(v) != v || !bearerPattern.MatchString(parts[1]) {
			return Principal{}, "", errors.New("bad credentials"), nil
		}
		token = parts[1]
	}
	id := token[5:13]
	t, ok, err := cfg.Verifier.VerifyAPIToken(token, cfg.now())
	if errors.Is(err, config.ErrInvalidAPIToken) {
		return Principal{}, id, errors.New("bad credentials"), nil
	}
	if err != nil {
		return Principal{}, id, errors.New("bad credentials"), err
	}
	if !ok || !allowedID(cfg.AllowedTokenIDs, t.ID) {
		return Principal{}, id, errors.New("bad credentials"), nil
	}
	if cfg.PinsDeclared {
		if _, pinned := cfg.Pins[t.ID]; !pinned {
			return Principal{}, id, errors.New("bad credentials"), nil
		}
	}
	show, showErr := cfg.Verifier.ShowAPIToken(t.ID)
	bootstrap := false
	if showErr != nil {
		if errors.Is(showErr, config.ErrAPITokenNotFound) && len(cfg.BootstrapTokenIDs) > 0 && allowedID(cfg.BootstrapTokenIDs, t.ID) {
			bootstrap = true
		} else if errors.Is(showErr, config.ErrAPITokenNotFound) {
			return Principal{}, id, errors.New("bad credentials"), nil
		} else {
			return Principal{}, id, errors.New("bad credentials"), showErr
		}
	} else {
		t = show
	}
	pin := ""
	workspace := ""
	binding := t.Binding.Normalized()
	if binding.Kind == "session" && binding.Pin != nil {
		pin = binding.Pin.Session
		workspace = binding.Pin.Workspace
	}
	if listenerPin, ok := cfg.Pins[t.ID]; ok {
		pin = listenerPin
		if workspace == "" {
			workspace = cfg.Workspace
		}
	}
	var batchBinding *config.APITokenBatchBinding
	if binding.Kind == "batch" && binding.Batch != nil {
		clone := *binding.Batch
		clone.Tools = append([]string(nil), binding.Batch.Tools...)
		batchBinding = &clone
	}
	fingerprint := ""
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) > 0 {
		fingerprint = certificateFingerprint(r)
	}
	return Principal{TokenID: t.ID, Scopes: append([]string(nil), t.Scopes...), Permission: t.Permission, PinnedSession: pin, TokenWorkspace: workspace, CertFingerprint: fingerprint, BatchBinding: batchBinding, verifier: cfg.Verifier, expires: t.Expires, bootstrap: bootstrap, tickerFactory: cfg.TickerFactory}, "", nil, nil
}

func sameCanonicalWorkspace(tokenWorkspace, listenerWorkspace string) bool {
	tokenPath, tokenErr := config.CanonicalAPIWorkspace(tokenWorkspace)
	listenerPath, listenerErr := config.CanonicalAPIWorkspace(listenerWorkspace)
	return tokenErr == nil && listenerErr == nil && tokenPath == listenerPath
}

func allowedID(ids []string, id string) bool {
	if len(ids) == 0 {
		return true
	}
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header()["WWW-Authenticate"] = []string{"Bearer"}
	writeProblem(w, 401, authFailureBody)
}

func writeProblem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func retryResponse(w http.ResponseWriter, d time.Duration) {
	if d < time.Second {
		d = time.Second
	}
	w.Header().Set("Retry-After", fmtInt(int((d+time.Second-1)/time.Second)))
	writeProblem(w, 429, authBusyBody)
}

func fmtInt(i int) string { return strconv.Itoa(i) }

func StillValid(p Principal, now time.Time) bool {
	if p.verifier == nil {
		return false
	}
	if p.expires != nil && !now.Before(*p.expires) {
		return false
	}
	t, err := p.verifier.ShowAPIToken(p.TokenID)
	if err != nil {
		return p.bootstrap && errors.Is(err, config.ErrAPITokenNotFound) && (p.expires == nil || now.Before(*p.expires))
	}
	return t.RevokedAt == nil && (t.Expires == nil || now.Before(*t.Expires))
}

func RunUsageFlusher(ctx context.Context, f TokenFlusher, every time.Duration, factories ...func(time.Duration) Ticker) {
	if every <= 0 {
		return
	}
	var factory func(time.Duration) Ticker
	if len(factories) > 0 {
		factory = factories[0]
	}
	if factory == nil {
		factory = func(d time.Duration) Ticker { return realTicker{t: time.NewTicker(d)} }
	}
	tick := factory(every)
	defer tick.Stop()
	runFlusher(ctx, f, tick.C())
}

func runFlusher(ctx context.Context, f TokenFlusher, c <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c:
			if err := f.Flush(); err != nil {
				slog.Error("API token usage flush failed", "error", Redact(err.Error()))
			}
		}
	}
}

type limitedBody struct {
	io.ReadCloser
	tooLarge bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if bodyTooLarge(err) {
		b.tooLarge = true
	}
	return n, err
}
