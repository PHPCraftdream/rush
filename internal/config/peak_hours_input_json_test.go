package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeakHoursJSONRejectsUnknownFieldsAndAmbiguousTimezone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data string
	}{
		{"legacy unknown", `{"start":"09:00","end":"10:00","unknown":true}`},
		{"policy unknown", `{"windows":[{"start":"09:00","end":"10:00"}],"unknown":true}`},
		{"window unknown", `{"windows":[{"start":"09:00","end":"10:00","unknown":true}]}`},
		{"timezone without windows", `{"timezone":"UTC"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var value ProviderPeakHours
			require.Error(t, json.Unmarshal([]byte(tc.data), &value))
		})
	}
}

func TestPeakHoursJSONKeepsLegacyFragmentsAndExplicitOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		data   string
		policy bool
	}{
		{"partial legacy overlay", `{"start":"09:00"}`, false},
		{"legacy message", `{"message":"note"}`, false},
		{"explicit off", `{"timezone":"UTC","windows":[]}`, true},
		{"nil windows off", `{"timezone":"UTC","windows":null}`, true},
		{"pause only", `{"paused_until":"2026-11-09T18:00:00Z"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var value ProviderPeakHours
			require.NoError(t, json.Unmarshal([]byte(tc.data), &value))
			require.Equal(t, tc.policy, value.Policy != nil)
			require.Equal(t, !tc.policy, value.Legacy != nil)
		})
	}
}
