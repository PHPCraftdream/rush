package agent

// F7 (P2, 2026-09-22 weekly commit audit round 7) regression test: the
// durable CallOptionsSpec mirror and both converters dropped
// CallOptions.IdleTimeout entirely, so a durable-queue round trip reset a
// positive idle-stall override (e.g. `rush run --idle-timeout 5s`) to the
// shared/default policy and lost the deliberately-disabled sentinel the
// same way. This file proves all three states of the field — unset
// (zero), a positive override, and the disabled sentinel — survive a full
// live-to-durable-to-live round trip unchanged.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/session"
)

// f7IdleTimeoutDisabledSentinel mirrors idleTimeoutDisabledSentinel in
// internal/cmd/run.go (`--idle-timeout 0` is rewritten to this large
// positive duration before it ever reaches CallOptions, because the
// stream watchdog has no native off-switch). Inlined here rather than
// imported: internal/cmd transitively imports agent, so an import would
// cycle.
var f7IdleTimeoutDisabledSentinel = 100 * 365 * 24 * time.Hour

func TestF7_IdleTimeoutSurvivesDurableRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input time.Duration
	}{
		{"unset_zero_falls_back_to_shared_policy", 0},
		{"positive_override_5s", 5 * time.Second},
		{"disabled_sentinel_100y", f7IdleTimeoutDisabledSentinel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := SessionAgentCall{
				SessionID:   "f7-idle-timeout-roundtrip",
				Prompt:      "probe",
				CallOptions: &CallOptions{IdleTimeout: tc.input},
			}

			liveToDurable := ToSessionAgentCallData(original)
			require.NotNil(t, liveToDurable.CallOptionsSpec)
			assert.Equal(t, tc.input, liveToDurable.CallOptionsSpec.IdleTimeout,
				"live-to-durable conversion must carry IdleTimeout verbatim")

			if tc.input == 0 {
				// The unset representation is the omitempty zero value:
				// the JSON key must be absent, not persisted as 0.
				raw, err := json.Marshal(liveToDurable)
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "idle_timeout",
					"unset IdleTimeout must be absent from the durable JSON, not written as 0")
			}

			raw, err := json.Marshal(liveToDurable)
			require.NoError(t, err)
			var decoded session.SessionAgentCallData
			require.NoError(t, json.Unmarshal(raw, &decoded))

			backToLive, err := FromSessionAgentCallData(decoded)
			require.NoError(t, err)
			require.NotNil(t, backToLive.CallOptions)
			assert.Equal(t, tc.input, backToLive.CallOptions.IdleTimeout,
				"durable-to-live conversion must restore IdleTimeout unchanged")
		})
	}
}
