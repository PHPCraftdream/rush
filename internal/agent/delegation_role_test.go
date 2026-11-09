package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentRequestedRoleJSON(t *testing.T) {
	t.Parallel()
	for _, role := range []RequestedModelRole{"smart", "fast", "worker", "reviewer"} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(map[string]any{"prompt": "p", "role": role})
			require.NoError(t, err)
			var params AgentParams
			require.NoError(t, json.Unmarshal(raw, &params))
			require.Equal(t, role, params.Role)
			require.NoError(t, validateAgentRoleInput(raw))
			encoded, err := json.Marshal(params)
			require.NoError(t, err)
			var roundTrip AgentParams
			require.NoError(t, json.Unmarshal(encoded, &roundTrip))
			require.Equal(t, role, roundTrip.Role)
		})
	}
}

func TestAgentRequestedRoleOmittedPreservesLegacy(t *testing.T) {
	t.Parallel()
	var params AgentParams
	require.NoError(t, json.Unmarshal([]byte(`{"prompt":"p"}`), &params))
	require.Empty(t, params.Role)
	encoded, err := json.Marshal(params)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"role"`)
	require.NoError(t, validateAgentRoleInput(encoded))
}

func TestAgentRequestedRoleRejectsInvalidExplicitValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`null`, `""`, `"SMART"`, `"auto"`, `"unknown"`, `7`, `true`, `{}`, `[]`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"prompt":"p","role":` + value + `}`)
			var params AgentParams
			require.ErrorIs(t, json.Unmarshal(raw, &params), ErrAgentRoleInvalid)
			require.ErrorIs(t, validateAgentRoleInput(raw), ErrAgentRoleInvalid)
		})
	}
}
