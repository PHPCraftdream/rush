# `sessions compact <session>`

## Syntax

```text
rush sessions compact <session> [--role worker|smart|fast|reviewer]
```

Passing one session ID compacts that conversation through the ordinary summary path. IDs may be supplied in full or as an unambiguous hash prefix. The summary role defaults to `worker`.

## Role selection and provider support

The requested role first uses that session's matching role override, then the configured role slot. If neither resolves to an available model, the command fails without falling back to another role. Standalone summaries support direct providers only. A role resolving to `local-cli` is refused fail-closed; this command does not gain native tools or bypass tool/authority permissions, and it does not fall back to another provider or role.

## Busy sessions and execution

The command verifies session activity, then uses the normal standalone `SummarizeNow` path, including mailbox admission and the OS session lock. Live or unreadable ownership state is refused. If the mailbox or OS lock is busy, compaction is refused immediately: it is not queued for later. Summary runs without tools and does not call `Run`.

Failure or cancellation is reported through the normal summary path. Original history is not removed until normal summary publication succeeds, so an unsuccessful summary leaves the prior history available. Provider limits still apply; the manual command does not preflight capacity or guarantee the summary will fit, so a provider may reject it.

Separately, during ordinary Codex runs (not this command), an exact certified context-overflow rejection before any output may trigger one automatic recovery. If the worker and capacity/budget checks permit, Rush silently summarizes eligible older history while preserving the context pin, original user message, and accepted tool-call tail, then retries once with the original model. Recovery is not guaranteed: a missing/unavailable worker, unsupported CLI worker, or failed safety check surfaces an error; a second overflow is terminal.

## Database mode remains separate

With no session argument, `rush sessions compact` retains its existing SQLite `VACUUM` behavior. Database-only flags (`--dry-run`, `--force`, and `--json`) are rejected when a session argument selects conversation compaction; `--role` only applies to conversation mode.
