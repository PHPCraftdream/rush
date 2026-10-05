package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
)

const (
	clientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	authorizeURL      = "https://auth.openai.com/oauth/authorize"
	tokenURL          = "https://auth.openai.com/oauth/token"
	scope             = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	redirectURI       = "http://localhost:1455/auth/callback"
	deviceUserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	deviceAuthURL     = "https://auth.openai.com/codex/device"
	requestTimeout    = 15 * time.Second
	browserTimeout    = 10 * time.Minute
	devicePollEvery   = 5 * time.Second
	devicePollMargin  = 3 * time.Second
	deviceMaxPolls    = 120
	maxResponseBytes  = 1 << 20
)

type listenFunc func() (net.Listener, error)

type listenerResult struct {
	conn net.Conn
	err  error
}

type combinedListener struct {
	listeners []net.Listener
	addr      net.Addr
	accepted  chan listenerResult
	closed    chan struct{}
	closeOnce sync.Once
	acceptWG  sync.WaitGroup
}

func newCombinedListener(listeners ...net.Listener) net.Listener {
	combined := &combinedListener{
		listeners: listeners,
		addr:      listeners[0].Addr(),
		accepted:  make(chan listenerResult, len(listeners)),
		closed:    make(chan struct{}),
	}
	for _, listener := range listeners {
		combined.acceptWG.Add(1)
		go func(listener net.Listener) {
			defer combined.acceptWG.Done()
			combined.acceptLoop(listener)
		}(listener)
	}
	return combined
}

func (l *combinedListener) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		select {
		case l.accepted <- listenerResult{conn: conn, err: err}:
		case <-l.closed:
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
		if err != nil {
			return
		}
	}
}

func (l *combinedListener) Accept() (net.Conn, error) {
	select {
	case result := <-l.accepted:
		return result.conn, result.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *combinedListener) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		close(l.closed)
		for _, listener := range l.listeners {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				closeErr = errors.Join(closeErr, err)
			}
		}
		l.acceptWG.Wait()
		for {
			select {
			case result := <-l.accepted:
				if result.conn != nil {
					_ = result.conn.Close()
				}
			default:
				return
			}
		}
	})
	return closeErr
}

func (l *combinedListener) Addr() net.Addr {
	return l.addr
}

func listenLoopback(port string) (net.Listener, error) {
	return listenLoopbackWith(port, net.Listen)
}

type networkListenFunc func(network, address string) (net.Listener, error)

func listenLoopbackWith(port string, listen networkListenFunc) (net.Listener, error) {
	ipv4, err := listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	ipv4Addr, ok := ipv4.Addr().(*net.TCPAddr)
	if !ok {
		ipv4.Close()
		return nil, errors.New("IPv4 OAuth callback listener has no TCP address")
	}
	port = strconv.Itoa(ipv4Addr.Port)

	probe, err := listen("tcp6", net.JoinHostPort("::1", "0"))
	if err != nil {
		if isIPv6Unavailable(err) {
			return ipv4, nil
		}
		ipv4.Close()
		return nil, fmt.Errorf("could not probe IPv6 loopback for Codex OAuth callback: %w", err)
	}
	if err := probe.Close(); err != nil {
		ipv4.Close()
		return nil, fmt.Errorf("could not release IPv6 loopback probe: %w", err)
	}
	ipv6, err := listen("tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		ipv4.Close()
		return nil, fmt.Errorf("could not bind Codex OAuth callback on IPv6 loopback: %w", err)
	}
	return newCombinedListener(ipv4, ipv6), nil
}

