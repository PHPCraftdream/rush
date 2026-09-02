package agent

// #859 Layer 2 tests (design doc §7.3, "belt and braces"): a durable row
// marked HostDiskProvider must NEVER be rebuilt into an executable call —
// its DiskProvider has no serializable form, so "rebuilding" it can only
// mean silently falling back to the real disk, exactly the silent
// restart promotion this feature exists to prevent. Every current
// producer already refuses to write such a row (Layer 1), so this is the
// fail-closed backstop for a producer added later or a row written by a
// different binary version.

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRebuildSessionAgentCall_RefusesRowMarkedHostDiskProvider is the
// mandatory revert-checked test for RebuildSessionAgentCall's Layer 2
// refusal.
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator_interrupt.go's RebuildSessionAgentCall, comment out
//     (or delete) the `if data.HostDiskProvider { ... return }` block at
//     the top of the function.
//  2. Run: go test ./internal/agent -run TestRebuildSessionAgentCall_RefusesRowMarkedHostDiskProvider -v
//  3. The test FAILS: RebuildSessionAgentCall succeeds and returns a
//     runnable SessionAgentCall instead of a terminal error.
//  4. Restore the block and the test passes again.
func TestRebuildSessionAgentCall_RefusesRowMarkedHostDiskProvider(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	data := session.SessionAgentCallData{
		SessionID:        "disk-provider-rebuild-refusal",
		LogicalCallID:    "logical-1",
		Prompt:           "hello",
		HostDiskProvider: true,
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.Error(t, err,
		"a row marked HostDiskProvider must never rebuild into a runnable call")
	require.ErrorIs(t, err, ErrDiskProviderNotDurable)
	require.Equal(t, SessionAgentCall{}, call)

	// Terminal, not retryable: the pump distinguishes the two via the
	// AlreadyAttempted() marker interface (session.AlreadyAttempted /
	// agent.ErrCallAlreadyAttempted) — retrying this row could never
	// succeed differently, since the provider it needs cannot exist in
	// any process but the one that built it.
	var alreadyAttempted *ErrCallAlreadyAttempted
	require.ErrorAs(t, err, &alreadyAttempted,
		"the refusal must be marked terminal so the run-queue pump never retries an unfixable row")
	require.True(t, alreadyAttempted.AlreadyAttempted())
}

// TestRebuildSessionAgentCall_RebuildsPlainRowsWithoutHostDiskProvider is
// the control: a row with HostDiskProvider left at its zero value (false)
// — the overwhelming majority of rows, including every one written
// before this field existed — rebuilds exactly as before.
func TestRebuildSessionAgentCall_RebuildsPlainRowsWithoutHostDiskProvider(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	data := session.SessionAgentCallData{
		SessionID:     "disk-provider-rebuild-control",
		LogicalCallID: "logical-2",
		Prompt:        "hello",
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)
	require.Equal(t, "disk-provider-rebuild-control", call.SessionID)
}

// TestSessionAgentCallDataHostDiskProviderJSONRoundTrip pins the durable
// JSON boundary: a call carrying a DiskProvider must serialize with
// HostDiskProvider true, and a plain call must serialize with it false —
// mirroring TestSessionAgentCallDataFolderScopeSpecJSONRoundTrip's shape
// for the folder-scope spec.
func TestSessionAgentCallDataHostDiskProviderJSONRoundTrip(t *testing.T) {
	fake := newFakeDiskProvider(nil)
	callWithProvider := SessionAgentCall{
		SessionID:   "json-diskprovider-roundtrip-probe",
		Prompt:      "hello",
		CallOptions: &CallOptions{DiskProvider: fake},
	}

	data := ToSessionAgentCallData(callWithProvider)
	require.True(t, data.HostDiskProvider,
		"ToSessionAgentCallData must mark a disk-provider-carrying call")

	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"HostDiskProvider":true`)

	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.True(t, decoded.HostDiskProvider,
		"the marker must survive the JSON boundary")

	// Control: a plain call (no CallOptions at all, and a call with
	// CallOptions but no DiskProvider) both serialize with the marker
	// false.
	plainCall := SessionAgentCall{SessionID: "json-diskprovider-roundtrip-plain", Prompt: "hello"}
	plainData := ToSessionAgentCallData(plainCall)
	require.False(t, plainData.HostDiskProvider)
	plainRaw, err := json.Marshal(plainData)
	require.NoError(t, err)
	require.Contains(t, string(plainRaw), `"HostDiskProvider":false`)

	optionsNoProvider := SessionAgentCall{
		SessionID:   "json-diskprovider-roundtrip-options-no-provider",
		Prompt:      "hello",
		CallOptions: &CallOptions{},
	}
	optionsNoProviderData := ToSessionAgentCallData(optionsNoProvider)
	require.False(t, optionsNoProviderData.HostDiskProvider)
}
