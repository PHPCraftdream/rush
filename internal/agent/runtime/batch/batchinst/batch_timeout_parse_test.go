package batchinst

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBatchParseTimeoutParam is the table over the retained parser copy.
// Every case names the production mutation it catches.
func TestBatchParseTimeoutParam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tool    string
		input   string
		wantNil bool
		wantErr string
		check   func(t *testing.T, it *TimeoutIntent)
	}{
		{
			// Mutation: treating the absence of a timeout as an error or as
			// a zero-intent (breaking every no-timeout batch call).
			name: "no timeout key yields no intent", tool: "bash",
			input: `{"command":"x"}`, wantNil: true,
		},
		{
			// Mutation: dropping bare-number acceptance (#1159 contract).
			name: "bare number is wake_only", tool: "bash",
			input: `{"command":"x","timeout":30}`, wantNil: false,
			check: func(t *testing.T, it *TimeoutIntent) {
				require.Equal(t, TimeoutWakeOnly, it.Kind)
				require.Equal(t, 30, it.Seconds)
				require.Equal(t, 30*time.Second, it.Duration)
			},
		},
		{
			name: "wake_only object", tool: "run_command",
			input: `{"cmd":"x","timeout":{"seconds":10,"kind":"wake_only"}}`,
			check: func(t *testing.T, it *TimeoutIntent) {
				require.Equal(t, TimeoutWakeOnly, it.Kind)
				require.Equal(t, 10, it.Seconds)
			},
		},
		{
			name: "terminate_and_wake object", tool: "bash",
			input: `{"command":"x","timeout":{"seconds":10,"kind":"terminate_and_wake"}}`,
			check: func(t *testing.T, it *TimeoutIntent) {
				require.Equal(t, TimeoutTerminateAndWake, it.Kind)
				require.Equal(t, 10, it.Seconds)
			},
		},
		{
			// Mutation: dropping the legacy run_command alias.
			name: "timeout_seconds alias is terminate", tool: "run_command",
			input: `{"timeout_seconds":45}`,
			check: func(t *testing.T, it *TimeoutIntent) {
				require.Equal(t, TimeoutTerminateAndWake, it.Kind)
				require.Equal(t, 45, it.Seconds)
			},
		},
		{
			// Mutation: silently preferring one axis when both are set.
			name: "both timeout forms rejected", tool: "run_command",
			input:   `{"timeout_seconds":45,"timeout":30}`,
			wantErr: "provide at most one of timeout or the legacy timeout_seconds",
		},
		{
			// Mutation: floor drift below 5 seconds.
			name: "seconds below floor rejected", tool: "bash",
			input:   `{"timeout":{"seconds":4,"kind":"wake_only"}}`,
			wantErr: "must be between 5 and 604800",
		},
		{
			// Mutation: ceil drift past 7 days.
			name: "seconds above ceil rejected", tool: "bash",
			input:   `{"timeout":{"seconds":604801,"kind":"wake_only"}}`,
			wantErr: "must be between 5 and 604800",
		},
		{
			// Mutation: accepting an unknown kind string.
			name: "bogus kind rejected", tool: "bash",
			input:   `{"timeout":{"seconds":10,"kind":"bogus"}}`,
			wantErr: "timeout.kind must be",
		},
		{
			// Mutation: accepting a non-numeric, non-object shape.
			name: "string shape rejected", tool: "bash",
			input:   `{"timeout":"abc"}`,
			wantErr: "timeout must be an object",
		},
		{
			// Mutation: accepting a container shape.
			name: "array shape rejected", tool: "bash",
			input:   `{"timeout":[]}`,
			wantErr: "timeout must be an object",
		},
		{
			// Mutation: applying the timeout contract to tools that never
			// promised a timeout field (agentic_fetch owns its HTTP client
			// timeout; view has none).
			name: "agentic_fetch input timeout ignored", tool: "agentic_fetch",
			input: `{"url":"x","timeout":30}`, wantNil: true,
		},
		{
			name: "view input timeout ignored", tool: "view",
			input: `{"path":"x","timeout":30}`, wantNil: true,
		},
		{
			// Mutation: treating an explicit null timeout as an intent.
			name: "null timeout yields no intent", tool: "bash",
			input: `{"command":"x","timeout":null}`, wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			it, err := ParseTimeoutParam(tc.tool, tc.input)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, it)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, it)
				return
			}
			require.NotNil(t, it)
			if tc.check != nil {
				tc.check(t, it)
			}
		})
	}
}
