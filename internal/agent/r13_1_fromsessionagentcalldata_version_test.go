package agent

// R13-1 (P2, security review round 13) regression tests:
// FromSessionAgentCallData used to discard fromSessionCallOptionsSpec's
// version error via the callOptionsFromCallData helper, silently degrading
// a version-mismatched durable row to a nil CallOptions — indistinguishable
// from "no CallOptions was ever set". The production consumer
// (coordinator.RebuildSessionAgentCall) was already covered by r6_4; these
// tests close the same gap on the other converter.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/session"
)

// TestFromSessionAgentCallData_UnsupportedCallOptionsSpecVersion_ReturnsError
// is the core fail-closed case: a durable row whose CallOptionsSpec carries
// an unsupported version must make FromSessionAgentCallData return an error
// wrapping ErrCallOptionsSpecVersionUnsupported and a zero struct — never a
// partially decoded call whose nil CallOptions masquerades as the legacy
// "no CallOptions was ever set" case.
func TestFromSessionAgentCallData_UnsupportedCallOptionsSpecVersion_ReturnsError(t *testing.T) {
	originalCall := SessionAgentCall{
		SessionID: "r13-1-unsupported-version-probe",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			DisableSubAgents: true,
		},
	}

	// Real JSON round trip through the durable-queue boundary, then
	// tamper with the version AFTER the legitimate round trip so this
	// test proves the version check itself, not a JSON-marshaling quirk.
	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.CallOptionsSpec,
		"precondition: the row must carry a CallOptionsSpec after the round trip")
	require.Equal(t, session.CallOptionsSpecVersion, decoded.CallOptionsSpec.Version,
		"precondition: the row must have round-tripped with the current version before tampering")
	decoded.CallOptionsSpec.Version = session.CallOptionsSpecVersion + 1

	call, err := FromSessionAgentCallData(decoded)
	require.Error(t, err,
		"a version-mismatched row must be refused, not silently degraded to nil CallOptions")
	assert.True(t, errors.Is(err, ErrCallOptionsSpecVersionUnsupported),
		"the refusal must be traceable to the version-compatibility check")
	assert.Nil(t, call.CallOptions, "no partial decode: CallOptions must stay nil")
	assert.Empty(t, call.LogicalCallID, "no partial decode: a zero struct must be returned")
	assert.Empty(t, call.Prompt, "no partial decode: a zero struct must be returned")
}

// TestFromSessionAgentCallData_AcceptedVersion_StillDecodesCallOptions is
// the happy path: an unmodified, current-version row keeps decoding
// normally, with the CallOptions execution policy surviving the JSON
// boundary.
func TestFromSessionAgentCallData_AcceptedVersion_StillDecodesCallOptions(t *testing.T) {
	originalCall := SessionAgentCall{
		SessionID: "r13-1-accepted-version-probe",
		Prompt:    "hello",
		CallOptions: &CallOptions{
			DisableSubAgents: true,
		},
	}

	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))

	call, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.NotNil(t, call.CallOptions)
	assert.True(t, call.CallOptions.DisableSubAgents,
		"the replay-relevant CallOptions field must survive the round trip")
}

// TestFromSessionAgentCallData_NilSpec_LegacyStillNoError pins the legacy
// contract that must NOT regress: a row with no CallOptionsSpec at all
// (a pre-R5-3 durable row, or a call that never carried CallOptions)
// keeps decoding to a nil CallOptions with no error.
func TestFromSessionAgentCallData_NilSpec_LegacyStillNoError(t *testing.T) {
	originalCall := SessionAgentCall{
		SessionID: "r13-1-nil-spec-probe",
		Prompt:    "hello",
	}

	callData := ToSessionAgentCallData(originalCall)
	raw, err := json.Marshal(callData)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))

	call, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	assert.Nil(t, call.CallOptions)
}
