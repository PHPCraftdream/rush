# R12 flag-ON failure inventory (shadow run 38602025121, branch ci-shadow/r12g-r3, commit 1b2e76e2)

Complete list of tests that FAIL when the whole suite runs with `RUSH_BATCH_RUNTIME=1` (no `-failfast`, `-skip TestCoderAgent`, no `-short`), merged over ubuntu/macOS/windows. ubuntu and macOS are identical; windows adds the rows marked (W).

Each failing test is either (a) a test of LEGACY semantics that R12-H deletes or ports (assertions about `async_jobs`, ledger waits, job-kill/job-output prompt rules), or (b) a real gap the ON path must close before H. The owner column is the package that must classify and act. A `panic` row means the package binary aborted: tests after the panic did not run, so the true list for that package can be longer; rerun after the panic is fixed.

| package | failing test | proposed owner |
| --- | --- | --- |
| internal/agent | TestAwaitTasksTopLevelPathUnchanged | R12-E |
| internal/agent | TestAwaitTasksWorkerBlocksUntilOwnJobFinishes | R12-E |
| internal/agent | TestAwaitTasksWorkerCancelUnblocks | R12-E |
| internal/agent | TestAwaitTasksWorkerCapAppliesWithoutMaxWait | R12-E |
| internal/agent | TestAwaitTasksWorkerRefusesWhenNothingRunning | R12-E |
| internal/agent | TestAwaitTasksWorkerUntilAllWaitsForBothJobs | R12-E |
| internal/agent/prompt | TestBuild_AwaitTasksRule_TracksWorkerAvailable | R12-E/H (prompt text) |
| internal/agent/prompt | TestBuild_ExplicitFalseHint_ByteIdentical | R12-E/H (prompt text) |
| internal/app | TestRunLoop_AwaitTasksAll_DefersUntilEveryTaskFinishes | R12-F |
| internal/app | TestRunLoop_AwaitTasksAny_WakesOnFirstCompletion | R12-F |
| internal/app | TestRunLoop_AwaitTasksMaxWaitAny_TaskFinishesFirst_ScheduleCancelled | R12-F |
| internal/app | TestRunNonInteractiveContinuesPastFiveAsyncCompletions | R12-F |
| internal/app | TestRunNonInteractiveDefaultCLIModeWaitsForAsyncCommand | R12-F |
| internal/app | TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON | R12-F |
| internal/app | TestRunNonInteractiveWaitsForAsyncSubAgentResult | R12-F |
| internal/app | TestWebAsyncCommandCompletionCreatesModelVisibleNotice | R12-F |
| internal/app | panic | R12-F |
| internal/cmd | TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork | R12-B |
| internal/cmd | TestSessionsCancel_PurgesQueuedWork | R12-B |
| internal/cmd | TestSessionsGcCmdRun_JobsOlderThan_DryRunCountsOnly | R12-B |
| internal/cmd | TestSessionsGcCmdRun_JobsOlderThan_PurgesTerminalOnly | R12-B |
| internal/cmd | TestSessionsInjectCmdRun_LiveLoopBetweenTurnsIsRunning | R12-B |
| internal/cmd | TestSessionsInjectCmdRun_OwnJobOrDelegationBetweenTurnsIsRunning | R12-B |
| internal/cmd | TestSessionsJobsCmdRun_TableAndForeignLiveHostHint | R12-B |
| internal/cmd | TestSessionsListCmdRun_LiveDescendantIsDelegatingNotDone | R12-B |
| internal/cmd | TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone | R12-B |
| internal/cmd | TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone | R12-B |
| internal/cmd | TestSessionsReset_LiveLoopRefusesEvenWithForce | R12-B |
| internal/cmd | TestSessionsShowCmdRun_AsyncJobCounts | R12-B |
| internal/config | panic (W) | R12-H (see note) |
| internal/server | TestP1_6_Rerun_FailsClosedWhenStillStopping (W) | R12-F (rerun) |
| sdk | TestRunWithCredentialsSnapshotsCallerStateBeforeDelegation | R12-F (operator decision: DiskProvider) |
| sdk | TestSDKRunDiskProviderAndRealDiskRunConcurrentSessionsIsolated | R12-F (operator decision: DiskProvider) |
| sdk | TestSDKRunWithDiskProviderRoundTripsWithoutTouchingRealDisk | R12-F (operator decision: DiskProvider) |

`internal/agent` agent shards (ubuntu/macOS/windows): only shard 1/8 is red (the six `TestAwaitTasks*` rows above, from the ubuntu inventory); the other shards were green.

## Reading notes

- `sdk` `TestSDKRunWithDiskProviderRoundTripsWithoutTouchingRealDisk` and `TestSDKRunDiskProviderAndRealDiskRunConcurrentSessionsIsolated`: R11G refuses DiskProvider calls with the flag ON (`ErrDiskProviderNotDurable`). This is the operator decision recorded in the R12 decomposition (SDK custom-disk contract), not a defect.
- `internal/app` `panic`: a test panics under ON; fix first, then rerun to reveal the hidden tail.
- Windows only: `internal/config` panic and `internal/server` `TestP1_6_Rerun_FailsClosedWhenStillStopping` need their own look (possible ON-specific flake or inherited-env interaction).
- `internal/agent/prompt` `TestBuild_AwaitTasksRule_TracksWorkerAvailable` / `TestBuild_ExplicitFalseHint_ByteIdentical`: the prompt text changes under ON (R13); at H the flag-off byte-identical expectation is retired.
