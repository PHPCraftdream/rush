package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
)

const (
	clientID = "Iv1.b507a08c87ecfe98"

	deviceCodeURL   = "https://github.com/login/device/code"
	accessTokenURL  = "https://github.com/login/oauth/access_token"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

	// maxOAuthBodyBytes caps how much of an OAuth-related HTTP response body
	// we read into memory. These are small JSON token/error payloads from
	// GitHub, but the response is still a real network endpoint, not a
	// trusted local source, so an unbounded io.ReadAll/Decode is avoided.
	// Matches internal/oauth/hyper/device.go's limit for the same class of
	// device-code-flow responses.
	maxOAuthBodyBytes = 1 << 20 // 1MB
)

var ErrNotAvailable = errors.New("github copilot not available")

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// RequestDeviceCode initiates the device code flow with GitHub.
func RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", "read:user")

	req, err := http.NewRequestWithContext(ctx, "POST", deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBodyBytes))
		return nil, fmt.Errorf("device code request failed: %s - %s", resp.Status, string(body))
	}

	var dc DeviceCode
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&dc); err != nil {
		return nil, err
	}
	return &dc, nil
}

// PollForToken polls GitHub for the access token after user authorization.
func PollForToken(ctx context.Context, dc *DeviceCode) (*oauth.Token, error) {
	interval := max(dc.Interval, 5)
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		token, err := tryGetToken(ctx, dc.DeviceCode)
		if err == errPending {
			continue
		}
		if err == errSlowDown {
			interval += 5
			ticker.Reset(time.Duration(interval) * time.Second)
			continue
		}
		if err != nil {
			return nil, err
		}
		return token, nil
	}

	return nil, fmt.Errorf("authorization timed out")
}

var (
	errPending  = fmt.Errorf("pending")
	errSlowDown = fmt.Errorf("slow_down")
)

func tryGetToken(ctx context.Context, deviceCode string) (*oauth.Token, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("device_code", deviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, "POST", accessTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthBodyBytes)).Decode(&result); err != nil {
		return nil, err
	}

	switch result.Error {
	case "":
		if result.AccessToken == "" {
			return nil, errPending
		}
		return getCopilotToken(ctx, nil, result.AccessToken)
	case "authorization_pending":
		return nil, errPending
	case "slow_down":
		return nil, errSlowDown
	default:
		return nil, fmt.Errorf("authorization failed: %s", result.Error)
	}
}

func getCopilotToken(ctx context.Context, client *http.Client, githubToken string) (*oauth.Token, error) {
	client = refreshHTTPClient(client)
	req, err := http.NewRequestWithContext(ctx, "GET", copilotTokenURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", githubToken))
	for k, v := range Headers() {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBodyBytes))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrNotAvailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token request failed: %s - %s", resp.Status, string(body))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	copilotToken := &oauth.Token{
		AccessToken:  result.Token,
		RefreshToken: githubToken,
		ExpiresAt:    result.ExpiresAt,
	}
	copilotToken.SetExpiresIn()

	return copilotToken, nil
}

// RefreshToken refreshes the Copilot token using the GitHub token, on
// the default network route.
func RefreshToken(ctx context.Context, githubToken string) (*oauth.Token, error) {
	return RefreshTokenWithClient(ctx, nil, githubToken)
}

// RefreshTokenWithClient is RefreshToken with an explicit HTTP client
// (R5-2): callers that resolved a provider network policy (proxy/DNS
// transport) pass it here so the refresh request rides the same route
// as inference instead of silently bypassing it. A nil client keeps
// the historical bare default-route client.
func RefreshTokenWithClient(ctx context.Context, client *http.Client, githubToken string) (*oauth.Token, error) {
	return getCopilotToken(ctx, client, githubToken)
}

// refreshHTTPClient resolves the client one OAuth refresh request uses:
// nil keeps the historical bare default-route client, and a configured
// client that carries no timeout of its own gets the same 30s ceiling
// cloned on, so threading a Transport through never drops the deadline
// the bare client used to impose.
func refreshHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	if client.Timeout != 0 {
		return client
	}
	clone := *client
	clone.Timeout = 30 * time.Second
	return &clone
}
