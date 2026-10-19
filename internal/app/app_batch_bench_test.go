// D11 coordinator turn benchmark. Gated so default tests always skip it.
package app

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestD11BatchRuntimeBenchmark(t *testing.T) {
	if os.Getenv("RUSH_D11_BENCH") != "1" {
		t.Skip("D11 explicitly gated")
	}
	mode := os.Getenv("RUSH_D11_MODE")
	require.Contains(t, []string{"baseline", "candidate"}, mode)
	workload := os.Getenv("RUSH_D11_WORKLOAD")
	require.Contains(t, []string{"direct-view", "legacy-bash"}, workload)
	e := newD11Env(t, mode, workload)
	// Flag-ON health preconditions must hold before any measurement.
	require.Equal(t, mode == "candidate", batchCutoverOn())
	if mode == "candidate" {
		require.NotNil(t, e.app.batchCutoverWriter())
	}
	journal, syncMode := d11Durability(t, e.app.DB())
	runs := []d11RunEntry{}
	var measuredMu sync.Mutex
	var measuredIDs []d11ToolIdentity
	entry := func(name string, res d11PhaseResult, cps float64) d11RunEntry {
		return d11RunEntry{Workload: workload, Phase: name, Samples: len(res.Values), Attempts: res.Attempts, Errors: res.Errors, P50Ms: percentile(res.Values, 50), P95Ms: percentile(res.Values, 95), P99Ms: percentile(res.Values, 99), CallsPerSec: cps, Commits: res.Commits, SubmitWrites: res.Submit, StartedWrites: res.Started, SettledWrites: res.Settled, RawMs: res.Values}
	}
	seq, conc := []float64{}, []float64{}
	seqErr, concErr, warmErr := 0, 0, 0
	totalWrites := int64(0)
	submit, started, settled := 0, 0, 0
	throughput := float64(0)
	for round := 1; round <= d11MeasuredRounds; round++ {
		seqSessions := e.newSessions(fmt.Sprintf("r%d-seq", round), d11SeqBatchSessions)
		concSessions := e.newSessions(fmt.Sprintf("r%d-conc", round), d11ThroughputSessions)
		warm := e.runPhase(fmt.Sprintf("warmup-seq-%d", round), seqSessions, d11WarmupCalls/d11SeqBatchSessions, false, false)
		runs = append(runs, entry(fmt.Sprintf("warmup-seq-%d", round), warm, 0))
		if round == 1 {
			// Pre-measurement health: the warm turns exercised the per-turn
			// facts read, so any fail-closed facts path or refused turn is
			// already on the record.
			require.Zero(t, e.health.count(), "health violations: %v", e.health.snapshot())
		}
		seqPhase := e.runPhase(fmt.Sprintf("sequential-%d", round), seqSessions, d11LatencySamples/d11SeqBatchSessions, false, true)
		measuredMu.Lock()
		measuredIDs = append(measuredIDs, seqPhase.Identities...)
		measuredMu.Unlock()
		seq = append(seq, seqPhase.Values...)
		seqErr += seqPhase.Errors
		totalWrites += seqPhase.Commits
		submit += seqPhase.Submit
		started += seqPhase.Started
		settled += seqPhase.Settled
		runs = append(runs, entry(fmt.Sprintf("sequential-%d", round), seqPhase, 0))
		warmConc := e.runPhase(fmt.Sprintf("warmup-conc-%d", round), concSessions, d11ConcSessionWarmupCalls, true, false)
		runs = append(runs, entry(fmt.Sprintf("warmup-conc-%d", round), warmConc, 0))
		concPhase := e.runPhase(fmt.Sprintf("contention-%d", round), concSessions, d11ThroughputCallsPerSession, true, true)
		measuredMu.Lock()
		measuredIDs = append(measuredIDs, concPhase.Identities...)
		measuredMu.Unlock()
		conc = append(conc, concPhase.Values...)
		concErr += concPhase.Errors
		totalWrites += concPhase.Commits
		submit += concPhase.Submit
		started += concPhase.Started
		settled += concPhase.Settled
		cps := float64(concPhase.Attempts) / concPhase.Elapsed.Seconds()
		throughput += cps
		runs = append(runs, entry(fmt.Sprintf("contention-%d", round), concPhase, cps))
		d11LogRoundState(t, e, round)
		warmErr += warm.Errors + warmConc.Errors
		require.Equal(t, warm.Attempts, len(warm.Values))
		require.Equal(t, warmConc.Attempts, len(warmConc.Values))
	}
	throughput /= d11MeasuredRounds
	s := d11Summary{Mode: mode, Workload: workload, CandidateSHA: os.Getenv("RUSH_D11_CANDIDATE_SHA"), BaselineSHA: os.Getenv("RUSH_D11_BASELINE_SHA"), P50Ms: percentile(seq, 50), P95Ms: percentile(seq, 95), P99Ms: percentile(seq, 99), P95ContentionMs: percentile(conc, 95), ThroughputCPS: throughput, Errors: seqErr + concErr, Commits: totalWrites, SequentialAttempts: d11MeasuredRounds * d11LatencySamples, ContentionAttempts: d11MeasuredRounds * d11ThroughputSessions * d11ThroughputCallsPerSession, SubmitWrites: submit, StartedWrites: started, SettledWrites: settled, HealthViolations: e.health.count(), OffWaitPath: d11OffWaitPath, Runs: runs, MeasuredRounds: d11MeasuredRounds, SequentialSessions: d11SeqBatchSessions, LatencySamplesPerRound: d11LatencySamples, ContentionSessions: d11ThroughputSessions, ContentionCallsPerSession: d11ThroughputCallsPerSession, WarmupCallsPerRound: d11WarmupCalls, ConcSessionWarmupCalls: d11ConcSessionWarmupCalls}
	if s.SequentialAttempts+s.ContentionAttempts > 0 {
		s.CommitsPerCall = totalWrites / int64(s.SequentialAttempts+s.ContentionAttempts)
	}
	s.Durability.JournalMode = journal
	s.Durability.Synchronous = syncMode
	measuredMu.Lock()
	ids := append([]d11ToolIdentity(nil), measuredIDs...)
	measuredMu.Unlock()
	measuredIDsOut := make([]string, len(ids))
	for index, identity := range ids {
		measuredIDsOut[index] = identity.call
	}
	s.MeasuredCallIDs = measuredIDsOut
	if mode == "candidate" {
		evidence, evidenceErr := e.transitions.validateCalls(ids)
		s.CallTransactions = evidence
		d11PrintSummary(t, s)
		require.NoError(t, evidenceErr)
	} else {
		d11PrintSummary(t, s)
	}
	if mode == "candidate" {
		expected := s.SequentialAttempts + s.ContentionAttempts
		require.Equal(t, expected, submit)
		require.Equal(t, expected, started)
		require.Equal(t, expected, settled)
	}
	require.Zero(t, warmErr)
	require.Zero(t, s.Errors)
	require.Equal(t, "wal", strings.ToLower(journal))
	require.Contains(t, []string{"NORMAL", "FULL", "EXTRA"}, strings.ToUpper(syncMode))
	require.Zero(t, e.health.count(), "health violations: %v", e.health.snapshot())
}