func isIPv6Unavailable(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

// LoginBrowser completes ChatGPT subscription OAuth through the fixed loopback callback.
func LoginBrowser(ctx context.Context, onURL func(string) error) (*oauth.Token, error) {
	return loginBrowser(ctx, onURL, http.DefaultClient, func() (net.Listener, error) {
		return listenLoopback("1455")
	}, authorizeURL, tokenURL)
}

func loginBrowser(ctx context.Context, onURL func(string) error, client *http.Client, listen listenFunc, authEndpoint, exchangeEndpoint string) (*oauth.Token, error) {
	if onURL == nil {
		return nil, errors.New("codex browser login requires a URL callback")
	}
	flowCtx, cancel := context.WithTimeout(ctx, browserTimeout)
	defer cancel()

	listener, err := listen()
	if err != nil {
		return nil, fmt.Errorf("could not bind Codex OAuth callback at localhost:1455: %w", err)
	}
	if !listenerIsLoopback(listener) {
		listener.Close()
		return nil, errors.New("codex OAuth callback listener is not bound to loopback")
	}
	defer listener.Close()

	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, fmt.Errorf("could not create OAuth PKCE verifier: %w", err)
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return nil, fmt.Errorf("could not create OAuth state: %w", err)
	}
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])

	callback := make(chan callbackResult, 1)
	server := &http.Server{
		Handler:           callbackHandler(state, callback),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Close()

	loginURL, err := authorizationURL(authEndpoint, state, challenge)
	if err != nil {
		return nil, err
	}
	if err := onURL(loginURL); err != nil {
		return nil, fmt.Errorf("could not open Codex OAuth URL: %w", err)
	}

	var result callbackResult
	select {
	case <-flowCtx.Done():
		if errors.Is(flowCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, errors.New("codex browser authorization timed out")
		}
		return nil, flowCtx.Err()
	case result = <-callback:
	}
	if result.err != nil {
		return nil, result.err
	}
	return exchangeCode(flowCtx, client, exchangeEndpoint, result.code, verifier, redirectURI)
}

type callbackResult struct {
	code string
	err  error
}

func callbackHandler(expectedState string, result chan<- callbackResult) http.Handler {
	var callbackOnce sync.Once
	sendResult := func(value callbackResult) {
		callbackOnce.Do(func() {
			select {
			case result <- value:
			default:
			}
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet || r.Host != "localhost:1455" || !isLoopbackRemote(r.RemoteAddr) {
			http.Error(w, "invalid OAuth callback request", http.StatusBadRequest)
			return
		}
		query := r.URL.Query()
		states := query["state"]
		if len(states) != 1 || len(states[0]) != len(expectedState) || subtle.ConstantTimeCompare([]byte(states[0]), []byte(expectedState)) != 1 {
			http.Error(w, "OAuth state did not match", http.StatusBadRequest)
			return
		}
		oauthErrors := query["error"]
		if len(oauthErrors) > 1 {
			http.Error(w, "invalid OAuth callback request", http.StatusBadRequest)
			return
		}
		if len(oauthErrors) == 1 && oauthErrors[0] != "" {
			http.Error(w, "Codex authorization was denied", http.StatusBadRequest)
			sendResult(callbackResult{err: errors.New("codex authorization was denied")})
			return
		}
		codes := query["code"]
		if len(codes) != 1 || codes[0] == "" {
			http.Error(w, "OAuth callback did not contain an authorization code", http.StatusBadRequest)
			sendResult(callbackResult{err: errors.New("OAuth callback did not contain an authorization code")})
			return
		}
		code := codes[0]
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Authentication complete. You may close this window.")
		sendResult(callbackResult{code: code})
	})
}

func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func listenerIsLoopback(listener net.Listener) bool {
	addr, ok := listener.Addr().(*net.TCPAddr)
	return ok && addr.IP != nil && addr.IP.IsLoopback()
}

func authorizationURL(endpoint, state, challenge string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Codex authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "omp")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// LoginDevice completes the ChatGPT device authorization flow.
func LoginDevice(ctx context.Context, onCode func(url, code string) error) (*oauth.Token, error) {
	return loginDevice(ctx, onCode, http.DefaultClient, deviceUserCodeURL, deviceTokenURL, tokenURL, devicePollEvery, devicePollMargin, deviceMaxPolls)
}

func loginDevice(ctx context.Context, onCode func(url, code string) error, client *http.Client, userCodeEndpoint, deviceTokenEndpoint, exchangeEndpoint string, interval, safetyMargin time.Duration, maxPolls int) (*oauth.Token, error) {
	if onCode == nil {
		return nil, errors.New("codex device login requires a code callback")
	}
	client = oauthHTTPClient(client)
	initCtx, initCancel := context.WithTimeout(ctx, requestTimeout)
	defer initCancel()

	var initResponse struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err := postJSON(initCtx, client, userCodeEndpoint, map[string]string{"client_id": clientID}, &initResponse); err != nil {
		return nil, fmt.Errorf("codex device authorization initiation failed: %w", err)
	}
	if initResponse.DeviceAuthID == "" || initResponse.UserCode == "" {
		return nil, errors.New("codex device authorization response missing required fields")
	}
	pollDelay := devicePollDelay(initResponse.Interval, safetyMargin)
	deviceCtx, cancel := context.WithTimeout(ctx, time.Duration(maxPolls)*(max(interval, pollDelay)+requestTimeout)+requestTimeout)
	defer cancel()

	if err := onCode(deviceAuthURL, initResponse.UserCode); err != nil {
		return nil, fmt.Errorf("could not deliver Codex device code: %w", err)
	}

	for poll := range maxPolls {
		delay := pollDelay
		if poll == 0 && delay > interval {
			delay = interval
		}
		if err := waitContext(deviceCtx, delay); err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, errors.New("codex device authorization timed out")
			}
			return nil, err
		}
		status, body, err := postJSONResponse(deviceCtx, client, deviceTokenEndpoint, map[string]string{
			"device_auth_id": initResponse.DeviceAuthID,
			"user_code":      initResponse.UserCode,
		})
		if err != nil {
			return nil, fmt.Errorf("codex device token polling failed: %w", err)
		}
		if status == http.StatusForbidden || status == http.StatusNotFound {
			continue
		}
		if status < 200 || status >= 300 {
			return nil, &oauth.TokenExchangeError{StatusCode: status, Body: safeTokenError(body)}
		}
		var pollResponse struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if err := json.Unmarshal(body, &pollResponse); err != nil {
			return nil, fmt.Errorf("invalid Codex device token response: %w", err)
		}
		if pollResponse.AuthorizationCode == "" || pollResponse.CodeVerifier == "" {
			return nil, errors.New("codex device token response missing authorization_code or code_verifier")
		}
		return exchangeCode(deviceCtx, client, exchangeEndpoint, pollResponse.AuthorizationCode, pollResponse.CodeVerifier, deviceRedirectURI)
	}
	return nil, errors.New("codex device authorization timed out")
}

