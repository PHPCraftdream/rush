# Batch intake

A token with the `run` scope can hand the server work that runs without a conversation: a single prompt (`POST /api/v1/tasks`) or a batch of agent items (`POST /api/v1/batches`). The server runs it on the same batch engine as `rush batch run`, and the client polls for the result. Batches are not a chat: there is no session to talk to, and nothing is streamed over SSE in this version.

## Service session

Each token owns one hidden service session (`api-<token id>-<hash>`, origin `api_service`). Every batch the token submits belongs to it. The service session is never returned by `/sessions`, answers `404` on every session route (read, messages, runs, inject, events) and is never woken by batch events. A token sees and controls only its own batches: another token's root answers `404`.

## Routes

| Method | Path | Scope | Success |
|---|---|---|---|
| POST | `/batches` | run | `202` `{ "root_id", "state": "pending", "replayed"? }` and `Location` |
| POST | `/tasks` | run | `202`, same body: one `agent` item run as a sequential batch |
| GET | `/batches/{root}` | read | `200` batch with all nodes |
| GET | `/batches/{root}/nodes/{node}` | read | `200` one node |
| POST | `/batches/{root}/stop` | run | stop the whole root |
| POST | `/batches/{root}/nodes/{node}/stop` | run | stop one node |
| POST | `/batches/{root}/nodes/{node}/answer` | run | `202` `{ "node_id", "delivered": true }` |

`POST /tasks` takes `{"prompt": "…", "resume_session_id"?: "…", "timeout_seconds"?: n}` (unknown fields are refused). `POST /batches` takes the external batch schema of `rush batch run` (see `rush batch run --help`) and must be a batch, not a bare task.

A batch has `root_id`, `state`, `done`, `created_at`, `updated_at` and `nodes`. A node has `id`, `parent_id`, `kind` (`batch` or `task`), `ordinal`, `state`, `cause`, `tool`, the outcome fields (`outcome_state`, `outcome_code`, `outcome_summary`), `child_session_id`, `output`, `output_expired`, `awaiting_answer` and `question`. `done` is true once the root is in a terminal state (`completed`, `failed`, `cancelled`, `timed_out`, `interrupted`, `skipped`).

`output` is the live output of the node and is kept for ten minutes after the root finishes. Afterwards the node reports `output_expired: true` instead of silently dropping it; the outcome fields stay. Poll `GET /batches/{root}` until `done` is true.

## What a token may submit

- Without a batch binding a token may submit only the `agent` tool. A batch-bound token may in addition name exact extra tool names; wildcards are not supported. The whole tree is checked, so a forbidden tool nested at any depth is refused with `400` before any row is written.
- A `readonly` token may submit only read-only tools. It cannot submit `agent`, `agentic_fetch` or `mcp_*` tools, because the read-only filter does not reach sub-agents.
- At most 64 nodes and a depth of 3 per submission.
- Parallelism is forced by the server: a sequential batch runs one item at a time, a parallel batch takes the binding's `max_parallel` (default 2). The caller's value never raises it.
- Every item has a timeout: the default and the ceiling are 30 minutes.
- `resume_session_id` continues an existing child of this token's batches. A session of another token, the service session itself, an unrelated session or a missing one is refused with `400` (`not a child of this token's batches`).
- A token in `auto` permission mode runs its batch children without a prompt.

## Limits and back-pressure

- At most 2 live roots per token (a binding can set `max_roots`). Over the limit: `429` with `Retry-After: 5`.
- The engine accepts at most 8 running leaves across all tokens of one server; a submission that would exceed it also gets `429` with `Retry-After`. Retry later.
- Request bodies are capped like runs.

## Idempotency

`Idempotency-Key` (1–128 characters of letters, digits, `.`, `_`, `:`, `-`) makes a submission safe to retry. The key and a hash of the body are recorded in the root's submit reference, so a replay survives a server restart: the same key and body return the original root with `replayed: true`; the same key with a different body returns `422`. Keys are scoped per token. Without a key every submission is a new root.

## Questions from a running node

When a child asks a question, its node shows `awaiting_answer: true` and the `question` text. Answer it with `POST /batches/{root}/nodes/{node}/answer` and `{"text": "…"}`. The answer continues the same node execution; it does not start another one. Answering a node that has no current question is `409`.

## Statuses

`400` invalid body, forbidden tool, bad resume target, too many nodes or too deep; `404` unknown or foreign root/node; `409` no question to answer; `422` idempotency key reused with another body; `429` capacity (see above); `503` batch service unavailable.

## Not in this version

No SSE events for batches and no `rush api` client commands for them yet: poll `GET /batches/{root}`. Binding a token to batches from the command line (`rush tokens --batch`) is a follow-up; until then batch bindings come only from the token store.
