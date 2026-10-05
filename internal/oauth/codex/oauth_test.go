package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
)

func TestLoginBrowserPKCEStateAndExactCallback(t *testing.T) {
	var callbackListener net.Listener
	boundBeforeURL := false
	var expectedChallenge atomic.Value
	var expectedState string
	var exchangeCount atomic.Int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("token exchange request = %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		verifier := r.Form.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		if got := base64.RawURLEncoding.EncodeToString(hash[:]); got != expectedChallenge.Load().(string) {
			t.Errorf("PKCE challenge derived from verifier = %q, want %q", got, expectedChallenge.Load())
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "one-time-code" {
			t.Errorf("unexpected code exchange form: %v", r.Form)
		}
		if r.Form.Get("redirect_uri") != redirectURI {
			t.Errorf("redirect_uri = %q, want %q", r.Form.Get("redirect_uri"), redirectURI)
		}
		exchangeCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, tokenJSON("browser-account", "browser-access", "browser-refresh"))
	}))
	defer tokenServer.Close()

	listen := func() (net.Listener, error) {
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		callbackListener = listener
		boundBeforeURL = err == nil
		return listener, err
	}
	var callbackStatus int
	token, err := loginBrowser(context.Background(), func(rawURL string) error {
		if !boundBeforeURL || callbackListener == nil {
			return fmt.Errorf("callback listener was not bound before URL delivery")
		}
		loginURL, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		query := loginURL.Query()
		if loginURL.Scheme != "https" || loginURL.Host != "auth.example.test" || loginURL.Path != "/oauth/authorize" {
			return fmt.Errorf("unexpected authorization URL: %s", rawURL)
		}
		if query.Get("client_id") != clientID || query.Get("redirect_uri") != redirectURI || query.Get("scope") != scope || query.Get("code_challenge_method") != "S256" {
			return fmt.Errorf("unexpected OAuth authorization parameters: %v", query)
		}
		if query.Get("id_token_add_organizations") != "true" || query.Get("codex_cli_simplified_flow") != "true" || query.Get("originator") != "omp" {
			return fmt.Errorf("missing Codex authorization parameters: %v", query)
		}
		challenge := query.Get("code_challenge")
		expectedChallenge.Store(challenge)
		expectedState = query.Get("state")
		if len(challenge) != 43 || len(expectedState) != 43 {
			return fmt.Errorf("unexpected PKCE/state lengths: challenge=%d state=%d", len(challenge), len(expectedState))
		}

		callbackURL := "http://" + callbackListener.Addr().String() + "/auth/callback"
		client := &http.Client{Transport: &http.Transport{Proxy: nil}}
		badRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callbackURL+"?state=incorrect&code=attacker-code", nil)
		if err != nil {
			return err
		}
		badRequest.Host = "localhost:1455"
		badResponse, err := client.Do(badRequest)
		if err != nil {
			return err
		}
		badResponse.Body.Close()
		if badResponse.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("wrong-state callback status = %d, want %d", badResponse.StatusCode, http.StatusBadRequest)
		}

		goodRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callbackURL+"?state="+url.QueryEscape(expectedState)+"&code=one-time-code", nil)
		if err != nil {
			return err
		}
		goodRequest.Host = "localhost:1455"
		response, err := client.Do(goodRequest)
		if err != nil {
			return err
		}
		callbackStatus = response.StatusCode
		response.Body.Close()
		return nil
	}, http.DefaultClient, listen, "https://auth.example.test/oauth/authorize", tokenServer.URL+"/token")
	if err != nil {
		t.Fatal(err)
	}
	if callbackStatus != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", callbackStatus, http.StatusOK)
	}
	if exchangeCount.Load() != 1 {
		t.Fatalf("token exchanges = %d, want 1", exchangeCount.Load())
	}
	if token.AccessToken == "" || token.RefreshToken != "browser-refresh" || token.AccountID != "browser-account" {
		t.Fatalf("browser token = %#v", token)
	}
}

func TestLoginBrowserCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, err = loginBrowser(ctx, func(string) error {
		cancel()
		return nil
	}, http.DefaultClient, func() (net.Listener, error) { return listener, nil }, authorizeURL, tokenURL)
	if err != context.Canceled {
		t.Fatalf("browser login error = %v, want context.Canceled", err)
	}
}

func TestCallbackRejectsNonLoopbackOrIncorrectHost(t *testing.T) {
	for _, test := range []struct {
		name       string
		host       string
		remoteAddr string
		query      string
	}{
		{name: "non-loopback peer", host: "localhost:1455", remoteAddr: "203.0.113.8:4567", query: "state=expected&code=code"},
		{name: "wrong Host", host: "127.0.0.1:1455", remoteAddr: "127.0.0.1:4567", query: "state=expected&code=code"},
		{name: "duplicate state", host: "localhost:1455", remoteAddr: "127.0.0.1:4567", query: "state=expected&state=other&code=code"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := make(chan callbackResult, 1)
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:1455/auth/callback?"+test.query, nil)
			request.Host = test.host
			request.RemoteAddr = test.remoteAddr
			response := httptest.NewRecorder()
			callbackHandler("expected", result).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("callback status = %d, want %d", response.Code, http.StatusBadRequest)
			}
			select {
			case got := <-result:
				t.Fatalf("rejected callback produced result: %#v", got)
			default:
			}
		})
	}
}

