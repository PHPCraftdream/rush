// D11 per-call-cost benchmark helpers (R12-G CI evidence package). This file
// holds the measurement scaffolding: tunable constants, the summary schema,
// the canned leaf/executor pair, and the hermetic per-path environment. The
// test itself lives in app_batch_bench_test.go.
//
// SEAM CHOICE (documented per the D11 spec): the timed NEW path drives the
// REAL batch runtime end to end — agent.InstallBatchRuntime over the App's
// own pooled SQLite writer (db.Connect, WAL/NORMAL), with the REAL jobbatch
// Runner engine, the REAL session batch commit hook (Submit/Started/Settled
// persistence into batch_nodes) and the REAL R11D delivery election
// (install.Delivery) reached through install.Runtime.Invoke — the same
// ingress entry point the coordinator's work tools use. MOCKED: (a) the leaf
// tool is a canned stub whose Run returns instantly, identical on both
// paths, so the measured delta is exactly the persistence/election overhead
// around the call; (b) the executor family is replaced by a minimal
// jobbatch.Executor that runs the canned leaf and reports Completed — the
// default generic family resolves leaves through the pinned policy-leaf
// registry (internal/agent/policy_leaf.go), whose type is not constructible
// outside internal/agent. This still satisfies "ordinary sequential
// work-tool call" for latency accounting: every Submit admission, commit-hook
// write and delivery election of the real path executes per call. The OLD
// (flag-off) path is the pre-batch behavior the coordinator falls back to:
// the work tool executes its leaf INLINE with no batch persistence —
// mirrored by a direct leaf.Run per call.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Tunable sizing constants. The whole benchmark must stay comfortably inside
// the shadow workflow's 40m -timeout on a CI runner; keep the products of
// these numbers under ~4000 timed calls total.
const (
	// d11WarmupCalls is the size of the one warmup round per path (not
	// counted in any reported statistic).
	d11WarmupCalls = 20
	// d11LatencySamples is the number of sequential per-call latency samples
	// per measured round and path.
	d11LatencySamples = 200
	// d11ThroughputSessions is the number of concurrent sessions sharing the
	// ONE database during the throughput phase (writer contention).
	d11ThroughputSessions = 8
	// d11ThroughputCallsPerSession is the sequential call count per session
	// in one throughput phase.
	d11ThroughputCallsPerSession = 50
	// d11MeasuredRounds is the number of measured rounds per path; rounds
	// alternate OFF, ON, OFF, ON, ...
	d11MeasuredRounds = 3
	// d11DonePoll tightens the delivery done-source poll cadence from its
	// 250ms fallback so per-call latency reflects persistence, not polling.
	d11DonePoll = 2 * time.Millisecond
	// d11P95BudgetMs is the PASS gate on the added p95 latency.
	d11P95BudgetMs = 15.0
	// d11ThroughputFloor is the PASS gate on throughput ON/OFF ratio.
	d11ThroughputFloor = 0.80
)

// d11Summary is the single-line JSON object printed between the
// D11-SUMMARY markers (parsed by tools/d11_report.py). Field order is the
// schema order; encoding/json marshals struct fields in declaration order.
type d11Summary struct {
	P95OffMs         float64 `json:"p95_off_ms"`
	P95OnMs          float64 `json:"p95_on_ms"`
	P95DeltaMs       float64 `json:"p95_delta_ms"`
	P95BudgetMs      float64 `json:"p95_budget_ms"`
	ThroughputOffCPS float64 `json:"throughput_off_cps"`
	ThroughputOnCPS  float64 `json:"throughput_on_cps"`
	ThroughputRatio  float64 `json:"throughput_ratio"`
	ErrorsOff        int     `json:"errors_off"`
	ErrorsOn         int     `json:"errors_on"`
	CommitsOff       int     `json:"commits_off"`
	CommitsOn        int     `json:"commits_on"`
	Durability       struct {
		JournalMode string `json:"journal_mode"`
		Synchronous string `json:"synchronous"`
	} `json:"durability"`
	Runs []d11RunEntry `json:"runs"`
}

