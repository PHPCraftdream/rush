package codexprovider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

func (m *languageModel) acquireResponse(ctx context.Context, payload []byte, userAgent string) (*http.Response, error) {
	used, ok := m.provider.source.Current()
	if !ok || strings.TrimSpace(used.AccessToken) == "" {
		return nil, errors.New("openai-codex OAuth credentials unavailable: provider removed, logged out, or access token empty")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := m.send(ctx, payload, userAgent, used)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			return response, nil
		}
		body, err := drainErrorResponse(ctx, response)
		if err != nil {
			return nil, err
		}
		original := providerError(response, payload, body)
		if response.StatusCode != http.StatusUnauthorized || attempt != 0 {
			return nil, original
		}
		refreshErr := m.provider.source.Refresh(ctx, used)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if refreshErr != nil {
			return nil, original
		}
		current, ok := m.provider.source.Current()
		if !ok || strings.TrimSpace(current.AccessToken) == "" || reflect.DeepEqual(current, used) {
			return nil, original
		}
		used = current
	}
	panic("unreachable Codex send attempt")
}

func drainErrorResponse(ctx context.Context, response *http.Response) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stop()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if err == nil {
		// Bounded drain keeps the connection reusable; the peer may never end the body.
		_, _ = io.CopyN(io.Discard, response.Body, maxErrorBody)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return body, err
}

func (m *languageModel) send(ctx context.Context, payload []byte, userAgent string, token oauth.Token) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	request.Header.Set("originator", "omp")
	request.Header.Set("version", codexVersion)
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "rush"
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("x-codex-routing-hint", "model="+m.modelID)
	if accountID := strings.TrimSpace(token.AccountID); accountID != "" {
		request.Header.Set("chatgpt-account-id", accountID)
	}
	if sessionID, ok := ctx.Value(cliprovider.SessionIDContextKey).(string); ok && sessionID != "" {
		request.Header.Set("conversation_id", sessionID)
		request.Header.Set("session_id", sessionID)
		request.Header.Set("x-client-request-id", sessionID)
	}
	return m.provider.client.Do(request)
}
