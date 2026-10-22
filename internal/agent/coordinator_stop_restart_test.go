// TestSweepDeadHosts_RestartAfterStopAddsNoExtraFacts (DUR-9's restart half)
// was DELETED as an obsolete contract: its mechanism was the legacy ledger --
// ledger.Start/acknowledged delegation and bash jobs, AsyncJobStore.Get
// readback, SimulateCrashForTest and AsyncJobStore.SweepDeadHosts -- all gone
// in the batch world, where a restart's dead-host sweep is
// BatchStore.BatchSweepDeadHosts and a running root is ended by the batch
// runtime runner, not by the coordinator's Stop alone (a bare-fixture
// stopTree cannot project running roots terminal: stopBatchOwners needs the
// bound runtime runner).
//
// The retained halves are pinned where each mechanism actually lives:
//   - the transitive Stop (tree walk, wake zeroing, suspension):
//     coordinator_batch_stop_test.go, coordinator_batch_stop_running_test.go,
//     coordinator_batch_stop_terminal_test.go and this package's
//     TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere
//     (coordinator_stop_tree_test.go);
//   - the dead-host sweep semantics (dead recovers, alive/own skip, terminal
//     roots keep their state, never re-labelled):
//     internal/session batch_recovery tests;
//   - the production crash-then-BatchSweepDeadHosts-over-the-same-data
//     integration: internal/app/app_batch_provider_crash_test.go.
package agent