// d11RunEntry is one reported round (warmup or measured, per path).
type d11RunEntry struct {
	Phase       string  `json:"phase"`
	Samples     int     `json:"samples"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	CallsPerSec float64 `json:"calls_per_sec"`
	Errors      int     `json:"errors"`
	Commits     int     `json:"commits"`
}

// d11BenchLeaf is the canned work-tool leaf: instant, identical on both
// paths. It mirrors the offline probe pattern of newFullNewApp's provider —
// a stub that satisfies the interface without doing real work.
type d11BenchLeaf struct {
	name string
	runs atomic.Int64
}

func (l *d11BenchLeaf) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: l.name} }

func (l *d11BenchLeaf) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.runs.Add(1)
	return fantasy.NewTextResponse("d11-ok"), nil
}

func (l *d11BenchLeaf) ProviderOptions() fantasy.ProviderOptions { return nil }

func (l *d11BenchLeaf) SetProviderOptions(fantasy.ProviderOptions) {}

// d11BenchExecutor is the minimal real Executor seam: it runs the canned
// leaf and reports Completed. It replaces the default generic family only
// because the pinned policy-leaf registry is not constructible outside
// internal/agent; Submit/Settle/delivery above it stay fully real.
type d11BenchExecutor struct {
	leaf fantasy.AgentTool
}

func (x *d11BenchExecutor) Start(
	ctx context.Context, launch jobbatch.Launch, report func(jobbatch.Result),
) (jobbatch.Execution, error) {
	call := fantasy.ToolCall{
		ID: string(launch.ID), Name: launch.Task.Kind, Input: string(launch.Task.Payload),
	}
	if _, err := x.leaf.Run(ctx, call); err != nil {
		report(jobbatch.Result{State: jobbatch.StateFailed, Code: "d11_leaf_error"})
	} else {
		report(jobbatch.Result{State: jobbatch.StateCompleted, Code: "d11_done"})
	}
	return d11NoopExecution{}, nil
}

// d11NoopExecution satisfies the Execution contract (Stop is idempotent per
// handle; the Runner owns the lifecycle).
type d11NoopExecution struct{}

func (d11NoopExecution) Stop(context.Context) error { return nil }

// d11BenchEnv is one hermetic per-path environment: a full App built through
// the REAL New entry point (newFullNewApp, flag-aware) plus, on the ON path,
// the real batch install driven over the SAME pooled database.
type d11BenchEnv struct {
	t          *testing.T
	on         bool
	app        *App
	conn       *sql.DB
	leaf       *d11BenchLeaf
	install    *agent.BatchInstall
	registryID string
}

// d11SetBatchRuntimeEnv sets RUSH_BATCH_RUNTIME for one App construction and
// restores the caller's value at cleanup. Plain os.Setenv + explicit restore
// is used instead of t.Setenv because the two constructions in one test need
// DIFFERENT values at different points mid-test; t.Setenv only restores at
// cleanup and would panic under t.Parallel (this test never goes parallel).
func d11SetBatchRuntimeEnv(t *testing.T, on bool) {
	t.Helper()
	prev, had := os.LookupEnv("RUSH_BATCH_RUNTIME")
	if on {
		require.NoError(t, os.Setenv("RUSH_BATCH_RUNTIME", "true"))
	} else {
		require.NoError(t, os.Unsetenv("RUSH_BATCH_RUNTIME"))
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("RUSH_BATCH_RUNTIME", prev)
		} else {
			_ = os.Unsetenv("RUSH_BATCH_RUNTIME")
		}
	})
}

// newD11BenchEnv builds one path. The OFF app is constructed with the flag
// unset and must expose NO batch runtime or ingress; the ON app is
// constructed with the flag on and must expose both. The measurement install
// (ON path only) shares the App's pooled SQLite writer through a second
// reference-counted db.Connect to the same data directory.
func newD11BenchEnv(t *testing.T, on bool) *d11BenchEnv {
	t.Helper()
	d11SetBatchRuntimeEnv(t, on)
	require.Equal(t, on, config.BatchRuntimeEnabled(),
		"flag parser must agree with the constructed path")
	app := newFullNewApp(t)
	if on {
		require.NotNil(t, app.batchRuntime, "flag on: runtime installed at New")
		require.NotNil(t, app.batchWorkIngress, "flag on: ingress seam installed")
	} else {
		require.Nil(t, app.batchRuntime, "flag off: no runtime installed")
		require.Nil(t, app.batchWorkIngress, "flag off: no ingress seam")
	}

	env := &d11BenchEnv{t: t, on: on, app: app, leaf: &d11BenchLeaf{name: "view"}}
	env.registryID = "d11-bench-registry"
	if !on {
		return env
	}
	ctx := context.Background()
	conn, err := db.Connect(ctx, app.dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(conn) })
	env.conn = conn
	// Populate: one real session row per benchmark session (owner FK).
	q := db.New(conn)
	for i := 0; i <= d11ThroughputSessions; i++ {
		id := env.sessionID(i)
		_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
		require.NoError(t, err, "seed session row")
	}
	profile, err := agent.NewBatchProfileDefault()
	require.NoError(t, err)
	install, err := agent.InstallBatchRuntime(ctx, agent.BatchInstallDeps{
		Host: "d11-host-" + uuid.NewString(), RunnerID: "d11-runner-" + uuid.NewString(),
		Profile: profile, Registries: agent.NewBatchLeafRegistryStore(),
		Store: session.NewBatchStore(conn), Messages: message.NewService(q),
		Executor:      &d11BenchExecutor{leaf: env.leaf},
		Limits:        jobbatch.Limits{MaxConcurrent: 16, MaxDepth: 8, MaxNodes: 1024},
		CommitTimeout: 5 * time.Second, ControlTimeout: 30 * time.Second,
		DonePoll: d11DonePoll,
	})
	require.NoError(t, err, "install the real batch runtime over the App DB")
	t.Cleanup(func() { _ = install.Runner.Close(ctx) })
	env.install = install
	return env
}

// sessionID maps a benchmark slot to its seeded session row.
func (e *d11BenchEnv) sessionID(slot int) string {
	return fmt.Sprintf("d11-sess-%s-%d", map[bool]string{true: "on", false: "off"}[e.on], slot)
}

// invokeNew drives ONE real work-tool call through the REAL ingress: attach
// probe, admission, Runner Submit (commit hook reserves the root), engine
// execution of the canned leaf, Settled persistence, and R11D delivery
// election. This is the exact seam the coordinator's work tools call.
func (e *d11BenchEnv) invokeNew(ctx context.Context, slot int, callID string) error {
	inv := agent.BatchInvocation{
		SessionID: e.sessionID(slot), MessageID: "d11-msg", RegistryID: e.registryID,
		Origin: "coordinator",
	}
	inv.Policy.RegistryID = e.registryID
	inv.Policy.RestrictedToolset = e.registryID
	call := fantasy.ToolCall{ID: callID, Name: "view", Input: `{"p":1}`}
	_, err := e.install.Runtime.Invoke(ctx, inv, e.leaf, call)
	return err
}

// invokeOld is the flag-off legacy behavior: the work tool executes its leaf
// inline with no batch persistence. The leaf is identical to the ON path's.
func (e *d11BenchEnv) invokeOld(ctx context.Context, callID string) error {
	call := fantasy.ToolCall{ID: callID, Name: "view", Input: `{"p":1}`}
	_, err := e.leaf.Run(ctx, call)
	return err
}

// d11Call runs one call of the path's mode and reports the wall time in ms.
func (e *d11BenchEnv) d11Call(ctx context.Context, slot int, callID string) (float64, error) {
	start := time.Now()
	var err error
	if e.on {
		err = e.invokeNew(ctx, slot, callID)
	} else {
		err = e.invokeOld(ctx, callID)
	}
	return float64(time.Since(start).Microseconds()) / 1000.0, err
}

// countSettledTasks counts completed Settled task projections. Each settled
// call leaves exactly one kind='task', state='completed' batch_nodes row
// (the commit hook's terminal transition), so the before/after delta of a
// round is the round's commit count.
func countSettledTasks(ctx context.Context, conn *sql.DB) int {
	var count int
	_ = conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM batch_nodes WHERE kind = 'task' AND state = 'completed'`,
	).Scan(&count)
	return count
}

