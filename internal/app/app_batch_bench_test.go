// D11 per-call-cost benchmark (R12-G CI evidence package). Measures, on ONE
// runner in ONE run, the ADDED p95 latency of an ordinary sequential
// work-tool call and the aggregate throughput of 8 concurrent sessions
// sharing ONE SQLite database (writer contention), OLD path (batch runtime
// effectively off; inline leaf execution) versus NEW path (RUSH_BATCH_RUNTIME
// on; full Submit/Started/Settled persistence and elected delivery per call),
// alternating warmup + 3 measured rounds per path over two fresh, equivalent,
// populated databases. Gated behind RUSH_D11_BENCH=1 — the shadow workflow
// (.github/workflows/job-batches-shadow.yml, job d11-benchmark) runs it
// explicitly; it NEVER runs in the default suite and is intentionally NOT
// skipped under -short.
package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestD11BatchRuntimeBenchmark is the D11 gate. Pass criteria (asserted
// AFTER the summary is printed, so a breach still produces a report):
// p95 delta <= 15ms, throughput ratio >= 0.80, zero errors on both paths,
// commits_on >= commits_off, and WAL + non-OFF synchronous durability.
func TestD11BatchRuntimeBenchmark(t *testing.T) {
	if os.Getenv("RUSH_D11_BENCH") != "1" {
		t.Skip("D11 benchmark is expensive and contention-sensitive; run explicitly with RUSH_D11_BENCH=1 (see .github/workflows/job-batches-shadow.yml, job d11-benchmark)")
	}

	// Two fresh, equivalent, populated databases: one flag-off app, one
	// flag-on app, both through the REAL New entry point with the same
	// isolated env and probe provider (see newFullNewApp).
	off := newD11BenchEnv(t, false)
	on := newD11BenchEnv(t, true)

	journalMode, syncMode := d11Durability(t, on.app.DB())
	offJournal, offSync := d11Durability(t, off.app.DB())
	require.Equal(t, journalMode, offJournal, "both paths must share journal mode")
	require.Equal(t, syncMode, offSync, "both paths must share synchronous level")

	ctx := context.Background()
	var runs []d11RunEntry
	var p95OffSamples, p95OnSamples []float64
	var cpsOffSum, cpsOnSum float64
	var errorsOff, errorsOn, commitsOff, commitsOn int

	// Warmup round per path (not counted): pages the DB, engine and pools.
	for _, env := range []*d11BenchEnv{off, on} {
		// The schema's phase enum is warmup|off|on.
		phase := "warmup"
		entry, _ := env.d11RunPhase(ctx, phase, true)
		// Warmup never reports samples, commits or rates.
		entry.Samples = 0
		entry.Commits = 0
		entry.P50Ms, entry.P95Ms, entry.CallsPerSec = 0, 0, 0
		runs = append(runs, entry)
	}

	// Measured rounds alternate OFF, ON, OFF, ON, ...
	for round := 0; round < d11MeasuredRounds; round++ {
		for _, env := range []*d11BenchEnv{off, on} {
			phase := fmt.Sprintf("off-%d", round+1)
			if env.on {
				phase = fmt.Sprintf("on-%d", round+1)
			}
			entry, samples := env.d11RunPhase(ctx, phase, false)
			runs = append(runs, entry)
			if env.on {
				p95OnSamples = append(p95OnSamples, samples...)
				cpsOnSum += entry.CallsPerSec
				errorsOn += entry.Errors
				commitsOn += entry.Commits
			} else {
				p95OffSamples = append(p95OffSamples, samples...)
				cpsOffSum += entry.CallsPerSec
				errorsOff += entry.Errors
				commitsOff += entry.Commits
			}
		}
	}

	p95Off := percentile(p95OffSamples, 95)
	p95On := percentile(p95OnSamples, 95)
	cpsOff := cpsOffSum / d11MeasuredRounds
	cpsOn := cpsOnSum / d11MeasuredRounds
	ratio := 0.0
	if cpsOff > 0 {
		ratio = cpsOn / cpsOff
	}

	summary := d11Summary{
		P95OffMs: p95Off, P95OnMs: p95On, P95DeltaMs: p95On - p95Off,
		P95BudgetMs:      d11P95BudgetMs,
		ThroughputOffCPS: cpsOff, ThroughputOnCPS: cpsOn, ThroughputRatio: ratio,
		ErrorsOff: errorsOff, ErrorsOn: errorsOn,
		CommitsOff: commitsOff, CommitsOn: commitsOn,
		Runs: runs,
	}
	summary.Durability.JournalMode = journalMode
	summary.Durability.Synchronous = syncMode
	d11PrintSummary(t, summary)

	// PASS gates — evaluated strictly after the evidence block is printed.
	require.LessOrEqual(t, summary.P95DeltaMs, d11P95BudgetMs,
		"added p95 latency of the batch-runtime work-tool call exceeds the budget")
	require.GreaterOrEqual(t, ratio, d11ThroughputFloor,
		"throughput ON/OFF ratio below the contention floor")
	require.Zero(t, errorsOff, "errors on the OFF path")
	require.Zero(t, errorsOn, "errors on the ON path")
	require.GreaterOrEqual(t, commitsOn, commitsOff,
		"the ON path must settle at least as many commits as OFF")
	require.Equal(t, "wal", strings.ToLower(journalMode),
		"journal_mode must be WAL (durability never disabled)")
	require.NotEqual(t, "OFF", strings.ToUpper(syncMode),
		"synchronous must never be OFF (durability never disabled)")
}