func TestLoginDeviceAuthorizationAndRefreshRotation(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usercode":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode user-code request: %v", err)
			}
			if request["client_id"] != clientID {
				t.Errorf("client_id = %q", request["client_id"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"device-id","user_code":"ABCD-EFGH","interval":"0.001"}`)
		case "/device-token":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode device poll: %v", err)
			}
			if request["device_auth_id"] != "device-id" || request["user_code"] != "ABCD-EFGH" {
				t.Errorf("device poll request = %v", request)
			}
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"authorization_code":"device-auth-code","code_verifier":"device-verifier"}`)
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse code exchange: %v", err)
			}
			if r.Form.Get("code") != "device-auth-code" || r.Form.Get("code_verifier") != "device-verifier" || r.Form.Get("redirect_uri") != deviceRedirectURI {
				t.Errorf("unexpected device token request: %v", r.Form)
			}
			_, _ = io.WriteString(w, tokenJSON("device-account", "device-access", "device-refresh"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var displayedURL, displayedCode string
	token, err := loginDevice(context.Background(), func(authURL, code string) error {
		displayedURL, displayedCode = authURL, code
		return nil
	}, server.Client(), server.URL+"/usercode", server.URL+"/device-token", server.URL+"/token", time.Millisecond, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if displayedURL != deviceAuthURL || displayedCode != "ABCD-EFGH" {
		t.Fatalf("displayed device authorization = %q %q", displayedURL, displayedCode)
	}
	if polls.Load() != 2 {
		t.Fatalf("device polls = %d, want 2", polls.Load())
	}
	if token.AccessToken == "" || token.RefreshToken != "device-refresh" || token.AccountID != "device-account" {
		t.Fatalf("device token = %#v", token)
	}
}

func TestLoginDeviceCancellationDuringPolling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_auth_id":"device-id","user_code":"ABCD","interval":"30"}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := loginDevice(ctx, func(string, string) error {
		cancel()
		return nil
	}, server.Client(), server.URL, server.URL+"/poll", server.URL+"/token", time.Second, 0, 2)
	if err != context.Canceled {
		t.Fatalf("device login error = %v, want context.Canceled", err)
	}
}

func TestRefreshUsesRotatedCredentialAndExtractsAccount(t *testing.T) {
	var gotRefresh atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse refresh request: %v", err)
		}
		gotRefresh.Store(r.Form.Get("refresh_token"))
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != clientID {
			t.Errorf("unexpected refresh form: %v", r.Form)
		}
		_, _ = io.WriteString(w, tokenJSON("rotated-account", "rotated-access", "rotated-refresh"))
	}))
	defer server.Close()

	token, err := refresh(context.Background(), server.Client(), server.URL, "previous-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if gotRefresh.Load().(string) != "previous-refresh" {
		t.Fatalf("refresh token sent = %q, want previous-refresh", gotRefresh.Load())
	}
	if token.AccessToken == "" || token.RefreshToken != "rotated-refresh" || token.AccountID != "rotated-account" {
		t.Fatalf("refreshed token = %#v", token)
	}
}

func TestRefreshFallsBackToIDTokenAccountClaim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"access_token":%q,"id_token":%q,"refresh_token":"rotated","expires_in":3600}`,
			jwtWithSubject("", "access-subject"),
			jwtWithAccountID("id-token-account"),
		))
	}))
	defer server.Close()

	token, err := refresh(context.Background(), server.Client(), server.URL, "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if token.AccountID != "id-token-account" {
		t.Fatalf("AccountID = %q, want id-token-account", token.AccountID)
	}
}

func TestTokenEndpointErrorDoesNotLeakRefreshToken(t *testing.T) {
	secret := "refresh-secret-do-not-log"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"`+secret+`"}`)
	}))
	defer server.Close()

	_, err := refresh(context.Background(), server.Client(), server.URL, secret)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("refresh error leaked credential: %v", err)
	}
	tokenError, ok := err.(*oauth.TokenExchangeError)
	if !ok || !tokenError.IsRefreshTokenRevoked() {
		t.Fatalf("refresh error = %T %v, want revocation-aware token error", err, err)
	}
}

func tokenJSON(account, access, refresh string) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600}`, jwtWithSubject(account, access), refresh)
}

func jwtWithAccountID(account string) string {
	return jwtWithSubject(account, "")
}

func jwtWithSubject(account, subject string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub":                         subject,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account},
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}