func devicePollDelay(raw json.RawMessage, safetyMargin time.Duration) time.Duration {
	seconds := 5.0
	if len(raw) > 0 {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil {
			if parsed, err := strconv.ParseFloat(n.String(), 64); err == nil && parsed > 0 {
				seconds = parsed
			}
		} else {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && parsed > 0 {
					seconds = parsed
				}
			}
		}
	}
	return time.Duration(seconds*float64(time.Second)) + safetyMargin
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Refresh rotates a ChatGPT OAuth refresh token using the configured network client.
func Refresh(ctx context.Context, client *http.Client, refreshToken string) (*oauth.Token, error) {
	if refreshToken == "" {
		return nil, errors.New("codex refresh token is empty")
	}
	return refresh(ctx, client, tokenURL, refreshToken)
}

func refresh(ctx context.Context, client *http.Client, endpoint, refreshToken string) (*oauth.Token, error) {
	return exchangeForm(ctx, client, endpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
}

func exchangeCode(ctx context.Context, client *http.Client, endpoint, code, verifier, callbackURI string) (*oauth.Token, error) {
	return exchangeForm(ctx, client, endpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {callbackURI},
	})
}

func exchangeForm(ctx context.Context, client *http.Client, endpoint string, values url.Values) (*oauth.Token, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("could not create Codex token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTPClient(client).Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not read Codex token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tokenError := &oauth.TokenExchangeError{
			StatusCode:  resp.StatusCode,
			Body:        safeTokenError(body),
			Diagnostics: tokenFailureDiagnostics(resp, body, values),
		}
		slog.Warn("OpenAI Codex OAuth token exchange rejected",
			"grant_type", values.Get("grant_type"), "status", resp.StatusCode,
			"details", tokenError.Diagnostics)
		return nil, tokenError
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid Codex token response: %w", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.ExpiresIn <= 0 {
		return nil, errors.New("codex token response missing access_token, refresh_token, or expires_in")
	}
	token := &oauth.Token{
		AccessToken:  response.AccessToken,
		RefreshToken: response.RefreshToken,
		ExpiresIn:    response.ExpiresIn,
	}
	token.AccountID = accountID(response.AccessToken)
	if token.AccountID == "" {
		token.AccountID = accountID(response.IDToken)
	}
	token.SetExpiresAt()
	return token, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, request any, response any) error {
	status, body, err := postJSONResponse(ctx, client, endpoint, request)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &oauth.TokenExchangeError{StatusCode: status, Body: safeTokenError(body)}
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("invalid Codex device response: %w", err)
	}
	return nil
}

