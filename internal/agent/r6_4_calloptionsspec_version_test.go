package agent

// R6-4 (P2, independent security review round 6) regression tests:
// session.CallOptionsSpec.Version was write-only metadata before this fix
// — toSessionCallOptionsSpec always stamped session.CallOptionsSpecVersion,
// but fromSessionCallOptionsSpec ignored the field entirely and
// reconstructed CallOptions from whatever fields happened to decode. An
// older binary reading a newer schema's durable row could silently drop
// an unknown execution-policy field and replay the turn with weaker
// semantics than the row actually declared.
//
// fromSessionCallOptionsSpec now validates spec.Version before
// reconstructing anything, and the production durable-restart consumer
// (coordinator.RebuildSessionAgentCall) propagates that failure instead of
// swallowing it. Four scenarios below, matching the review's required
// coverage: legacy nil, accepted v1, malformed/zero version, and a future
// unsupported version — the last two both exercise the same fail-closed
// path with different concrete version values.

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/session"
)

// TestFromSessionCallOptionsSpec_NilSpec_LegacyNoError is the "legacy nil"
// case: a row persisted before CallOptionsSpec existed at all (or a call
// that never carried CallOptions) must keep decoding to (nil, nil) — no
// error, no options — exactly as before this fix.
func TestFromSessionCallOptionsSpec_NilSpec_LegacyNoError(t *testing.T) {
	opts, err := fromSessionCallOptionsSpec(nil)
	require.NoError(t, err)
	assert.Nil(t, opts)
}

// TestFromSessionCallOptionsSpec_AcceptedVersion_Decodes is the "accepted
// v1" case: a spec stamped with the current session.CallOptionsSpecVersion
// decodes normally, with every field surviving.
func TestFromSessionCallOptionsSpec_AcceptedVersion_Decodes(t *testing.T) {
	spec := &session.CallOptionsSpec{
		Version:                  session.CallOptionsSpecVersion,
		DisableSubAgents:         true,
		ModelRole:                "fast",
		TimeoutOptionsSet:        true,
		TimeoutExtendsOnProgress: true,
		TimeoutHardCap:           1234,
	}
	opts, err := fromSessionCallOptionsSpec(spec)
	require.NoError(t, err)
	require.NotNil(t, opts)
	assert.True(t, opts.DisableSubAgents)
	assert.EqualValues(t, "fast", opts.ModelRole)
	assert.True(t, opts.TimeoutOptionsSet)
	assert.True(t, opts.TimeoutExtendsOnProgress)
	assert.EqualValues(t, 1234, opts.TimeoutHardCap)
}

// TestFromSessionCallOptionsSpec_ZeroVersion_FailsClosed is the
// "malformed/zero version" case: a non-nil spec whose Version is the zero
// value (never a legitimate stamped version — toSessionCallOptionsSpec
// always stamps session.CallOptionsSpecVersion, currently 1) must be
// refused rather than partially decoded.
func TestFromSessionCallOptionsSpec_ZeroVersion_FailsClosed(t *testing.T) {
	spec := &session.CallOptionsSpec{
		Version:          0,
		DisableSubAgents: true,
	}
	opts, err := fromSessionCallOptionsSpec(spec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCallOptionsSpecVersionUnsupported)
	assert.Nil(t, opts, "a refused spec must never partially decode")
}

// TestFromSessionCallOptionsSpec_FutureVersion_FailsClosed is the "future
// unsupported version" case: a spec stamped by a NEWER binary's
// incompatible schema must also be refused, not silently decoded with
// whatever fields this binary happens to recognize.
func TestFromSessionCallOptionsSpec_FutureVersion_FailsClosed(t *testing.T) {
	spec := &session.CallOptionsSpec{
		Version:          session.CallOptionsSpecVersion + 1,
		DisableSubAgents: true,
	}
	opts, err := fromSessionCallOptionsSpec(spec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCallOptionsSpecVersionUnsupported)
	assert.Nil(t, opts)
}

// TestRebuildSessionAgentCall_UnsupportedCallOptionsSpecVersion_RefusesRow
// drives the REAL durable-replay consumer end to end (not just the
// conversion function in isolation, mirroring R6-3's own discipline): a
// durable row whose CallOptionsSpec carries a future, unsupported version
// must make RebuildSessionAgentCall refuse the whole row rather than
// return a partially-reconstructed CallOptions that silently drops the
// unrecognized policy fields.
func TestRebuildSessionAgentCall_UnsupportedCallOptionsSpecVersion_RefusesRow(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	originalCall := SessionAgentCall{
		SessionID: "r6-4-unsupported-version-probe",
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
	require.NotNil(t, decoded.CallOptionsSpec)
	require.Equal(t, session.CallOptionsSpecVersion, decoded.CallOptionsSpec.Version,
		"precondition: the row must have round-tripped with the current version before tampering")
	decoded.CallOptionsSpec.Version = session.CallOptionsSpecVersion + 1

	_, rebuildErr := coord.RebuildSessionAgentCall(t.Context(), decoded)
	require.Error(t, rebuildErr,
		"a durable row with an unsupported CallOptionsSpec version must refuse the row, not silently drop the field")
	assert.True(t, errors.Is(rebuildErr, ErrCallOptionsSpecVersionUnsupported),
		"the refusal must be traceable to the version-compatibility check")
}
