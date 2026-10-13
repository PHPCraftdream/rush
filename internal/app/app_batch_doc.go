package app

// Package R11E — dormant host lifecycle scaffolding (decomposition row E,
// docs/research/2026-10-12-r11-ingress-decomposition.md). This file is
// documentation only; it holds no code and compiles to nothing.
//
// Old -> new mapping (all anchors re-verified at f7d1bf27):
//
//   app_lifecycle.go releaseResources (stillBusy path) releases the host
//   identity via asyncJobStore.CloseKeepLock/Close and the writer via
//   db.ReleaseConn/Release on agent-idle — which is NOT proof of a drained
//   Runner, because no Runner exists in the lifecycle yet. R11F replaces
//   those two release sites with a single call to
//   BatchRuntime.DrainAndRelease, the drain gate in app_batch_drain.go,
//   which drains the Runner first and only then releases.
//
//   app_lifecycle.go cancelAgentsBeforeRelease stays authoritative for agent
//   cancellation; the gate runs after it (R11F wires the order).
//
//   D4 fatal exit (docs/plans/2026-10-12-job-batches-rush-integration.md,
//   decision D4; decomposition row E): app_batch_fault.go owns the
//   process-wide exit-70 handler for every host kind (CLI `rush run`,
//   long-lived server, SDK host — an SDK host also exits the process, not
//   only returns an error). R11F installs the handler alongside Runner
//   construction; today nothing calls it from production paths.
//
// Ownership transfer plan for R11F (files/lines are anchors at f7d1bf27):
//
//   1. app.go New: construct BatchRuntime with the R11B commit hook
//      (session.NewBatchCommitHook) and release funcs bound to
//      app.asyncJobStore / app.dbConns; store it in a new App field
//      (e.g. app.batchRuntime). No behavior change.
//   2. app_lifecycle.go cancelAgentsBeforeRelease: after agent
//      cancellation, call app.batchRuntime.Start (installs the fault
//      watcher) — or start it at first Runner set. Start and SetRunner are
//      order-independent: Start may be called while dormant, and SetRunner
//      wakes the dormant watcher. SetRunner is FIRST WINS: the first
//      Runner attaches and any later SetRunner with a different Runner is
//      refused. Fenced() does not depend on the watcher having run — it
//      also observes the Runner's closed Faulted channel directly.
//   3. app_lifecycle.go releaseResources: replace the asyncJobStore
//      CloseKeepLock/Close block and the db release block with
//      app.batchRuntime.DrainAndRelease(ctx, drainTimeout); the gate
//      drains the Runner, then releases host identity then writer.
//   4. Runner construction site (R11F, coordinator/ingress install):
//      call BatchRuntime.SetRunner so the narrow batchRunner interface
//      points at the live *jobbatch.Runner.
//
// Assumed engine contracts (verified against internal/jobbatch at
// f7d1bf27):
//
//   - Runner.Faulted() <-chan struct{} closes exactly once on the first
//     commit fault; FaultErr() returns the fault error thereafter
//     (runner_commit.go:26-31).
//   - Runner.Close(ctx) publishes closing intent exactly once, waits for
//     Final bounded by ctx, returns ErrCloseTimeout-wrapped
//     *CloseTimeoutError on timeout without fault, DisposeError for
//     dispose failures, or the fault error (runner_requests.go:350).
//   - On a fault the Runner contains all attempts and never publishes
//     failed transitions; the process is expected to exit (R4 fault
//     semantics, gate 3 of the decomposition doc).
//
// NOT-DONE (left for R11F or later packages):
//
//   - No production call sites: nothing in app.go/app_lifecycle.go
//     references BatchRuntime yet (R11F).
//   - No Runner construction, no commit-hook installation, no
//     RUSH_BATCH_RUNTIME flag (R11F; flag listed as non-goal here).
//   - No per-host-kind wiring: CLI/server/SDK all share the same handler;
//     only the CLI subprocess is exercised end-to-end in tests because
//     server/SDK hosts only exist after installation (R11F).
//   - No integration with R9 recovery/probe (BatchRecoverDeadHost,
//     BatchHostProbe) — the gate retains the lock on fault/timeout so the
//     existing recovery sweep keeps treating this host as alive.
//   - No metrics, no configurable exit code (70 is fixed by D4).
