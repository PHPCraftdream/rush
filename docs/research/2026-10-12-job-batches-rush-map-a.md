<!-- Read-only code map produced for the job-batches Rush integration plan. Anchors are as of commit 48890aaa; re-read the code before relying on a line number. OBSERVED = seen in source, INFERRED = integration conclusion, neither is executed verification. -->

# Area A — tool execution paths and executor integration

**Method:** strictly read-only source inspection; one worker at a time, followed by direct verification of the principal source anchors. **OBSERVED** below means observed in code, not runtime-tested. **INFERRED** identifies proposed implementation or consequences. No files changed; no commands, builds, tests, vet, or lint ran.

For compact citations, these aliases denote repo-relative files:

| Alias | File |
|---|---|
| Design | `docs/plans/2026-10-07-job-batches-design.md` |
| Construction | `internal/agent/coordinator_tools.go` |
| Async | `internal/agent/async_tool.go` |
| Stall | `internal/agent/turn_stall_tool_detach.go` |
| Ledger | `internal/agent/work_ledger.go` |
| Shell | `internal/shell/background.go` |
| CLI-stream | `internal/agent/cliprovider/provider_stream.go` |
| CLI-tools | `internal/agent/cliprovider/mcpserver_tools.go` |

## 1. CURRENT STATE

### A. Model-visible construction and policy

**OBSERVED — the main construction path is per-call, but not universal.**

`pinCallTools` builds and pins tools, fails closed when a distinct scoped toolset is required, and waits for embedded child-agent construction. It does not publish those tools onto the shared agent (`internal/agent/coordinator_models.go:610–643`; `Construction:569–578`).

`buildTools` applies disable-subagents, worker layering, folder scope, reviewer filtering, and finally the no-real-workspace capability floor. It captures the call’s `FolderScope` and `DiskProvider` (`Construction:574–604`). Folder scope substitutes `fs_*`, removes escape hatches, conditionally retains command tools, and disables external MCP tools (`Construction:318–348`, `Construction:386–425`).

The coordinator constructs built-ins, appends live MCP candidates subject to the pinned MCP allowlist, sorts, then wraps:

**stall-detach → hooks → restricted-run → async → underlying tool**

Only bash, run_command, agent, and agentic_fetch receive the async wrapper (`Construction:701–824`; `Async:521–531`).

Hooks can deny/halt, rewrite input, or stamp call-ID permission approval; child tools deliberately omit hook interception (`internal/agent/hooked_tool.go:27–39`, `internal/agent/hooked_tool.go:62–107`). Restricted-run dispatch handles command parameters and dynamically supplied `RestrictedRunAction`; hook approval bypasses this gate (`internal/agent/restricted_tool.go:102–153`). Permission requests separately handle pre-gate approval, hook approval, and session restricted allowlists (`internal/permission/permission.go:461–527`).

There are additional assembly paths:

- `agentic_fetch` privately constructs web/search/glob/grep/view tools and its own `NewSessionAgent` (`internal/agent/agentic_fetch_tool.go:170–212`).
- `NewSessionAgent` and `SetTools` add restricted wrapping and logging, not the coordinator’s complete pipeline (`internal/agent/agent.go:839–852`, `internal/agent/agent.go:887–891`).
- CLI-provider bridge tools implement execution independently (`CLI-tools:54–97`, `CLI-tools:117–184`).

**OBSERVED — agentguard is part of execution, not merely tool filtering.** Bash checks command recursion/window safety, blocklists, linked-worktree git writes, then permission; run_command checks structured argv and linked-worktree git writes (`internal/agent/tools/bash.go:142–197`; `internal/agent/tools/run_command.go:139–200`). CLI bridge Bash repeats these guards (`CLI-tools:140–176`). Shared guard entry points are `CheckArgs`, runtime `CommandBlockFunc`, and `CheckAll`; linked-worktree detection examines `.git` indirection (`internal/agent/agentguard/agentguard.go:159–175`, `internal/agent/agentguard/agentguard.go:226–255`; `internal/agent/agentguard/git_writes.go:153–171`).

### B. Execution families and real completion sources