// d11Durability reads the EFFECTIVE journal mode and synchronous level from
// the live connection (never set — only recorded and asserted; durability is
// never disabled). synchronous returns an integer pragma value.
func d11Durability(t *testing.T, conn *sql.DB) (string, string) {
	t.Helper()
	var mode string
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`PRAGMA journal_mode`).Scan(&mode))
	var sync int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`PRAGMA synchronous`).Scan(&sync))
	names := map[int]string{0: "OFF", 1: "NORMAL", 2: "FULL", 3: "EXTRA"}
	name, ok := names[sync]
	require.True(t, ok, "unknown synchronous pragma value %d", sync)
	return mode, name
}

// percentile computes the p-th percentile of ms samples by sort (small
// dependency-free helper; monotonic time.Now deltas only).
func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)-1) * p / 100.0)
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// d11RunPhase executes one round on one path: the latency samples (one
// sequential session) plus, on measured rounds, the 8-session
// shared-database throughput phase. Warmup rounds run only
// d11WarmupCalls sequential calls and are never reported. It returns the
// round's report entry and ALL latency samples. Every call carries full
// Submit/Settle persistence and delivery on the ON path.
func (e *d11BenchEnv) d11RunPhase(
	ctx context.Context, phase string, warmup bool,
) (d11RunEntry, []float64) {
	e.t.Helper()
	var (
		samples []float64
		errs    atomic.Int64
	)
	latencyCalls := d11LatencySamples
	if warmup {
		latencyCalls = d11WarmupCalls
	}
	for i := 0; i < latencyCalls; i++ {
		ms, err := e.d11Call(ctx, 0, fmt.Sprintf("lat-%s-%d", phase, i))
		if err != nil {
			errs.Add(1)
			continue
		}
		samples = append(samples, ms)
	}

	before := countSettledTasks(ctx, e.app.DB())
	start := time.Now()
	var wg sync.WaitGroup
	for slot := 1; !warmup && slot <= d11ThroughputSessions; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for i := 0; i < d11ThroughputCallsPerSession; i++ {
				_, err := e.d11Call(ctx, slot, fmt.Sprintf("thr-%s-%d-%d", phase, slot, i))
				if err != nil {
					errs.Add(1)
				}
			}
		}(slot)
	}
	wg.Wait()
	wall := time.Since(start)
	commits := countSettledTasks(ctx, e.app.DB()) - before
	totalCalls := d11ThroughputSessions * d11ThroughputCallsPerSession
	cps := float64(totalCalls) / wall.Seconds()
	entry := d11RunEntry{
		Phase: phase, Samples: len(samples),
		P50Ms: percentile(samples, 50), P95Ms: percentile(samples, 95),
		CallsPerSec: cps, Errors: int(errs.Load()), Commits: commits,
	}
	return entry, samples
}

// d11PrintSummary prints the single-line JSON evidence block. It runs BEFORE
// any numerical gate assertion so a breach still produces a report.
func d11PrintSummary(t *testing.T, s d11Summary) {
	t.Helper()
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	fmt.Printf("===D11-SUMMARY-BEGIN===\n%s\n===D11-SUMMARY-END===\n", raw)
}
