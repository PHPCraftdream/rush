// The bg-shell completion WINDOW tests (R7B-1) were DELETED as obsolete
// contracts: their mechanism was the deleted ledger park --
// TestChildScope_FinishedShellStaysOpenUntilItsNoticeCommits asserted the
// window through ledger.recheckChild + hasParked + the onWebDone delivery
// channel ("the parent gets the child's reaction"), and
// TestBGShellDone_OwnRecheckRunsAfterTheHoldDrops asserted a driver release
// fired from the ledger's finish/recheckChild path (releaseDriverIfScopeClosed
// has no production caller in the batch world any more).
//
// The retained window semantics live on and are pinned where they actually
// live now:
//   - the completion hold (PendingCompletionsOwned) across the shell-finished
//     -> notice-committed window, including the bgArrival gate:
//     bgshell_observer_recheck_test.go and cliscope_completion_hold_test.go;
//   - the committed notice's delivery as a batch reaction/wake:
//     batch_reaction_persist_test.go and batch_notice_presentation_test.go;
//   - the delegated-child wake on a committed bg-shell completion:
//     coordinator_bgshell_child_test.go
//     (TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays);
//   - the post-commit re-check reaching a session through the recheck set
//     (noteSubAgentChildRunEnded -> recheck set -> RecheckPass):
//     coordinator_recheck_child_test.go and session_idle_trigger_test.go.
package agent