| Family | OBSERVED current execution |
|---|---|
| Ordinary built-ins / `fs_*` | `fs_write` requests permission once, then calls `RunFSBatch`; its same-path group is the filesystem atomicity unit, not a scheduler batch (`internal/agent/tools/fs_write.go:79–113`; `internal/agent/tools/fs_batch.go:163–193`). |
| Fetch/download | Permission precedes context-bound HTTP. Fetch defers body close and reads a bounded body; download streams into a temporary file and removes it on copy failure (`internal/agent/tools/fetch.go:79–145`; `internal/agent/tools/download.go:97–154`, `internal/agent/tools/download.go:170–207`). |
| Bash | Both explicit background and foreground-with-auto-background launch detached `StartOwned`. Foreground delivery polls output; caller cancellation attempts bounded Kill, ignores its error, and returns `ctx.Err()` (`internal/agent/tools/bash.go:199–204`, `internal/agent/tools/bash.go:272–342`). |
| Background shell | `ExecStream` returns, outcome is recorded, active count decremented, then `done` closes. `KillOwned` removes manager lookup, cancels, and waits for done **or** control-context expiry (`Shell:485–523`, `Shell:607–624`). |
| run_command | Permission → context deadline → configured process Start → live-output registration → actual `cmd.Wait` (`internal/agent/tools/run_command.go:164–221`). |
| MCP | `tools.Tool.Run` requests permission, invokes `Owner.RunTool`, then converts text/image/media. Owner holds a client lease and invokes `lease.session.CallTool(lease.ctx, …)` (`internal/agent/tools/mcp-tools.go:132–183`; `internal/agent/tools/mcp/tools.go:157–176`). Lease release is distinct from server/session closure (`internal/agent/tools/mcp/lease.go:305–320`). |
| CLI provider | PTY has `ptycmd.Wait → ptyWaitDone`; pipe mode has `cmd.Wait → pipeWaitCh`. Both outward wait closures can return on caller cancellation before those actual sources complete (`CLI-stream:415–470`, `CLI-stream:586–630`). |
| CLI bridge | Bridge Bash runs its own `runShell`; external MCP callbacks perform permission then call a proxy. That proxy directly invokes Owner and returns only `Content` (`CLI-tools:54–97`, `CLI-tools:179–184`; `internal/agent/coordinator_providers.go:906–911`). |
| Agent/delegation | Resume validates direct-parent ownership; new calls create task sessions. The driver is registered before execution, with generation-bound restricted-policy inheritance (`internal/agent/coordinator_subagents.go:49–82`, `internal/agent/coordinator_subagents.go:234–279`). |

**OBSERVED — a tool response is not always termination.** Async bash forces background mode for CLI/web and later waits on its shell; after cancellation it ignores Kill failure and reads output without requiring a successful final wait (`Async:312–353`, `Async:387–416`). Therefore that path cannot be reused unchanged as a termination-reporting executor.

**OBSERVED — delegation completion already exceeds one model turn.** Async finalization parks agent results; release checks running child work, shells/completion holds, busy drivers, and cross-process scope (`Async:360–384`; `internal/agent/work_ledger_delegation.go:115–177`, `internal/agent/work_ledger_delegation.go:221–255`).

Held-question answering resumes the existing driver under `answerHold`, but explicitly excludes synchronous delegations (`internal/agent/delegation_question.go:173–235`). `ask_question` currently returns a hint instead of stopping when own work is live; otherwise it throws `AskQuestionError`. `runSubAgent` converts awaiting-answer into a successful question-shaped response (`internal/agent/tools/ask_question.go:112–125`; `internal/agent/coordinator_subagents.go:327–337`).

### C. Pre-batch jobs, storage, output, and cancellation

**OBSERVED — there are several overlapping identity/control mechanisms.**

1. **Work-ledger jobs:** non-sync launch calls `AsyncJobStore.Claim`; kinds map agent→agent, agentic_fetch→fetch, commands→command (`Ledger:283–305`; `internal/agent/work_ledger_transition.go:131–141`).
2. **SDK shell escape:** `claimBackgroundShellRow` creates a separate `bg_shell` row keyed by shell ID and installs an OnDone terminal writer (`internal/agent/bgshell_claim.go:49–119`).
3. **Stall jobs:** an outer wrapper moves eligible synchronous calls into goroutines; a stall signal returns a `stall-*` identity while execution continues. Static exclusions omit dynamically named MCP tools (`Stall:31–38`, `Stall:93–148`).

