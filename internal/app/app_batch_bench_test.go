// D11 coordinator turn benchmark. Gated so default tests always skip it.
package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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
	journal, syncMode := d11Durability(t, e.app.DB())
	runs := []d11RunEntry{}
	var measuredMu sync.Mutex
	var measuredIDs []d11ToolIdentity
	phase := func(label string, n, slot int, measured bool) ([]float64, int, int64, int, int, int) {
		before, sub, started, settled := e.writeSnapshot()
		values := make([]float64, 0, n)
		errs := 0
		for i := 0; i < n; i++ {
			requestID := fmt.Sprintf("%s-%d", label, i)
			ms, err := e.call(context.Background(), slot, requestID)
			if measured {
				if identity, ok := e.toolIdentity(requestID); ok {
					measuredMu.Lock()
					measuredIDs = append(measuredIDs, identity)
					measuredMu.Unlock()
				} else {
					measuredMu.Lock()
					measuredIDs = append(measuredIDs, d11ToolIdentity{call: ""})
					measuredMu.Unlock()
				}
			}
			if err != nil {
				errs++
			} else {
				values = append(values, ms)
			}
		}
		after, asub, astarted, asettled := e.writeSnapshot()
		return values, errs, after - before, asub - sub, astarted - started, asettled - settled
	}
	warm, warmErr, warmWrites, warmSub, warmStarted, warmSettled := phase("warmup", d11WarmupCalls, 0, false)
	runs = append(runs, d11RunEntry{Workload: workload, Phase: "warmup", Samples: len(warm), Attempts: d11WarmupCalls, Errors: warmErr, P50Ms: percentile(warm, 50), P95Ms: percentile(warm, 95), P99Ms: percentile(warm, 99), Commits: warmWrites, SubmitWrites: warmSub, StartedWrites: warmStarted, SettledWrites: warmSettled, RawMs: warm})
	seq, conc := []float64{}, []float64{}
	seqErr, concErr := 0, 0
	totalWrites := int64(0)
	submit, started, settled := 0, 0, 0
	throughput := float64(0)
	for round := 0; round < d11MeasuredRounds; round++ {
		v, errs, w, su, st, se := phase(fmt.Sprintf("seq-%d", round), d11LatencySamples, 0, true)
		seq = append(seq, v...)
		seqErr += errs
		totalWrites += w
		submit += su
		started += st
		settled += se
		runs = append(runs, d11RunEntry{Workload: workload, Phase: fmt.Sprintf("sequential-%d", round+1), Samples: len(v), Attempts: d11LatencySamples, Errors: errs, P50Ms: percentile(v, 50), P95Ms: percentile(v, 95), P99Ms: percentile(v, 99), Commits: w, SubmitWrites: su, StartedWrites: st, SettledWrites: se, RawMs: v})
		var mu sync.Mutex
		var wg sync.WaitGroup
		block := []float64{}
		blockErr := 0
		before, bsub, bst, bse := e.writeSnapshot()
		start := time.Now()
		for slot := 1; slot <= d11ThroughputSessions; slot++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				own := []float64{}
				errs := 0
				for i := 0; i < d11ThroughputCallsPerSession; i++ {
					requestID := fmt.Sprintf("conc-%d-%d-%d", round, slot, i)
					ms, err := e.call(context.Background(), slot, requestID)
					if identity, ok := e.toolIdentity(requestID); ok {
						measuredMu.Lock()
						measuredIDs = append(measuredIDs, identity)
						measuredMu.Unlock()
					} else {
						measuredMu.Lock()
						measuredIDs = append(measuredIDs, d11ToolIdentity{call: ""})
						measuredMu.Unlock()
					}
					if err != nil {
						errs++
					} else {
						own = append(own, ms)
					}
				}
				mu.Lock()
				block = append(block, own...)
				blockErr += errs
				mu.Unlock()
			}(slot)
		}
		wg.Wait()
		after, asu, ast, ase := e.writeSnapshot()
		w = after - before
		su = asu - bsub
		st = ast - bst
		se = ase - bse
		totalWrites += w
		submit += su
		started += st
		settled += se
		conc = append(conc, block...)
		concErr += blockErr
		cps := float64(d11ThroughputSessions*d11ThroughputCallsPerSession) / time.Since(start).Seconds()
		throughput += cps
		runs = append(runs, d11RunEntry{Workload: workload, Phase: fmt.Sprintf("contention-%d", round+1), Samples: len(block), Attempts: d11ThroughputSessions * d11ThroughputCallsPerSession, Errors: blockErr, P50Ms: percentile(block, 50), P95Ms: percentile(block, 95), P99Ms: percentile(block, 99), CallsPerSec: cps, Commits: w, SubmitWrites: su, StartedWrites: st, SettledWrites: se, RawMs: block})
	}
	throughput /= d11MeasuredRounds
	s := d11Summary{Mode: mode, Workload: workload, CandidateSHA: os.Getenv("RUSH_D11_CANDIDATE_SHA"), BaselineSHA: os.Getenv("RUSH_D11_BASELINE_SHA"), P50Ms: percentile(seq, 50), P95Ms: percentile(seq, 95), P99Ms: percentile(seq, 99), P95ContentionMs: percentile(conc, 95), ThroughputCPS: throughput, Errors: seqErr + concErr, Commits: totalWrites, SequentialAttempts: d11MeasuredRounds * d11LatencySamples, ContentionAttempts: d11MeasuredRounds * d11ThroughputSessions * d11ThroughputCallsPerSession, SubmitWrites: submit, StartedWrites: started, SettledWrites: settled, Runs: runs}
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
	require.Equal(t, d11WarmupCalls, len(warm))
	require.Zero(t, s.Errors)
	require.Equal(t, "wal", strings.ToLower(journal))
	require.Contains(t, []string{"NORMAL", "FULL", "EXTRA"}, strings.ToUpper(syncMode))
}
