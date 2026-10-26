// The stop-releases-parked-child-driver tests (R7B-2, R8B-4) were DELETED as
// obsolete contracts: both premised the deleted ledger park --
// TestStop_ReleasesTheChildDriverOfAParkedDelegation armed the delegation
// through ledger.Start/acknowledge/armDelegation + hasParked and stopped it
// three ways including StopRerunJobs over session.VoidedAsyncJob (deleted);
// TestStop_StaleExecutorReleasesTheChildDriverItsShellHeld asserted a driver
// release fired from the ledger's finish stale-job branch (recheckChild is
// deleted; releaseDriverIfScopeClosed's production caller moved to the
// release path -- afterReleaseContext's releaseChildDriverIfScopeClosed
// tail, supervision.go).
//
// The retained halves are pinned where they live now:
//   - the transitive Stop (tree walk, suspension, wake zeroing, recheck-set
//     registration of the stopped children):
//     coordinator_batch_stop_test.go, coordinator_batch_stop_running_test.go,
//     coordinator_batch_stop_terminal_test.go, TestStop_NSubAgentsEachOwnBash_
//     NoNewTurnAnywhere (coordinator_stop_tree_test.go);
//   - the bg-shell completion hold and the shell manager's kill/ownership
//     semantics the stale-executor case exercised:
//     bgshell_observer_recheck_test.go, cliscope_completion_hold_test.go;
//   - the rerun-void transitive stop over live batch roots:
//     BatchRerunStopVoided (batch_cutover_rerun.go) and its tests.
package agent
