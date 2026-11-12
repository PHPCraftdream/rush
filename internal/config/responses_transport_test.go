package config

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesTransportTypedPolicyAndPrecedence(t *testing.T) {
	for _, value := range []string{`"auto"`, `"http"`, `null`} {
		var policy ResponsesTransport
		require.NoError(t, json.Unmarshal([]byte(value), &policy))
	}
	for _, value := range []string{`""`, `"ws"`, `"AUTO"`, `1`, `true`, `{}`} {
		var policy ResponsesTransport
		require.Error(t, json.Unmarshal([]byte(value), &policy))
	}
	require.Equal(t, ResponsesTransportAuto, EffectiveResponsesTransport("", ""))
	require.Equal(t, ResponsesTransportHTTP, EffectiveResponsesTransport(ResponsesTransportHTTP, ""))
	require.Equal(t, ResponsesTransportAuto, EffectiveResponsesTransport(ResponsesTransportHTTP, ResponsesTransportAuto))
}

func TestResponsesTransportScopedWriteRejectsBeforeDiskCommit(t *testing.T) {
	store, path := isolatedLegacyStore(t, "global.json")
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.responses_transport", "http"))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, field := range []string{"options.responses_transport", "providers.openai.responses_transport"} {
		require.Error(t, store.SetConfigField(ScopeGlobal, field, "websocket"))
		after, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		require.Equal(t, before, after)
	}
	require.NoError(t, store.SetConfigField(ScopeGlobal, "providers.openai.responses_transport", "auto"))
	require.NoError(t, store.RemoveConfigField(ScopeGlobal, "options.responses_transport"))
}

func TestResponsesTransportParentWritesAreValidated(t *testing.T) {
	for _, key := range []string{"options", "providers", "providers.openai"} {
		require.Error(t, validateResponsesTransportWrite([]byte(`{"options":{"responses_transport":"ws"},"providers":{"openai":{"responses_transport":"ws"}}}`), []string{key}))
	}
	// Unrelated edits must not reinterpret legacy provider fields.
	require.NoError(t, validateResponsesTransportWrite([]byte(`{"options":{"responses_transport":"ws"}}`), []string{"options.debug"}))
}
