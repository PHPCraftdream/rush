package codexprovider

import (
	"errors"
	"net/http"

	"charm.land/fantasy"
)

// webSocketStreamFailure turns a stream-consumer error into the terminal error
// for a Codex WebSocket exchange. The request was already sent, so every result
// except a certified context rejection stays non-replayable.
func webSocketStreamFailure(conn WebSocketConnection, err error, acceptedOutput bool) error {
	var rejection *safeContextRejection
	if !acceptedOutput && errors.As(err, &rejection) {
		// The server rejected the request before any output: retire the
		// connection (clearing delta state) but keep the certified error so
		// the agent can recover with a full frame on a fresh connection.
		_ = conn.Close()
		return err
	}
	terminal := conn.Abort(err)
	var providerErr *fantasy.ProviderError
	if !acceptedOutput && errors.As(err, &providerErr) && providerErr != nil {
		return &webSocketProviderFailure{terminal: terminal, provider: nonRetryableProviderView(providerErr)}
	}
	return terminal
}

// webSocketProviderFailure keeps the no-replay terminal error while exposing a
// provider verdict made before any output, for limit/context classification.
type webSocketProviderFailure struct {
	terminal error
	provider *fantasy.ProviderError
}

func (e *webSocketProviderFailure) Error() string { return e.provider.Error() }

func (e *webSocketProviderFailure) Unwrap() []error {
	if e.terminal == nil {
		return []error{e.provider}
	}
	return []error{e.terminal, e.provider}
}

func (e *webSocketProviderFailure) RetrySafe() bool { return false }
func (e *webSocketProviderFailure) NoReplay() bool  { return true }

// nonRetryableProviderView copies err minus every signal Fantasy's retry
// middleware reads, so visibility to errors.As cannot re-send the frame.
func nonRetryableProviderView(err *fantasy.ProviderError) *fantasy.ProviderError {
	view := *err
	view.Cause = nil
	view.TransientError = false
	view.ResponseHeaders = nil
	switch {
	case view.StatusCode == http.StatusRequestTimeout, view.StatusCode == http.StatusConflict,
		view.StatusCode == http.StatusTooManyRequests, view.StatusCode >= http.StatusInternalServerError:
		view.StatusCode = 0
	}
	return &view
}
