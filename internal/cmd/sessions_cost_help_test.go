package cmd

// Help text of `rush sessions cost` and of its parent. The command sells
// itself as able to tell "free" from "nobody priced it", so that contract has
// to be in the help an operator reads -- and in the list every subcommand has
// to appear in.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionsCostHelp_StatesThePricingContract: the help must name the four
// rate keys an operator has to write, say where they live, and state what the
// command prints when a model has none of them.
//
// Revert-check: with the Pricing block removed from Long, this test fails
// naming cost_per_1m_in, providers.<provider-id>.models and n/a (no price).
func TestSessionsCostHelp_StatesThePricingContract(t *testing.T) {
	t.Parallel()

	long := oneLine(sessionsCostCmd.Long)
	for _, key := range []string{
		"cost_per_1m_in",
		"cost_per_1m_out",
		"cost_per_1m_in_cached",
		"cost_per_1m_out_cached",
	} {
		require.Contains(t, long, key, "the rate keys must be in the help")
	}
	require.Contains(t, long, "n/a (no price)")
	require.Contains(t, long, "providers.<provider-id>.models")
	require.Contains(t, long, "unpriced")

	// A zero rate is the only way to say "free"; the help must say so, or an
	// unpriced model keeps printing n/a (no price) with no way to fix it.
	require.Contains(t, long, "genuinely free still has to say so with a zero rate")

	require.Equal(t, "cost", sessionsCostCmd.Use)

	for _, name := range []string{"since", "by", "json", "top"} {
		require.NotNilf(t, sessionsCostCmd.Flags().Lookup(name), "flag --%s is missing from the help", name)
	}

	// The model fallback has to be documented: it is the whole point of the
	// "(unknown)" group going away.
	require.Contains(t, long, "attributed to the model that produced most of its messages")
	require.NotContains(t, long, "group by SmartModelID")

	// The --json rows carry the flag, so the examples must show how to reach
	// it: an operator who cannot filter has to read every row by hand.
	require.Contains(t, sessionsCostCmd.Example, "priced")
	require.Contains(t, sessionsCostCmd.Example, "select(.priced == false)")
}

// TestSessionsParentHelp_ListsCost: the `sessions` root help lists every
// subcommand it has — one that is missing from it does not exist for the
// operator reading it.
func TestSessionsParentHelp_ListsCost(t *testing.T) {
	t.Parallel()

	observe := oneLine(sessionsCmd.Long)
	require.Contains(t, observe, "cost")

	var registered bool
	for _, sub := range sessionsCmd.Commands() {
		if sub.Name() == "cost" {
			registered = true
			require.Same(t, sessionsCostCmd, sub, "the registered command must be the one under test")
		}
	}
	require.True(t, registered, "`sessions cost` must be registered on the sessions command")
}
