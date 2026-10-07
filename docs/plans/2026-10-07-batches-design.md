# Batches — design (read-only review, 2026-10-07)

Status: proposal, awaiting operator decisions (section 8). File:line references
are to `main` at the time of writing. "Batch" already names the fs_* batch
runner (`tools/fs_batch.go:3-6`), so the new tools are `batch_run`,
`batch_add`, `batch_status`.

## Requirement (operator)

A batch is a list of items: a nested batch, a CLI command, a tool call, an MCP
call, or an agent launch. Parallel batches start everything and wait for all;
sequential batches run items one by one and either stop on a failed item or
continue. Notifications: per item, or only when all items stopped. Several
batches may run at once. Any running batch accepts new items; any item at any
depth can be stopped, its output inspected, and an agent item can receive an
injected message.

## 1. Surface

v1: the model, through tools (covers `rush run` and web sessions). Everything
in the ledger is keyed to session + tool call: claim/ack gate
(`work_ledger.go:254-351`), "started" answer (`async_tool.go:265-270`), notice
pull/wake (`agent_notice_pull.go:22-66`, `coordinator_wake.go:27-79`). A
CLI-created batch would still need an owner session and a host process, and
there is no cross-process job kill (`sessions_jobs.go:11-18`). The operator can
already observe leaves (`rush sessions jobs`, web LiveWorkPanel
`handlers_livework.go:1-13`, `rush run` heartbeat `app_run_async_wait.go:21-62`)
and inject into any agent item (`sessions_inject.go:202-235`). CLI = phase 2,
web = phase 3.

## 2. Data model

- Batch id = the `batch_run` tool-call id = root async_jobs key. Node id =
  `<batch>.<path>` with append-only ordinals (`b.2.1`); a leaf's node id is its
  async_jobs `tool_call_id`; an agent leaf's child session =
  `CreateAgentToolSessionID(batchMsgID, nodeID)` (`session_lifecycle.go:160`).
- Batch node: `{mode: parallel|sequential, on_fail: stop|continue,
  notify: each|all, max_parallel}`. Leaves v1: command (`run_command`), agent;
  later bash, tool, mcp. Caps: depth 3, 100 nodes per tree.
- Item states: pending → running → completed|failed|cancelled|timed_out|
  interrupted, or pending → skipped. A batch settles when all children are
  terminal and then refuses adds; failed if any child failed/timed out/was
  interrupted; cancelled if stopped.
- Failure per kind: command — `IsError` (exit ≠ 0, not found, process timeout,
  agentguard/hook refusal; `run_command.go:141`, `:236-268`); agent — the
  delegation is released with an error (`coordinator_subagents.go:334-346`,
  `work_ledger_delegation.go:98-121`; a question is not terminal, it holds the
  delegation); tool/mcp (phase 2) — `IsError` or a Go error (`mcp-tools.go:161-164`);
  any — host died → interrupted (`async_job_recovery.go:110-161`).
- Cancellation: stopping a node refuses adds, skips pending children and stops
  running leaves by kind (command → `StopRunCommandJob` `work_ledger.go:817-875`;
  agent → `coordinator.Cancel(child)` as `StopAgent`
  `coordinator_agent_control.go:274-286`; nested batch → recurse). Stopping a
  leaf in a nested parallel batch touches only that leaf. Session Stop already
  covers the whole tree (`work_ledger_delegation.go:321-360`).
- Persistence v1: no tree table; root and leaves are async_jobs rows, the spec
  is the stored tool-call input. One migration adds kinds `batch` and `tool` to
  the async_jobs kind check (bg_shell rebuild pattern,
  `20261005000001_bg_shell_jobs.sql:17-75`). Restart: graceful exit = crash
  (`work_ledger.go:955-997`); recovery marks root and running leaves
  interrupted (`async_job_recovery.go:138-149`); no resume.

## 3. Execution

- Root: `batch_run` wrapped by asyncTool (`async_tool.go:527`) → claim, ack
  gate, "started", supervision (`:206-210`); the inner Run is the batch engine
  and returns the summary when the tree settles (`finalize`/`finish`,
  `:375-385`). Never the sync path: origin-less Drain/web turns make asyncTool
  synchronous (`async_tool.go:71`), which would block a turn for the whole
  batch, so `batch_run` is always non-sync and SDK-origin calls are refused in
  v1.
- Leaves: new `workLedger.startBatchLeaf` in a new `work_ledger_batch.go`
  (`work_ledger.go` is at 997 lines); claim + announced in one step like
  `ClaimShell` (`async_job_bgshell.go:50-82`); executor = existing
  `asyncTool.run` + `finalize` (`async_tool.go:296-385`); inner tool wrapped
  only by restricted-run and hooks (`coordinator_tools.go:808-814`), agentguard
  runs inside the tool. Agent leaves inherit permissions like an agent call
  (`async_tool.go:180-195`).
- Tools run outside a model turn already (`tools.go:20-52`,
  `turn_stall_tool_detach.go:15-24`). Item tools come only from the caller's
  own filtered tool set (keeps orchestrator stripping
  `coordinator_tools.go:194-208` and the recursion guard `:627-641`); never
  items: `batch_*`, `job_*`, wake tools, `ask_question`. A hook Halt fails the
  item.
- MCP (phase 2): existing `tools.Tool.Run` → `Owner.RunTool`
  (`mcp-tools.go:132-184`); cancel via item ctx (INV-19). No change in
  `internal/agent/tools/mcp`.