The durable store currently owns terminal CAS arbitration: `Transition` returns won/lost/gone, and the ledger retries errors and adopts the committed row (`internal/session/async_job_store.go:481–557`; `internal/agent/work_ledger_transition.go:244–300`). Started acknowledgment fuses message insertion and announced state; its ledger caller aborts on failure (`internal/session/notice_pull.go:340–376`; `internal/agent/work_ledger_announce.go:155–176`). Dead-host recovery probes the host lock, deletes unannounced rows, and interrupts announced running rows through the same Transition API (`internal/session/async_job_recovery.go:92–149`).

`job_output` tries stall control first, then resolves ledger/run_command/shell paths. Shell output concatenates stdout and stderr snapshots and uses snapshot-length cursors; wait means output growth or completion (`internal/agent/tools/job_output.go:70–145`). Stall output is final-only and ordinarily deleted after reading (`Stall:245–273`).

`job_kill` similarly routes stall first, then durable shell/run_command/ledger control. It can return a committed “stopped” result even when shell Kill fails (`internal/agent/tools/job_kill.go:61–105`, `internal/agent/tools/job_kill.go:130–203`). run_command terminal cancellation is committed **before** executor cancellation, without waiting for `cmd.Wait` (`Ledger:849–875`).

**OBSERVED — output contracts differ.** Shell output is separate head/tail-style snapshots with timed release; run_command uses a merged tail-retained buffer and signed cursor normalization (`internal/shell/background_shell.go:12–59`; `internal/agent/tools/run_command_output_buffer.go:17–40`, `internal/agent/tools/run_command_output_buffer.go:84–103`).

### D. Executor requirements

**INFERRED — use the implemented interface, but introduce policy-leaf handles before ingress.** `Start` rejects only when no execution exists; report means execution stopped. Output, message, and disposal capabilities are optional (`internal/jobbatch/executor.go:24–54`).

| Family | Start / completion | Stop | ReadOutput / Inject / Dispose |
|---|---|---|---|
| Ordinary built-ins, fs, HTTP | Run the **pinned policy-wrapped leaf**, report after return and cleanup. One `fs_*` call remains one leaf. | Cancel execution context; synchronous non-cancellable I/O remains live until return. | Retained response stream/envelope; Inject unsupported; Dispose releases retained data. |
| Bash | Extract launch returning retained shell handle; report from real done source, not background acknowledgment. | Cancel retained handle; bounded control failure must not lose handle or settle task. | New merged absolute-cursor stream; no Inject; Dispose releases buffers/timers after termination. |
| run_command | Extract Start/Wait/output handle; report only after actual Wait. | Tree cancellation, separately from settlement. | Adapt merged buffer with oldest/next/EOF; no Inject; release handle/output. |
| MCP | Call existing Tool.Run→Owner.RunTool under per-call execution context; report local call return. | Cancel that invocation, **never close shared Owner/session**. | Preserve response media and failure envelope; no generic Inject; dispose invocation-local retention only. |
| CLI provider | Retain actual PTY/pipe Wait source beyond iterator cancellation. | Existing process-tree containment, bounded control outcome distinct from Wait. | Retained merged stream; generic Inject unsupported unless a concrete existing capability is selected; dispose process/PTY resources once safe. |
| Agent/agentic_fetch | Retain driver plus scope; report after confirmed scope closure, not model-turn return. | Cancel driver and owned descendants without waiting for new dependency admission. | Output/response envelope; Inject answers held question or sends existing message control; Dispose releases generation-bound driver/policy/resources. |

These requirements follow the lifetime contract, especially cancellation acknowledgment versus termination and post-report disposal (`Design:490–504`, `Design:563–613`). The policy leaf must exclude async/stall/ingress wrappers; payload must resolve only within the pinned filtered tool registry (`Design:2152–2158`).

## 2. What the design should say differently

**INFERRED corrections, grounded in the observed paths:**

