Wait for your live background work to finish before continuing.

<usage>
- Use ONLY while the session has live work: your own background jobs (bash,
  run_command) or running delegations (the `agent` tool). Calling it when
  nothing is running is refused with "nothing is running; continue or
  finish" — then just continue or end your turn normally.
- `until` selects the wake mode: "any" (default) wakes you on the FIRST
  completion; "all" sleeps until every running task has finished. Use "all"
  when you need every result before you can act.
- `max_wait_seconds` (60..21600) is an optional safety deadline: a wake
  fires then even if the work is still running, so you can reassess. The
  schedule id comes back in the result.
- As the TOP-LEVEL agent: the call SUCCEEDS and ends your turn immediately.
  The turn does not continue past this result; do not call any other tool
  after this one.
- As a DELEGATED WORKER: the call BLOCKS inside this turn until the wake
  (first completion for "any", all finished for "all", or the
  max_wait_seconds deadline, capped below the tool watchdog) and returns
  with the result. The delegation to you stays open the whole time: never
  let your turn end while jobs you still need are running — call
  await_tasks instead.
</usage>

<important>
This is the correct way to wait for running work. Never wait by calling
ask_question, and never poll inspect_agent/job_output in a loop — those
spend tokens and wait for nothing.
</important>

<tips>
- Prefer `until: "all"` when you dispatched several chunks and need them
  all; the default "any" is best when the first result already unblocks you.
- Add a short `note` when the reason for waiting would otherwise be unclear
  when you wake up.
</tips>