- Limits: the 50-jobs-per-session cap counts root + leaves
  (`work_ledger.go:24`, `:271-277`); a cap refusal is back-pressure (item stays
  pending), never a failure. `max_parallel` default 4, max 16; process-wide
  running command items default 2 (memory ceiling).
- Ordering: sequential claims the next item only after the previous leaf's
  terminal commit; parallel launches in ordinal order. Timeouts: per item
  `TimeoutSpec`/`parseTimeoutParam` (`work_job.go:60-64`,
  `async_tool.go:451-519`); per batch via `batch_run` timeout
  (`terminate_and_wake` stops the tree).
- Item-finished hook: only when the in-memory adoption wins
  (`transitionToTerminal` true, `work_ledger_transition.go:297`) and after
  `l.mu` is released (`work_ledger.go:370-371`) → exactly one event per leaf.

## 4. Notifications

A node notifies when it finishes if its parent is `notify=each`; the root
always notifies. Leaf under `each`: its own async_jobs notice via
`notifyAsyncCompletion` → `wakeSession` (`coordinator_background.go:71-93`).
Leaf under `all`: override right after `causeStateNoticeKindWake`
(`work_ledger_transition.go:231`) to delivery=done, wake=0, reacted=1 (as
job_kill, `:107-109`). Nested batch under `each`: new session_notices kind
`batch_done` (`notice_pull.go:37-67`, `:315`). Root under `each`: wake=0.
`child_question` and `wake_only` are never silenced. Operator: v1 the `rush
run` heartbeat names leaves (`app_run_async.go:805`); phase 2 one stderr line
per item; phase 3 web.

## 5. Operations

- Add: `batch_add(batch, parent, items)`, owner-checked.
- Stop: `job_kill(item)` — run_command leaf works today
  (`work_ledger.go:621-625`); agent leaf → `stop_agent`; batch node → new
  intercept-first branch like `stallDetachKillResponse` (`job_kill.go:63`).
- Output: `job_output(item)` for run_command (`work_ledger.go:773-793`); agent →
  `inspect_agent`/`read_delegation_transcript`; new `batch_status` (tree,
  states, result summaries).
- Inject: `inject_agent(child)` at any depth (every agent item is a direct
  child of the owner, `coordinator_agent_control.go:24-36`); operator `rush
  sessions inject <child>`. Gap: an idle held child only stores the message
  (`:246-249`).

## 6. Risks and invariants

No dependency cycles (children never get batch tools; owner-scoped job lookup
`work_ledger.go:596-604`; questions wake the owner). The engine never holds its
mutex across ledger calls. Stops go by item kind (snapshot → transition → kill,
`work_ledger.go:797-804`). One finish event per leaf, one notice per notifying
node. Fan-out capped. `batch_run` joins `stallDetachExcludedTools`
(`turn_stall_tool_detach.go:31-36`). `each` costs a paid drain turn per item
(no cap like `coordinator_bgshell_cap.go:3`), hence default `all`. New code in
new files (1000-line rule). Acceptance tests are the finish line (review stop
rule).

## 7. Phases

v1 (kinds batch/command/agent; parallel/sequential; both notify modes; add,
stop, status; caps). New files: migration `async_jobs_batch_kind`,
`agent/batch_engine.go`, `agent/batch_leaf.go`, `agent/work_ledger_batch.go`,
`tools/batch_run.go`+`.md`, `tools/batch_control.go`+`.md`. Edits:
`async_tool.go` (`:71`, `:452`, `:527`), `work_ledger_transition.go` (`:231`,
`:297`), `coordinator_tools.go`, `job_kill.go`, `job_output.go`,
`turn_stall_tool_detach.go`, `config.go:416`, `notice_pull.go`.

Acceptance tests: (1) parallel [ok, exit 1, ok] + `all` → one notice 2 ok/1
failed, leaves delivery=done wake=0; (2) sequential `stop` → third never
claimed, reported skipped; (3) sequential `continue` → third claimed after the
second's commit; (4) `each` → three leaf notices, root wake=0; (5) job_kill on a
nested-parallel leaf leaves siblings/parent running, on a nested batch id stops
only its subtree; (6) agent item: inject at depth 2 accepted, a question holds a
sequential batch and wakes the owner in `all`; (7) session Stop → nothing
claimed afterwards; (8) `batch_add` starts at once on a running parallel batch,
refused on a finished one; (9) 60 items, max_parallel 16 → no cap failures,
never more than 50 open; (10) origin-less turn → "started" immediately; (11)
host crash → interrupted; (12) `rush run` exits only after the batch finishes.

Phase 2: `job_batch_nodes` table; `rush sessions batches`; control-request
table polled by the owner for operator stop/add (patterns
`agent_turn_step.go:72`, `session_update.go:311-320`); stderr line per item;
kinds tool, mcp, bash. Phase 3: web (LiveWorkPanel tab, livework snapshot,
WebSocket commands next to `protocol.go:96-103`, Playwright). Phase 4 (only if
asked): resume after restart; model-free batches.

## 8. Operator decisions (recommended defaults)

1. v1 surface = model tools only — recommended yes.
2. Concurrency defaults: max_parallel 4 per batch, 2 running command items per
   process, 4 agent items per batch — recommended.
3. A manually stopped item does not count as a failure for `on_fail=stop` —
   recommended (the sequential batch moves on).
4. Default notify mode `all` (`each` costs a paid turn per item) — recommended.
5. v1 "CLI command" = `run_command` only (no shell, 600 s cap), bash in phase 2
   — recommended.
6. After a host restart: mark interrupted, no resume — recommended.
