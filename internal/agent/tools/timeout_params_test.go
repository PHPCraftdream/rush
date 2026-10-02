package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// #1159: a bare number of seconds is accepted as
// {"seconds": N, "kind": "wake_only"}; garbage still fails with a clear
// message. Revert-check: removing TimeoutParams.UnmarshalJSON makes the
// number and string cases fail (number -> unmarshal type error, string ->
// json's opaque UnmarshalTypeError).
func TestTimeoutParamsUnmarshalJSON(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		Timeout *TimeoutParams `json:"timeout"`
	}

	var withNumber wrapper
	require.NoError(t, json.Unmarshal([]byte(`{"timeout":300}`), &withNumber))
	require.NotNil(t, withNumber.Timeout)
	require.Equal(t, 300, withNumber.Timeout.Seconds)
	require.Equal(t, "wake_only", withNumber.Timeout.Kind)

	var withObject wrapper
	require.NoError(t, json.Unmarshal([]byte(`{"timeout":{"seconds":600,"kind":"terminate_and_wake"}}`), &withObject))
	require.NotNil(t, withObject.Timeout)
	require.Equal(t, 600, withObject.Timeout.Seconds)
	require.Equal(t, "terminate_and_wake", withObject.Timeout.Kind)

	var withNull wrapper
	require.NoError(t, json.Unmarshal([]byte(`{"timeout":null}`), &withNull))
	require.Nil(t, withNull.Timeout)

	var withString wrapper
	err := json.Unmarshal([]byte(`{"timeout":"600"}`), &withString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be an object")

	var withGarbage wrapper
	err = json.Unmarshal([]byte(`{"timeout":{"seconds":"many"}}`), &withGarbage)
	require.Error(t, err)
}