- Replace “one construction path” with an explicit consolidation inventory: coordinator, agentic_fetch private tools, SessionAgent boundaries, and CLI bridge callbacks. The latter two do not currently share coordinator assembly (`Construction:826–831`; `internal/agent/agentic_fetch_tool.go:170–212`; `CLI-tools:54–97`).
- State that **retaining completion sources is prerequisite work**, not merely adding wrappers. Shell lookup disappears during Kill; async bash and CLI-provider return paths can precede actual completion (`Shell:607–624`; `Async:409–416`; `CLI-stream:462–470`, `CLI-stream:609–627`).
- Explicitly remove DB-CAS terminal authority and early “stopped” completion. Preserve neither old row contracts nor terminal-on-intent behavior (`Ledger:849–875`; `internal/session/async_job_store.go:519–557`). The replacement remains batch_nodes/control requests and fatal commit failure (`Design:2197–2254`).
- Describe universal held questions as **new behavior**. Existing sync exclusion and successful question-shaped return do not implement it (`internal/agent/delegation_question.go:187–195`; `internal/agent/coordinator_subagents.go:327–337`).
- Require structured MCP error/media preservation at both boundaries. Owner conversion never consults upstream `IsError` in the viewed conversion, and CLI proxy drops everything except Content (`internal/agent/tools/mcp/tools.go:171–231`; `internal/agent/coordinator_providers.go:906–911`).
- Resolve the “every invocation” versus “await holds no admission unit” tension before implementation; both statements appear in the design (`Design:2162–2165`, `Design:2318`). Generic task items must still reject scheduler/control recursion (`Design:2411–2414`).

## 3. Independently mergeable implementation breakdown

All steps are **INFERRED proposals**. Focused tests listed here are future acceptance work, not executed results.

| Step | Goal and files to touch | Do not touch | Verification; dependencies; size/risk |
|---|---|---|---|
| **A1 Policy-leaf registry** | Extract pinned filtered registry and shared policy decoration from `coordinator_tools.go`; integrate `agentic_fetch_tool.go`, `agent.go`, `restricted_tool.go`, `hooked_tool.go`. Basis: `Construction:574–824`. | Engine, DB, prompts. | Hooks rewrite/deny once; dynamic MCP restriction; folder/custom-disk floors; child hook omission; unknown target fails closed. No dependency. **M**; wrapper recognition/pinning risk. |
| **A2 Shell lifetime seam** | `shell/background.go`, `background_shell.go`, `tools/bash.go`: retained cancellation/completion/output handle independent of manager lookup. Basis: `Shell:485–624`. | Notice schema and new identities. | Kill timeout leaves done open; repeated Stop; late done; output until Dispose. **M**; retention/manager removal risk. |
| **A3 CLI lifetime seam** | `cliprovider/provider_stream.go`: expose actual PTY/pipe completion independently from canceled iterator return. Basis: `CLI-stream:415–470`, `CLI-stream:586–630`. | Model catalog and output-parser policy. | Held pipe/PTY completion, late Wait, final output, cleanup once. **M–L**; platform and scanner ordering risk. |
| **A4 Managed-call detach exclusion** | `turn_stall_tool_detach.go`, construction wrappers: capability/context-based exclusion, including dynamic MCP. Basis: `Stall:31–38`. | Unmanaged stall-clock behavior. | Managed calls create zero stall identities; unmanaged behavior retained. Depends A1. **S**; marker forwarding risk. |
| **A5 Generic/MCP adapters** | New Rush adapter files; `tools/mcp-tools.go`, `tools/mcp/tools.go` for full outcome envelope. Basis: `internal/jobbatch/executor.go:24–54`. | Shared MCP lifecycle; production ingress. | Report-before-Start-return, cancellation, denied/IsError, panic containment, copied output, two shared calls/stop one. Depends A1/A4. **M**; remote termination ambiguity. |
| **A6 Process adapters** | Separate shell and argv commits touching adapters, `bash.go`, `run_command.go`, output buffer; CLI adapter consumes A3. Basis: `internal/agent/tools/run_command.go:203–221`. | Old ledger terminal writers. | Stop acknowledgment/timeout never settles; real completion settles once; cursor/EOF/truncation; Dispose barrier; one termination timer. Depends A2/A3. **L** total; split by family. |
| **A7 Agent scope adapter** | Extract scope observation/control from `work_ledger_delegation.go`, `coordinator_subagents.go`, `delegation_question.go`, driver/control files; wire agent/agentic_fetch adapters. Basis: scope checks at `internal/agent/work_ledger_delegation.go:221–255`. | Outcome-declaration policy, new outbox schema. | Turn-ended/descendants-live; held question with/without work; same-execution Inject; Stop/Inject race; generation cleanup. Depends A1 and new live-root reader seam. **L**; cross-process scope/debt coupling. |
| **A8 CLI bridge unification** | `cliprovider/mcpserver_tools.go`, `coordinator_providers.go`: callbacks resolve pinned policy leaves through shared ingress. Basis: `CLI-tools:54–97`, `CLI-tools:179–184`. | Provider protocol/parser refactors. | Guards/restrictions once, no raw factory bypass, full MCP media/error, exactly one identity. Depends A1/A5/A6. **M**; nested admission risk. |
| **A9 Profile and cutover seam** | Consumer adapter/profile validator, coordinator ingress, async/stall/job-control retirement seams. Basis: `Design:2289–2295`. | Core kind/rank hardcoding; compatibility migration. | OneItem direct ingress; no nested rewrap; closed caps; forbidden rank/gate dependency; Add validation; AW-a/AW-b. Depends A5–A8 and persistence owner. **M–L**; control/self-wait risk. |

