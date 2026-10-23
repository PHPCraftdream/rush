package agent

import (
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func testPolicyContext() batchLeafPolicyContext {
	return batchLeafPolicyContext{
		RegistryID:        "reg-1",
		MessageID:         "msg-1",
		ModelName:         "test-model",
		RestrictedToolset: "toolset-1",
		RunAllowlist:      &permission.RunAllowlistSpec{Restrict: true},
	}
}

func testInvocation() BatchInvocation {
	return BatchInvocation{
		SessionID:        "sess-1",
		MessageID:        "msg-1",
		RegistryID:       "reg-1",
		Origin:           "test",
		Delivery:         batchDeliveryChoice{Mode: batchDeliverySync},
		BackgroundEscape: batchEscapeNever,
		WaiterRank:       0,
		Policy:           testPolicyContext(),
	}
}

// TestBatchInvocationValidateProviderMarkersExecutorEnforced pins Validate as
// a well-formedness gate only: provider/custom-disk markers without a binding
// id are admitted here, and enforcement happens later — the executor rejects a
// provider-marked payload without a binding id (batch_exec.go), and the batch
// install ingress rejects DiskProvider=true with an empty binding id
// (batch_install_ingress.go).
func TestBatchInvocationValidateProviderMarkersExecutorEnforced(t *testing.T) {
	t.Parallel()
	t.Run("provider marker without binding", func(t *testing.T) {
		t.Parallel()
		inv := testInvocation()
		inv.Policy.DiskProvider = true
		// Admission is not the enforcement point: Validate checks
		// well-formedness only (batch_invocation.go); the executor's
		// provider/binding check (batch_exec.go) owns this refusal.
		require.NoError(t, inv.Validate())
	})
	t.Run("custom disk without binding", func(t *testing.T) {
		t.Parallel()
		inv := testInvocation()
		inv.Policy.CustomDisk = " disk "
		// Admission is not the enforcement point: Validate checks
		// well-formedness only (batch_invocation.go); the executor's
		// provider/binding check (batch_exec.go) owns this refusal.
		require.NoError(t, inv.Validate())
	})
	t.Run("malformed binding id", func(t *testing.T) {
		t.Parallel()
		inv := testInvocation()
		inv.Policy.DiskBindingID = "bad id"
		require.ErrorIs(t, inv.Validate(), errBatchBadDiskBinding)
	})
	t.Run("well-formed binding id accepted", func(t *testing.T) {
		t.Parallel()
		inv := testInvocation()
		inv.Policy.DiskBindingID = "bind-1"
		require.NoError(t, inv.Validate())
	})
}

func TestBatchInvocationValidateAcceptsComplete(t *testing.T) {
	t.Parallel()
	inv := testInvocation()
	require.NoError(t, inv.Validate())
}

func TestBatchInvocationValidateFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mut   func(*BatchInvocation)
		users error
	}{
		{"missing session", func(i *BatchInvocation) { i.SessionID = "" }, errBatchMissingSessionID},
		{"blank session", func(i *BatchInvocation) { i.SessionID = "   " }, errBatchMissingSessionID},
		{"missing message", func(i *BatchInvocation) { i.MessageID = "" }, errBatchMissingMessageID},
		{"missing registry", func(i *BatchInvocation) { i.RegistryID = "" }, errBatchMissingRegistryID},
		{"missing origin", func(i *BatchInvocation) { i.Origin = "" }, errBatchMissingOrigin},
		{"missing policy registry", func(i *BatchInvocation) { i.Policy.RegistryID = "" }, errBatchMissingPolicy},
		{"missing restricted toolset", func(i *BatchInvocation) { i.Policy.RestrictedToolset = "" }, errBatchMissingPolicy},
		{"blank restricted toolset", func(i *BatchInvocation) { i.Policy.RestrictedToolset = " " }, errBatchMissingPolicy},
		{"bad delivery mode", func(i *BatchInvocation) { i.Delivery.Mode = "eventually" }, errBatchBadDeliveryMode},
		{"bad escape policy", func(i *BatchInvocation) { i.BackgroundEscape = "whenever" }, errBatchBadEscapePolicy},
		{"bad waiter rank", func(i *BatchInvocation) { i.WaiterRank = 7 }, errBatchBadWaiterRank},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inv := testInvocation()
			tc.mut(&inv)
			err := inv.Validate()
			require.Error(t, err)
			require.True(t, errors.Is(err, tc.users), "want %v, got %v", tc.users, err)
		})
	}
}

func TestBatchInvocationValidateDistinctErrors(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		errBatchMissingSessionID, errBatchMissingMessageID, errBatchMissingRegistryID,
		errBatchMissingOrigin, errBatchMissingPolicy, errBatchBadDeliveryMode,
		errBatchBadEscapePolicy, errBatchBadWaiterRank,
	}
	seen := make(map[string]bool, len(sentinels))
	for _, err := range sentinels {
		require.False(t, seen[err.Error()], "duplicate sentinel text: %v", err)
		seen[err.Error()] = true
	}
}

func TestBatchInvocationValidateCanonicalisesOnce(t *testing.T) {
	t.Parallel()
	inv := testInvocation()
	inv.SessionID = "  sess-1 "
	inv.Policy.ModelName = " model "
	require.NoError(t, inv.Validate())
	require.Equal(t, "sess-1", inv.SessionID)
	require.Equal(t, "model", inv.Policy.ModelName)
	// Defaults are filled in, canonicalised.
	require.Equal(t, batchDeliverySync, inv.Delivery.Mode)
	require.Equal(t, batchEscapeNever, inv.BackgroundEscape)
}

func TestBatchInvocationValidateDefaults(t *testing.T) {
	t.Parallel()
	inv := testInvocation()
	inv.Delivery.Mode = ""
	inv.BackgroundEscape = ""
	require.NoError(t, inv.Validate())
	require.Equal(t, batchDeliverySync, inv.Delivery.Mode)
	require.Equal(t, batchEscapeNever, inv.BackgroundEscape)
}

func TestBatchCanonicalInputRejectsEmptyAndInvalid(t *testing.T) {
	t.Parallel()
	_, err := batchCanonicalInput("")
	require.ErrorIs(t, err, errBatchNilInput)
	_, err = batchCanonicalInput("   ")
	require.ErrorIs(t, err, errBatchNilInput)
	_, err = batchCanonicalInput("{nope")
	require.Error(t, err)
	raw, err := batchCanonicalInput(`{ "a" : 1 }`)
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1}`, string(raw))
}

func TestBatchCallIdentityAndHash(t *testing.T) {
	t.Parallel()
	inv := testInvocation()
	specA, err := batchOneItemSpec(inv, newTestCall("call-1", `{"a":1}`), mustCanonical(t, `{"a":1}`))
	require.NoError(t, err)
	specB, err := batchOneItemSpec(inv, newTestCall("call-1", `{"a":2}`), mustCanonical(t, `{"a":2}`))
	require.NoError(t, err)
	hashA := batchInvocationHash(specA)
	require.Len(t, hashA, 64)
	require.NotEqual(t, hashA, batchInvocationHash(specB))
	// The same logical retry yields the same hash.
	specA2, err := batchOneItemSpec(inv, newTestCall("call-1", `{"a":1}`), mustCanonical(t, `{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, hashA, batchInvocationHash(specA2))
}
