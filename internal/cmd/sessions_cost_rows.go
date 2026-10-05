package cmd

// Row computation and rendering for `sessions cost`, split from the command
// in sessions_cost.go so it can be exercised without a database: the command
// resolves models and budgets, this only adds up facts.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// costGroup is one row of a cost table. Priced is false when no price is
// configured for the group's model, which is what makes the COST cell print
// costNoPriceText instead of a confident $0.000.
type costGroup struct {
	Key      string
	Sessions int
	Tokens   int64
	CostUSD  float64
	Priced   bool
	// Unpriced counts the sessions in this group whose model has no price.
	Unpriced int
}

// costTotals is the TOTAL row.
type costTotals struct {
	Sessions int
	Tokens   int64
	CostUSD  float64
	// Unpriced counts sessions whose model has no price configured.
	Unpriced int
}

// costSessionFact is one session's resolved contribution to the table.
type costSessionFact struct {
	Key    string
	Tokens int64
	Cost   float64
	Priced bool
}

// buildCostGroups sums facts per Key. order selects the row order; nil keeps
// first-seen order. Sessions whose model has no price are counted per group
// and in total rather than being dropped: their tokens are still real.
func buildCostGroups(facts []costSessionFact, order func(a, b costGroup) bool) ([]costGroup, costTotals) {
	groups := make(map[string]*costGroup)
	var keys []string
	var total costTotals
	for _, f := range facts {
		g, ok := groups[f.Key]
		if !ok {
			g = &costGroup{Key: f.Key, Priced: true}
			groups[f.Key] = g
			keys = append(keys, f.Key)
		}
		g.Sessions++
		g.Tokens += f.Tokens
		g.CostUSD += f.Cost
		// A group is priced only when every member is.
		g.Priced = g.Priced && f.Priced
		if !f.Priced {
			g.Unpriced++
			total.Unpriced++
		}
		total.Sessions++
		total.Tokens += f.Tokens
		total.CostUSD += f.Cost
	}
	rows := make([]costGroup, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, *groups[k])
	}
	if order != nil {
		sort.SliceStable(rows, func(i, j int) bool { return order(rows[i], rows[j]) })
	}
	return rows, total
}

// costTotalSuffix is the "+ N unpriced" marker appended to the TOTAL row.
// Empty when every session had a price, so the common case reads unchanged.
func costTotalSuffix(total costTotals) string {
	if total.Unpriced == 0 {
		return ""
	}
	return fmt.Sprintf("  (+ %d unpriced)", total.Unpriced)
}

// writeCostTable renders header/rows/separator/TOTAL. The COST column goes
// through costCellText, so an unpriced row never claims $0.000.
func writeCostTable(w io.Writer, header string, rows []costGroup, total costTotals) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
			row.Key, row.Sessions, formatInt64(row.Tokens), costCellText(row.CostUSD, row.Priced))
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", strings.Repeat("─", 53), "", "", "")
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t%s%s\n",
		total.Sessions, formatInt64(total.Tokens), costCellText(total.CostUSD, total.Unpriced == 0),
		costTotalSuffix(total))
	return tw.Flush()
}