**Activation boundary:** these seams can merge disabled. Production activation requires the separate durable-model owner’s ordered commit hook, root acknowledgment/delivery, fatal-exit wiring, and live-root await/scope reader. Do not implement adapters by calling the old Claim/Transition terminal machinery (`Design:2212–2279`, `Design:2297–2327`).

### First defensible profile

**INFERRED, restricted initial mapping:** finite caps for every accepted group; global ≥ sum of caps; validate descending separate-root dependencies, including Add (`Design:689–717`).

- **rank 0 `io`:** filesystem, HTTP, independent commands, non-reentrant MCP.
- **rank 1 `cli_driver`:** CLI calls that wait on rank-0 bridge roots.
- **rank 2 `agent_driver`:** agent/agentic_fetch, allowed to wait on ranks 0/1; no equal-rank recursion.
- Immediate status/Stop/Inject directives need an explicitly decided control treatment; blocking await/output must not be classified as dependency-free rank 0.

This is capability-based, not the current coarse durable kind map (`internal/agent/work_ledger_transition.go:131–141`). Existing worker await captures job pairs and waits for disappearance; batch conversion must exclude its own invocation and enclosing execution to avoid self-dependency (`internal/agent/coordinator_await_tasks.go:111–178`; `Design:2411–2414`).

## 4. Operator/design questions

1. **Control accounting:** how does universal batching reconcile await’s stated zero-unit accounting? Which self/enclosing roots are excluded from capture? (`Design:2162–2165`, `Design:2297–2318`)
2. **Termination guarantee:** is local MCP return sufficient, and does process termination require Wait plus joined I/O or stronger tree-empty confirmation? Current sources establish neither remote side-effect termination nor a universal tree-empty proof (`internal/agent/tools/mcp/tools.go:166–176`; `CLI-stream:415–470`).
3. **Caps/ranks:** choose finite numeric caps and whether the restricted closed profile is mandatory. Held-agent answers require external Inject; do not promise unconditional descending-rank progress across parent-answer dependencies (`Design:696–717`, `Design:2294–2295`).
4. **NotifyEach via Add:** operator wording says “only batch_run,” while the design permits notification policy on nested batch nodes supplied through batch_add. Clarify that precise boundary (`Design:2380–2383`).
5. **Agent outcome:** keep declaration requirements undecided until explicitly selected; adapters must not infer success from prose (`Design:2332–2335`).
6. **Commit fault:** freeze hook and containment-to-process-exit behavior before enabling adapters; no retry/continued writer is compatible with the chosen contract (`Design:2231–2254`).

**Files changed: none. Tests run: none, as required. This is a source-backed integration map, not runtime verification.**