func postJSONResponse(ctx context.Context, client *http.Client, endpoint string, value any) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, err := json.Marshal(value)
	if err != nil {
		return 0, nil, fmt.Errorf("could not encode Codex device request: %w", err)
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("could not create Codex device request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("codex device request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := readBounded(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("could not read Codex device response: %w", err)
	}
	return resp.StatusCode, responseBody, nil
}

func oauthHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	secured := *client
	secured.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &secured
}

func readBounded(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("OAuth response exceeded size limit")
	}
	return body, nil
}

func safeTokenError(body []byte) string {
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil {
		return ""
	}
	var code string
	if json.Unmarshal(response.Error, &code) != nil {
		var nested struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(response.Error, &nested) != nil {
			return ""
		}
		code = nested.Code
	}
	switch code {
	case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "access_denied", "revoked", "server_error", "temporarily_unavailable", "authorization_pending", "slow_down", "expired_token":
		return code
	default:
		return ""
	}
}

var (
	codexRayIDPattern     = regexp.MustCompile(`(?i)^[0-9a-f]{16,32}(?:-[a-z]{3})?$`)
	codexRequestIDPattern = regexp.MustCompile(`^req_[A-Za-z0-9_-]{8,64}$`)
)

func tokenFailureDiagnostics(resp *http.Response, body []byte, values url.Values) string {
	contentType := "missing"
	if raw := resp.Header.Get("Content-Type"); raw != "" {
		contentType = "other"
		if mediaType, _, err := mime.ParseMediaType(raw); err == nil {
			switch mediaType {
			case "application/json", "application/problem+json", "text/html", "text/plain":
				contentType = mediaType
			}
		}
	}
	trimmed := bytes.TrimSpace(body)
	kind := "other"
	switch {
	case len(trimmed) == 0:
		kind = "empty"
	case json.Valid(trimmed):
		kind = "json"
	case contentType == "text/html" || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype html")) || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html")):
		kind = "html"
	}
	var result strings.Builder
	fmt.Fprintf(&result, "kind=%s content_type=%s bytes=%d", kind, contentType, len(body))
	if code := safeTokenError(body); code != "" {
		fmt.Fprintf(&result, " oauth_error=%s", code)
	}
	if ray := resp.Header.Get("Cf-Ray"); safeCorrelationID(ray, codexRayIDPattern, values) {
		fmt.Fprintf(&result, " cf_ray=%s", ray)
	}
	requestID := resp.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = resp.Header.Get("X-Openai-Request-Id")
	}
	if safeCorrelationID(requestID, codexRequestIDPattern, values) {
		fmt.Fprintf(&result, " request_id=%s", requestID)
	}
	return result.String()
}

func safeCorrelationID(value string, pattern *regexp.Regexp, values url.Values) bool {
	if !pattern.MatchString(value) {
		return false
	}
	for _, key := range [...]string{"code", "code_verifier", "refresh_token"} {
		secret := values.Get(key)
		if secret != "" && (strings.Contains(secret, value) || strings.Contains(value, secret)) {
			return false
		}
	}
	return true
}

func randomURLSafe(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func accountID(token string) string {
	_, rest, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	payloadPart, signature, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(signature, ".") {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(payloadPart)
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}
