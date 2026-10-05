package cmd

// Grouping and rendering of the `sessions cost` table. No database, no App:
// these are the facts the command hands over, so a regression here is a
// regression in the arithmetic or the COST wording, never in the plumbing.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildCostGroups_SumsAndCountsUnpriced: unpriced sessions must be summed
// like any other row (their tokens are real) AND counted, so the TOTAL can
// say how many rows it could not price. Dropping either half is the bug.
func TestBuildCostGroups_SumsAndCountsUnpriced(t *testing.T) {
	t.Parallel()

	facts := []costSessionFact{
		{Key: "a", Tokens: 100, Cost: 0.01, Priced: true},
		{Key: "b", Tokens: 50, Cost: 0, Priced: false},
		{Key: "a", Tokens: 200, Cost: 0.02, Priced: true},
	}
	rows, total := buildCostGroups(facts, func(a, b costGroup) bool { return a.CostUSD > b.CostUSD })

	require.Len(t, rows, 2)
	require.Equal(t, "a", rows[0].Key, "highest cost first")
	require.Equal(t, "b", rows[1].Key)

	require.Equal(t, 2, rows[0].Sessions)
	require.Equal(t, int64(300), rows[0].Tokens)
	require.InDelta(t, 0.03, rows[0].CostUSD, 1e-9)
	require.Equal(t, 0, rows[0].Unpriced)

	require.Equal(t, 1, rows[1].Sessions)
	require.Equal(t, int64(50), rows[1].Tokens)
	require.Equal(t, 1, rows[1].Unpriced, "the unpriced session must be counted in its group")

	require.Equal(t, 3, total.Sessions)
	require.Equal(t, int64(350), total.Tokens)
	require.InDelta(t, 0.03, total.CostUSD, 1e-9)
	require.Equal(t, 1, total.Unpriced)
}

// TestBuildCostGroups_StableWithinEqualCost: the old table sorted by cost with
// sort.Slice, which reordered equal-cost rows arbitrarily. Rows that cost the
// same must keep first-seen order or the table shuffles between runs.
func TestBuildCostGroups_StableWithinEqualCost(t *testing.T) {
	t.Parallel()

	facts := []costSessionFact{
		{Key: "first", Tokens: 1, Cost: 0.5, Priced: true},
		{Key: "second", Tokens: 1, Cost: 0.5, Priced: true},
		{Key: "third", Tokens: 1, Cost: 0.5, Priced: true},
	}
	rows, _ := buildCostGroups(facts, func(a, b costGroup) bool { return a.CostUSD > b.CostUSD })

	require.Equal(t, []string{"first", "second", "third"},
		[]string{rows[0].Key, rows[1].Key, rows[2].Key})
}

// TestBuildCostGroups_NilOrderKeepsFirstSeen: nil order is the identity, not
// an empty table.
func TestBuildCostGroups_NilOrderKeepsFirstSeen(t *testing.T) {
	t.Parallel()

	facts := []costSessionFact{
		{Key: "z", Priced: true},
		{Key: "a", Priced: true},
		{Key: "z", Priced: true},
	}
	rows, total := buildCostGroups(facts, nil)
	require.Len(t, rows, 2)
	require.Equal(t, "z", rows[0].Key)
	require.Equal(t, "a", rows[1].Key)
	require.Equal(t, 3, total.Sessions)
}

// TestBuildCostGroups_NoUnpricedSuffix: the "+ N unpriced" marker must be
// absent when every session had a price, so the common case reads unchanged.
func TestBuildCostGroups_NoUnpricedSuffix(t *testing.T) {
	t.Parallel()

	priced, _ := buildCostGroups([]costSessionFact{
		{Key: "a", Priced: true},
		{Key: "b", Priced: true},
	}, nil)
	require.Len(t, priced, 2)
	require.Empty(t, costTotalSuffix(costTotals{}))
	require.Empty(t, costTotalSuffix(costTotals{Sessions: 2, Tokens: 9, CostUSD: 1.5}))

	unpriced, total := buildCostGroups([]costSessionFact{
		{Key: "a", Priced: true},
		{Key: "b", Priced: false},
	}, nil)
	require.Equal(t, 1, unpriced[1].Unpriced)
	require.Equal(t, 1, total.Unpriced)
	require.Contains(t, costTotalSuffix(total), "(+ 1 unpriced)")
}

