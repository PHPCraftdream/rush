package server

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPeakHoursWireJSONRejectsMalformedPolicyBeforePersistence(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		`{"windows":[],"unknown":true}`,
		`{"start":"09:00","end":"10:00","unknown":true}`,
		`{"windows":[{"start":"09:00","end":"10:00","unknown":true}]}`,
		`{"timezone":"UTC"}`,
		`{"start":"","end":"","windows":[]}`,
	} {
		var value PeakHoursWirePayload
		require.Error(t, json.Unmarshal([]byte(input), &value), input)
	}
}

func TestPeakHoursWireEmptyPolicyRoundTripPreservesDiscriminator(t *testing.T) {
	t.Parallel()
	wire := peakHoursToWire(&config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC", Message: "off but retained",
	}})
	data, err := json.Marshal(wire)
	require.NoError(t, err)
	require.Equal(t, "[]", gjson.GetBytes(data, "windows").Raw)
	var decoded PeakHoursWirePayload
	require.NoError(t, json.Unmarshal(data, &decoded))
	value, err := peakHoursFromWire(&decoded)
	require.NoError(t, err)
	require.NotNil(t, value.Policy)
	require.Nil(t, value.Legacy)
	require.Empty(t, value.Policy.Windows)
	require.Equal(t, "UTC", value.Policy.Timezone)
	require.Equal(t, "off but retained", value.Policy.Message)
	require.True(t, value.IsOff())
}

func TestPeakHoursWireLegacyAndNullWindowsRemainCompatible(t *testing.T) {
	t.Parallel()
	legacy := PeakHoursWirePayload{Start: "22:00", End: "06:00", Message: "legacy"}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.JSONEq(t, `{"start":"22:00","end":"06:00","message":"legacy"}`, string(data))
	var decoded PeakHoursWirePayload
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Nil(t, decoded.Windows)
	value, err := peakHoursFromWire(&decoded)
	require.NoError(t, err)
	require.Equal(t, &config.PeakHoursWindow{Start: "22:00", End: "06:00", Message: "legacy"}, value.Legacy)

	require.NoError(t, json.Unmarshal([]byte(`{"timezone":"UTC","windows":null}`), &decoded))
	require.NotNil(t, decoded.Windows)
	value, err = peakHoursFromWire(&decoded)
	require.NoError(t, err)
	require.NotNil(t, value.Policy)
	require.True(t, value.IsOff())
}
