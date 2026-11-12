package cmd

// runLongHelp and runExample hold the "rush run" help text, sectioned.

const runLongHelp = `Run a single prompt in non-interactive mode and exit.

  WARNING — auto-bypass of inner CLI permissions:
  ` + "`rush run`" + ` is non-interactive by design (no human at the keyboard),
  so any inner CLI sub-process it spawns (claude / codex / gemini) is
  launched with its own bypass-permissions flag (claude
  --dangerously-skip-permissions, codex --approval-mode yolo, gemini
  --yolo). The sub-process can read, write, and execute anywhere the
  invoking user has permission. There is no per-tool confirmation. Use
  ` + "`rush run`" + ` only in workspaces you can afford to lose; if you need
  real isolation, wrap the invocation in an OS-level sandbox (Sandboxie
  Plus, Windows Sandbox, WSL2 + landlock, Docker, etc.). Interactive
  sessions (TUI / web) keep the normal permission flow — this WARNING
  applies only to ` + "`rush run`" + `.

== Roles and models ==
Flags: --role, --model, --fast-model, --effort

--role is REQUIRED: every invocation must declare which model slot it
wants. This avoids silently burning premium tokens on a one-liner.
Four roles exist:
  smart              the strong default slot (top-level agent runs here)
  fast               the cheap slot, for trivial work
  worker             optional, no alias; cheap slot for delegated
                      hands-on sub-task work. Never auto-selected — only
                      reachable via --role worker directly, OR indirectly
                      when a worker model is configured and this run uses
                      --role smart: sub-agents spawned by the "agent" tool
                      then run on worker, get a work-capable toolset
                      (edit/multiedit/write/bash/todos/download/fetch/
                      ask_question), and the default sub-agent ban below
                      is lifted so the orchestrator can delegate at all —
                      even against an explicit --agents single, since a
                      configured worker means delegation is the intent.
                      The smart agent's edit/multiedit/write tools are
                      also REMOVED (bash and read tools stay, for
                      verification), and its system prompt gains an
                      "Orchestrator mode" rule to delegate in
                      context-sized chunks.
  reviewer           optional, no alias; the strongest slot, for explicit
                      review invocations. Never auto-selected anywhere —
                      reachable only via --role reviewer.
                      Reviewer auto-pass: with a Reviewer model configured,
                      a successful --role smart run is automatically
                      continued with one more turn on the Reviewer model
                      that reviews everything the session did and states
                      its own conclusion. The executor's own last answer
                      stays the run's output ("final_text" in --json); the
                      reviewer's conclusion is carried additively in the
                      "review" field of the --json envelope (empty when no
                      reviewer ran). "review_verdict" carries the parsed
                      first line: pass, pass_with_notes, fail, unverified
                      (the reviewer passed the run without a single
                      read-tool call of its own), unparsed (no verdict
                      line) or error (the review turn failed). The verdict
                      is an opinion of a second model and the exit code is
                      not changed by it. The reviewer turn's toolset is
                      read-only (view/grep/glob/ls/git_read/
                      read_delegation_transcript and the fs_* read tools; no
                      write, no shell, no sub-agents, no network, no
                      questions), so a reviewer pass can never mutate the
                      workspace it audits, and it is bounded by a 60-minute
                      hang-guard on top of the run's own --timeout. An error
                      in the review turn does not change the run's outcome:
                      the executor's answer and its exit code are kept. Runs
                      that failed, were canceled, timed out, or queued are
                      returned unchanged, and an explicit --role reviewer
                      invocation is never extended. Unconfigure the reviewer
                      to disable the pass.
worker/reviewer are configured with "rush models use <smart> <fast>
--worker <model> --reviewer <model>" (or the web UI / rush.json's
models.worker / models.reviewer directly). The actual model id behind
each role comes from "rush models show"; --model overrides it for one
invocation. If the session already has a per-session worker/reviewer
override (set via the web UI's model selector), that override wins over
the config default — same precedence as the smart/fast per-session pin.

== Prompt input ==
Flags: positional args, stdin, --session for stable ids, --findings-from

Prompt sources (combined as "<stdin>\n\n<args>"):
  - positional args:   rush run --role smart "your prompt"
  - stdin pipe:         echo "hello" | rush run --role smart
  - stdin redir:        rush run --role smart < prompt.md
  - both (stdin = context, args = question)

== Session ==
Flags: --session, --continue, --system-prompt, --system-prompt-file; also "rush sessions cancel"

Sessions: --session takes either an existing session id (or hash prefix)
to continue, OR an arbitrary new id to start a fresh session with that
exact id — handy for CI where the build matrix maps to a stable key.

System prompt: --system-prompt / --system-prompt-file persists the prompt
on the session so subsequent runs with the same --session pick it up.
If neither is passed and .rush/system-prompts/default.md exists, it is
loaded automatically (see "rush claude-init" for a starter template).

Session title: if the session title is empty or "Untitled Session", the
first 60 characters of the user prompt are used as the title. This makes
"sessions list" immediately useful without a --title flag.

External cancellation: use "rush sessions cancel <id>" from another
terminal to set a DB flag; the running agent checks it after each step
and stops gracefully.

== Sub-agents ==
Flags: --agents, --aggregation, --max-agent-depth

Sub-agent policy:
  --agents <single|with-agents|agent-allow>
                            (unset)       -- DEFAULT. No sub-agents: the
                                             "agent" and "agentic_fetch"
                                             tools are removed from the
                                             toolset for this run. A
                                             non-interactive run has no UI
                                             to surface sub-agent work and a
                                             nested run burns tokens out of
                                             view (and risks recursion), so
                                             the model EXECUTES instead of
                                             re-delegating.
                            single        -- same as the default, explicit.
                                             Still yields to a configured
                                             Worker model under --role smart
                                             (see "worker" above) — a
                                             configured worker means
                                             delegation is the intent.
                            agent-allow   -- opt in: tools present, the
                                             model decides whether to use
                                             them.
                            with-agents   -- opt in + nudge the model to fan
                                             out via the "agent" tool when
                                             work is decomposable.

Sub-agent aggregation (only matters when --agents permits fan-out):
  --aggregation <summary|concat|attach>
                            summary  -- default. Parent composes a
                                        wrap-up; sub-agent detail lives
                                        only in the DB. Easy on the
                                        envelope, can lose information.
                            concat   -- prompt-nudge asks the parent to
                                        include each sub-agent's reply
                                        verbatim in final_text. Bigger
                                        final_text but no detail lost.
                            attach   -- each sub-agent's last assistant
                                        text is collected into the
                                        envelope's sub_agent_outputs
                                        array; final_text becomes a
                                        brief wrap-up. Best for
                                        machine consumers that want the
                                        structured set.
                            A reduction-loss warning ALWAYS fires in
                            envelope.warnings when parent collapses
                            sub-agent outputs to <40% of their combined
                            character count.

== Output and results ==
Flags: --json, --stream, --format, --quiet, --verbose, RUSH_FORBID_WRITES; also "rush result" and --fail-on-review

Output modes (mutually exclusive --stream / --json):
  - default (terse): tool-call names on stderr as "▶ <toolName>"; only
    the final assistant message on stdout.
  - --stream:        every assistant token streamed live to stdout.
  - --json:          a single JSON object on stdout when the run ends —
                     {session_id, exit_reason, final_text, review,
                     review_verdict, assistant_notes, tool_calls, usage,
                     duration_ms, error}. "review" carries the auto
                     reviewer pass's verdict (empty when none ran) and
                     "review_verdict" its parsed first line —
                     pass/pass_with_notes/fail/unverified/unparsed/error;
                     final_text stays the executor's answer and the exit
                     code stays the executor's. Tool-call
                     heartbeat still goes to stderr so wrappers can show
                     progress.

Shaping the model's output:
  --format <preset|string>  Appends an in-prompt hint that asks the model
                            to produce a specific shape for this turn
                            only (not persisted). Presets:
                              json              -> final answer is one
                                                   raw JSON value, no
                                                   fence, no prose.
                              json-schema:<f>   -> same + conform to <f>.
                              @<file>           -> use <file> contents
                                                   verbatim as the hint.
                              <any other text>  -> freeform "Output
                                                   format: ..." instruction.
                            With --json or --format json, the envelope's
                            final_text is also post-processed: a
                            triple-backtick "json" fence and prose
                            preamble/suffix are stripped so
                            "jq .final_text" works directly. The
                            original (unstripped) text is preserved in
                            envelope.assistant_notes when stripping ran.

Protecting harness-owned files: when the orchestrator pipes "rush run""
output through a shell redirect, the model can pick the same path for
its write/edit tool (e.g. it sees ".tmp-audit-D.json" in the prompt
and decides "I'll just write findings there directly"). Result: a
mixed file where envelope and tool output overwrite each other in
non-deterministic order. To block this set the RUSH_FORBID_WRITES
env-var to a comma-separated list of paths the write/edit/multiedit
tools must NOT touch. Example:

  out=/tmp/audit.json
  RUSH_FORBID_WRITES="$out" \
    rush run --json --role smart --format json ... > "$out" 2>"$out.err"

The tools fail with a visible error to the model when a forbidden path
is targeted; the model then either retries with a different path or
falls back to returning the content via final_text — both of which
keep the redirect target intact.

== Limits and timeouts ==
Flags: --timeout, --timeout-extends-on-progress, --timeout-hard-cap, --idle-timeout, --stall-timeout, --max-cost, --max-tokens, --job-timeout, RUSH_RUN_DEFAULT_HARD_TIMEOUT

Runaway protection:
  --max-cost 0.50     abort when session cost (USD) crosses the threshold
  --max-tokens 100k   abort when prompt+completion tokens cross the limit
                      (supports k/M suffixes: 100k = 100000, 1M = 1000000)
  Both checks fire after each agent step. The session is saved with its
  partial work so you can inspect or fork from it.
  Total run deadlines, timeout reporting and the 60s hard backstop are described
  under "Time limits" below.

Budget persistence: --max-cost, --max-tokens, and --timeout values are
saved on the session row. "sessions show" displays cost vs budget, and
"sessions locks" shows an ELAPSED / BUDGET column. ended_reason is written
when the run ends and equals its exit_reason (the --json envelope's): the
model's finish (end_turn, stop, max_tokens, ...), "error" (a failed run; also
a --max-cost/--max-tokens exit), "timeout" (--timeout or the default 6h cap),
or "canceled" (Ctrl-C, caller cancellation, "sessions cancel"; an early
cancel can leave the older spelling "cancelled"). It is empty while a run is in progress
and appears in "sessions show" and "sessions list --json" (ended_reason).

Time limits (usually leave both alone): --timeout bounds the WHOLE run's total
wall-clock time, including active work and all waits, and defaults to 0 (no explicit
deadline), but a run is never unbounded: without --timeout the default six-hour
(6 h) TOTAL wall-clock cap applies. RUSH_RUN_DEFAULT_HARD_TIMEOUT overrides this
default cap only. A positive --timeout replaces the default cap. Progress does
not reset this total cap. This deadline is separate from the per-turn stream
watchdog: --timeout-extends-on-progress can extend that watchdog, not the total
run limit. --idle-timeout (default 15m) ends the run if the agent goes quiet for
that long DURING A TURN; it does not watch a run that is waiting between turns
on a job, a delegation or a retry pause -- that wait is bounded only by
--timeout or the cap. It watches turns only. At a total deadline Rush attempts
graceful cancellation; a final envelope is not guaranteed, and a process still
alive 60s later is force-killed (exit 124). Only set --timeout when
intentionally imposing a hard external deadline or allowing the run past its
default cap. The timeout error reports the deadline source and gives the
session/role resume command; follow operator authorization and safe-resume
rules; do not infer deadlock or its absence or automatically raise --timeout.

Wake timers: the model can schedule its own wakeups with the wakein/wakeon
(one-shot) and loop (recurring) tools. A ONE-SHOT timer holds this process
open: the run waits until it fires, reacts to the wake_fired event, and
exits as usual (--timeout, the 6h cap, Ctrl-C and "sessions cancel" still
stop the wait: owned deadlines end "timeout", caller cancellation "canceled";
an overdue timer is not caught up). An endless loop schedule never holds a non-interactive run open: when
the scope closes with no other open work, the session's loop schedules are
cancelled at run end (one stderr line and a --json warnings entry), while a
Ctrl-C/--timeout exit leaves them in the DB to fire when any rush process
of that data directory runs.

== Hooks ==
Flags: --on-finish

Lifecycle hooks:
  --on-finish "cmd"   run a shell command after the agent finishes.
                      Env vars set: RUSH_SESSION_ID, RUSH_EXIT_REASON,
                      RUSH_COST_USD, RUSH_TOKENS, RUSH_DURATION_SEC.

== Git-worktree isolation ==
Flags: --worktree, --base, --branch; also "rush worktree list" / "rush worktree remove"

Git-worktree isolation:
  --worktree <slug> [--base <ref>] [--branch <name>]
                     resolves the MAIN checkout (works when rush
                     itself runs inside a linked worktree), creates
                     <main>/worktrees/<slug> on branch <branch|slug>
                     from <base> (default: the main checkout's HEAD),
                     and runs this invocation with cwd inside the
                     worktree. An existing registered worktree whose
                     branch matches is reused and announced; a path
                     occupied by anything else, or a branch mismatch,
                     is a clean error. The slug is a single path
                     segment ([a-z0-9][a-z0-9._-]*, no ".."), so a
                     worktree can only ever be created under
                     <main>/worktrees/. Without --session the session
                     id is the slug. The run's stdout (the --json
                     envelope, or the terse final answer) and stderr
                     are ALSO written to <main>/.rush/stdin/<session>.out
                     and .err so results survive ` + "`rush worktree remove`" + `;
                     shell redirection keeps working as before. If
                     worktrees/ is not git-ignored a warning is printed
                     to stderr (rush never edits .gitignore).
                     --worktree is incompatible with --cwd and
                     --data-dir. Lifecycle: ` + "`rush worktree list`" + `
                     and ` + "`rush worktree remove <slug>" + ` [--keep-branch]
                     [--force]` + "`" + `.` + `

== Permissions and MCP ==
Flags: --all-mcp, --allow-peak-hours, --no-supervision, --supervision-interval

Permissions: non-interactive runs auto-approve every permission request
(no one is on the keyboard to confirm). The agent gets the full tool
set with no prompting. This is fast but irreversible — only run
"rush run"" in a workspace whose contents you can afford to lose, and
prefer --cwd /some/sandbox-or-temp-dir for one-shot scripts.

Restricted-run allowlist (scoped permissions, configured in rush.json):
  permissions.run          flip from auto-approve-everything to
                           deny-by-default: only allowlist matches are
                           approved, everything else is denied cleanly.
                           Arm it with permissions.run.restrict=true in
                           rush.json. There are no CLI flags for this —
                           per-run permission flags were removed, and
                           further run-permission management is planned
                           through roles, not the command line.
  run.allow_tools          non-bash tools or tool:action entries the run
                           may use, e.g. 'view' or 'edit:write'. NOTE:
                           'bash' / 'bash:execute' are ignored here —
                           command-level control of the bash tool is
                           run.allow_bash only.
  run.allow_bash           bash command patterns the run may use. Forms:
                             cmd args    word-boundary prefix
                                         (e.g. 'git diff' matches
                                         'git diff HEAD~1' but refuses
                                         'ls && rm -rf /')
                             exact:cmd   whole-string equality
                             glob:pat    filepath.Match glob
                             regex:pat   regexp on the command

  Config equivalent (rush.json):
    "permissions": {
      "run": {
        "restrict": true,
        "allow_tools": ["view"],
        "allow_bash": ["git diff", "glob:ls *"]
      }
    }
  Precedence: permissions.allowed_tools is a global bypass that still
  wins over both lists (it is checked before the gate). Listing 'bash'
  there approves EVERY bash command; for command-level control, leave
  allowed_tools empty for bash and use run.allow_bash.
  Restricted mode never affects interactive TUI / web sessions.

Peak-hours override:
  --allow-peak-hours  bypass a provider's configured peak_hours refusal for
                      this single invocation. No persistent config-level
                      equivalent exists; the override is conscious and
                      one-off by design.
                      WARNING: this flag must NEVER be added by an
                      orchestrating agent on its own initiative. Only pass
                      --allow-peak-hours when a human operator has explicitly
                      asked, in this specific request, to override peak hours.
                      An agent that adds this flag without an explicit human
                      instruction is violating the operator's intent.

MCP servers: by default only servers with mcp.<id>.enabled_in_cli=true
in rush.json are started for "rush run"" (and every other non-web CLI
subcommand) — most MCP servers add nothing to an unattended agent while
still paying real startup cost (a stdio server spawns its own child
process every invocation). Interactive "rush"" (the web UI) is
unaffected and always starts every non-disabled server.
  --all-mcp   start every configured, non-disabled MCP server for this
              run, ignoring enabled_in_cli. Use when this specific run
              genuinely needs an MCP server rush would otherwise skip.
`
const runExample = `
# Run a simple prompt
rush run --role smart "Guess my 5 favorite Pokémon"

# Pipe input from stdin (stdin is prepended to the args prompt)
curl https://charm.land | rush run --role smart "Summarize this website"

# Read the prompt from a file
rush run --role smart < prompt.md

# Redirect output to a file
rush run --role smart "Generate a hot README for this project" > MY_HOT_README.md

# Quiet mode (hide the spinner) / verbose mode (show logs)
rush run --quiet --role smart "Generate a README for this project"
rush run --verbose --role smart "Generate a README for this project"

# Continue a previous session by id (or hash prefix)
rush run --role smart --session {session-id} "Follow up on your last response"

# Continue the most recent session
rush run --role smart --continue "Follow up on your last response"

# Idempotent CI: same id across runs continues the same conversation;
# the first run creates it. Use a stable key like a PR number.
rush run --role smart --session "pr-42" "Review the latest changes"

# Override the session's system prompt from a flag
rush run --role smart --system-prompt "You are a terse senior reviewer." \
          --session "pr-42" "Review the latest changes"

# Or from a file (mutually exclusive with --system-prompt)
rush run --role smart --system-prompt-file ./reviewer-prompt.md \
          --session "pr-42" "Review the latest changes"

# Stdin user-prompt + file system-prompt + stable session id — the three
# inputs are independent, so this works as one pipeline:
git diff HEAD~1 | rush run --role smart --system-prompt-file ./reviewer-prompt.md \
                            --session "pr-42" \
                            "Review this diff"

# Required role flag — pick the strong model
rush run --role smart "design a migration for X"

# …or the cheap one for a quick one-liner
rush run --role fast "tldr this readme" < README.md

# Watch the agent think token-by-token (legacy output mode)
rush run --role smart --stream "explain this codebase"

# Runaway protection: cap cost and tokens (persisted to session budget)
rush run --role smart --max-cost 0.50 --max-tokens 100k "refactor storage"
# After run: rush sessions show <id> → "Cost: $0.32 / $0.50 budget (64%)"

# On-finish hook (env: RUSH_SESSION_ID, RUSH_EXIT_REASON, RUSH_COST_USD, ...)
rush run --role smart --on-finish "echo done >> /tmp/log" "analyze codebase"

# Auto-loaded repo-default system prompt (from .rush/system-prompts/default.md)
rush run --role smart --session "pr-42" "review this PR"
# → auto-loads scope-control / summary-required rules without --system-prompt-file

# Machine-readable summary for wrapper scripts
rush run --json --role smart --session "pr-42" "review the diff" | jq .final_text

# Force raw JSON in final_text (model is instructed AND envelope is
# post-stripped; markdown fences and prose preamble go to
# assistant_notes).
rush run --role smart --json --format json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# JSON conforming to a schema
rush run --role smart --json \
          --format json-schema:./schemas/audit.json \
          --session "audit-A" "..." < /tmp/audit-prompt.md

# Disable sub-agent fan-out (the "agent" tool is removed from the
# toolset entirely so the model cannot dispatch even if it wants to).
rush run --role smart --agents single --session "linear-task" "..."

# Tell the model to fan out (system-prompt nudge — still up to the
# model whether it actually decomposes the work).
rush run --role smart --agents with-agents --session "parallel-audit" "..."

# Fan-out and recover every sub-agent's full text via the envelope.
# Parent's final_text becomes a brief wrap-up; sub_agent_outputs
# array carries the verbatim sub-agent replies.
rush run --role smart --json --agents with-agents --aggregation attach \
          --session "structured-audit" < /tmp/p.txt > /tmp/audit.json

# Orchestrator mode: with a worker model configured (models.worker in
# rush.json / web UI), --role smart on its own is enough — the default
# sub-agent ban is lifted, sub-agents get a work-capable toolset and run
# on the cheap worker slot, and the smart agent's edit/multiedit/write
# tools are removed so hands-on work has to be delegated.
rush run --role smart --session "orchestrated-refactor" "..."

# Bypass the strongest slot for an explicit one-off review (never picked
# automatically — must be requested by role name).
rush run --role reviewer --session "pr-42-review" "review this diff"

# Fan-out but keep everything in final_text verbatim (one big string).
rush run --role smart --json --agents with-agents --aggregation concat \
          --session "flat-audit" < /tmp/p.txt > /tmp/audit.json

# Scope an unattended CI run via rush.json (there are no CLI flags for
# this): "permissions": { "run": { "restrict": true, "allow_tools":
# ["view"], "allow_bash": ["git diff", "glob:go *"] } } — only view and
# the listed bash commands are approved; the rest is denied cleanly
# (no UI to hang on).
rush run --role fast --session "ci-123" "summarize the diff"

# Rarely needed: a run has no --timeout deadline by default (--idle-timeout ends
# it after 15m of inactivity; a graceful 6h wall-clock cap ends a very long
# wait). --timeout is for fitting a hard external deadline, e.g. a CI job
# slot, or for waiting longer than that cap.
rush run --role smart --timeout 5m --session "long-task" "refactor the storage layer"

# One command instead of the manual chain: create/reuse an isolated
# worktree, run inside it, and mirror the results into the main
# checkout's .rush/stdin (they survive worktree removal).
rush run --role smart --worktree "task-a" --json "refactor the parser" \
          > /tmp/out.json 2>/tmp/out.err
rush worktree list
rush worktree remove "task-a"
  `