// TestWriteCostTable_UnpricedRowPrintsNoPrice: an unpriced row must not claim
// $0.000 -- that is the number an operator would read as "free" and leave
// alone.
func TestWriteCostTable_UnpricedRowPrintsNoPrice(t *testing.T) {
	t.Parallel()

	rows := []costGroup{
		{Key: "glm-4.6", Sessions: 2, Tokens: 30000, CostUSD: 0.030, Priced: true},
		{Key: "some-local-model", Sessions: 1, Tokens: 100, CostUSD: 0, Priced: false},
	}
	total := costTotals{Sessions: 3, Tokens: 30100, CostUSD: 0.030, Unpriced: 1}

	var buf bytes.Buffer
	require.NoError(t, writeCostTable(&buf, "MODEL\tSESSIONS\tTOKENS\tCOST", rows, total))

	out := buf.String()
	require.Contains(t, out, "n/a (no price)")
	require.Contains(t, out, "$0.030")
	require.Contains(t, out, "30,000")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "TOTAL") {
			require.Contains(t, line, "+ 1 unpriced")
		}
	}
}

// TestWriteCostTable_PricedZeroIsStillADollar: a model with an explicit zero
// rate IS free, and its row must still print $0.000 -- the reverse mistake of
// dropping every zero.
func TestWriteCostTable_PricedZeroIsStillADollar(t *testing.T) {
	t.Parallel()

	rows := []costGroup{{Key: "ollama-local", Sessions: 1, Tokens: 10, CostUSD: 0, Priced: true}}
	total := costTotals{Sessions: 1, Tokens: 10, CostUSD: 0}

	var buf bytes.Buffer
	require.NoError(t, writeCostTable(&buf, "MODEL\tSESSIONS\tTOKENS\tCOST", rows, total))

	out := buf.String()
	require.Contains(t, out, "$0.000")
	require.NotContains(t, out, costNoPriceText)
	require.NotContains(t, out, "unpriced")
}

// TestBuildCostGroups_MixedPricedGroupIsUnpriced: a group mixing priced and
// unpriced sessions must be unpriced, whatever order the facts arrive in.
// REVERT CHECK: restoring `g.Priced = f.Priced` (last member wins) turns
// both orders red.
func TestBuildCostGroups_MixedPricedGroupIsUnpriced(t *testing.T) {
	t.Parallel()

	unpriced := costSessionFact{Key: "2026-01-02", Tokens: 100, Cost: 0, Priced: false}
	priced := costSessionFact{Key: "2026-01-02", Tokens: 200, Cost: 0, Priced: true}
	orders := [][]costSessionFact{
		{unpriced, priced},
		{priced, unpriced},
	}

	for _, facts := range orders {
		rows, total := buildCostGroups(facts, nil)
		require.Len(t, rows, 1)
		require.False(t, rows[0].Priced)
		require.Equal(t, 1, rows[0].Unpriced)
		require.Equal(t, 2, rows[0].Sessions)
		require.Equal(t, int64(300), rows[0].Tokens)
		require.Equal(t, 1, total.Unpriced)

		var buf bytes.Buffer
		require.NoError(t, writeCostTable(&buf, "DATE\tSESSIONS\tTOKENS\tCOST", rows, total))
		require.Contains(t, buf.String(), "n/a (no price)")
		require.NotContains(t, buf.String(), "$0.000")
	}
}

// TestBuildCostGroups_AllPricedGroupStaysPriced: the control case -- a group
// whose every member is priced keeps its $0.000.
func TestBuildCostGroups_AllPricedGroupStaysPriced(t *testing.T) {
	t.Parallel()

	rows, _ := buildCostGroups([]costSessionFact{
		{Key: "2026-01-02", Tokens: 100, Cost: 0, Priced: true},
		{Key: "2026-01-02", Tokens: 200, Cost: 0, Priced: true},
	}, nil)
	require.Len(t, rows, 1)
	require.True(t, rows[0].Priced)
	require.Equal(t, 0, rows[0].Unpriced)

	var buf bytes.Buffer
	require.NoError(t, writeCostTable(&buf, "DATE\tSESSIONS\tTOKENS\tCOST", rows, costTotals{Sessions: 2}))
	require.Contains(t, buf.String(), "$0.000")
}
