# Responses WebSocket transport

Operator guide to the WebSocket transport for the OpenAI Responses protocol:
how it is selected, the failure and fallback contract, the diagnostics that
make a WebSocket turn comparable with an HTTP turn, the delta-chain rules,
and a live-smoke checklist for a human to run.

Code: `internal/nettransport/responses_ws*.go` (connections, manager, delta
chaining), `internal/agent/codexprovider/provider_websocket*.go` (direct
Codex), `internal/agent/public_responses_websocket.go` (public OpenAI),
`internal/agent/coordinator_responses_ws.go` (wiring and policy).

## 1. Selection and configuration

Two providers can use the transport. Nothing else does.

| Provider | Eligible when |
|---|---|
| `openai-codex` (ChatGPT OAuth) | a streaming call with a session ID, not a title-generation call |
| `openai` (public API key) | provider ID `openai`, API key auth (no OAuth), default base URL `https://api.openai.com/v1`, a streaming call (`Stream`/`StreamObject`) with a session ID, `POST /v1/responses`, request body at most 8 MiB |

Setting:

```json
{
  "options":   { "responses_transport": "auto" },
  "providers": { "openai-codex": { "responses_transport": "http" } }
}
```

- Values: `auto` (default) or `http`. Anything else is rejected at load time.
- A provider's value overrides `options.responses_transport`.
- `http` never opens a WebSocket and retires any idle connection of the
  conversation. `auto` uses WebSocket when eligible and falls back to HTTP as
  described in section 2.
- The connection honours the same `network` settings (proxy, custom DNS/DoH)
  as the HTTP client of that provider. A change to credentials, network
  settings or the transport value retires the connection before the next
  exchange.

Connection management (`ResponsesWSManager`):

- One connection per conversation (the session ID), used sequentially. A busy
  conversation does not share its connection; the second caller goes to HTTP.
- At most 16 connections per process. At capacity the least recently used idle
  one is retired; if all are busy the call goes to HTTP.
- Idle connections are retired after 90 s, and every connection after 20 min
  (a local reuse budget, not a claim about the server's lifetime).
- A handshake rejected with HTTP 404, 405 or 426 marks the endpoint
  unsupported for 5 min; calls in that window go to HTTP without dialing.
- A handshake answered 401 triggers one credential refresh (Codex) and one
  retry of the handshake; a second 401 is an error, not a fallback.

## 2. Failure and fallback contract

The contract hinges on one fact: **has the request frame possibly reached the
server?** The transport tracks it as the send state
(`not_sent`, `possibly_sent`, `streaming`, `completed`).

| Situation | Result |
|---|---|
| Not eligible, policy `http`, endpoint unsupported, manager busy/at capacity, handshake failed (not 401), credentials or policy changed between decision and send, frame could not be written because of a pre-send check | Silent HTTP request (reason is logged, section 3). Nothing was sent, so HTTP cannot duplicate work. |
| Frame possibly sent, then the connection fails, times out or the context is cancelled | Error surfaced to the caller, marked no-replay. Neither Fantasy's retry nor the coordinator nor HTTP may re-send it. The connection is closed. |
| Server error event or `response.failed` after output was delivered | Error surfaced, no replay. |
| Server error event before any output (rate limit, usage limit, overload, uncertified context error) | Provider error is visible for classification but stays no-replay on the wire. |
| Certified context-length rejection before any output | Replayable: the connection is retired and the agent recovers once with a full frame (compaction path). |
| **Chained frame rejected as `previous_response_not_found` before any output** | **Recovered inside the transport:** the connection is retired and the same request is resent once as a full frame on a fresh connection. If that also fails, the error surfaces as no-replay. Never recovered after output, never for a full frame, never twice. |
| Response `incomplete`, or the consumer declined the finish, or the context was cancelled before the terminal event was accepted | The connection is closed, not reused, and its chain state is dropped. |

A connection is reused only after an accepted terminal `response.completed`.
Everything else retires it, so a stale parent can never be chained.

## 3. Diagnostics

All records are `slog` debug records; run with `rush --debug`. They go to
`logs/rush.log` under the data directory (see `rush logs`). Sessions appear
only as `session_hash` (the same hash `agent: prompt cache observation`
uses). Credentials, prompts and frame bodies are never logged.

### 3.1 One comparable record per model call

`responses transport exchange` is emitted once per Codex call, for both
transports, and once per public-OpenAI call that reaches the bridge.

| Field | Meaning |
|---|---|
| `provider`, `model`, `session_hash` | identity; Codex only for `model` |
| `transport` | `websocket` or `http` |
| `http_fallback_reason` | why an eligible call used HTTP: `policy_http`, `no_conversation`, `frame_encode`, `frame_too_large`, `ineligible_request`, `no_authorization`, `acquire_failed`, `credentials_or_policy_changed`, `exchange_state`, `send_not_sent` |
| `chain_recoveries` | 1 if a rejected chained frame was recovered with a full frame |
| `outcome`, `has_usage`, `input_tokens`, `cache_read_tokens`, `elapsed_ms` | Codex only. `cache_read_tokens` 0 means "reported zero" only when `has_usage` is true |
| `frame_mode`, `frame_reason`, `frame_full_bytes`, `frame_wire_bytes`, `input_items`, `wire_items`, `diverged_at` | WebSocket only, from the frame decision (3.2) |
| `exchange_seq`, `conn_age_ms`, `conn_delta_exchanges`, `conn_full_bytes_total`, `conn_wire_bytes_total` | WebSocket only. `exchange_seq` above 1 means the connection was reused; the totals give the bytes saved by chaining so far |

To compare a WebSocket turn with an HTTP turn of the same conversation,
filter on `session_hash` and compare `transport`, `elapsed_ms`,
`input_tokens`/`cache_read_tokens`. For token and prompt-prefix evidence on
the public provider use the existing `agent: prompt cache observation`
record (fields `prefix_fingerprint`, `tools_fingerprint`, `cached_tokens`)
joined on `session_hash` and time; that record is transport-agnostic.

### 3.2 Frame decision

Every WebSocket send logs `responses websocket frame sent` with the same
`frame_*` fields. `frame_mode` is `full` or `delta`; for a full frame
`frame_reason` says why chaining was not used:

| `frame_reason` | Meaning |
|---|---|
| `first_exchange` | first frame on this connection |
| `no_parent` | the previous exchange was not accepted or its state was dropped |
| `options_changed` | model, tools, instructions, reasoning or any non-`input` field differs |
| `history_diverged` | the request is not exactly the previous request plus the previous output; `diverged_at` is the first differing history index (equal to the item count when the history shrank) |
| `not_smaller` | the chained frame would not be shorter |
| `over_budget` | request over 1 MiB or 256 input items |
| `ineligible_request` | `store` is not `false`, a field that forbids chaining is present (`generate`, `context_management`, `stream_id`, `background`, `truncation`, `prompt_cache_retention`, `safety_identifier`, `metadata`), or a client-set `previous_response_id` |
| `frame_too_large` | frame over 1 MiB |
| `unparseable` | not JSON the chainer accepts (duplicate keys, invalid escapes, excessive depth) |
| `delta_disabled` | protocol or `OpenAI-Beta` header not eligible for chaining |

Other connection records: `responses websocket exchange completed`
(adds `exchange_ms`), `responses websocket exchange failed` (adds `stage`,
`send_state`, `close_status`), `responses websocket retired`,
`responses websocket chain rejected before output, retrying with a full frame`,
`responses transport: websocket unavailable, using http` (adds the acquire
error text, which contains no credentials).

## 4. Delta chaining

A chained frame sends only the new input items plus `previous_response_id`.
It is built only when all of these hold:

- the previous exchange on the same connection ended in an accepted
  `response.completed` with a response ID and an output list consistent with
  the `response.output_item.done` events seen;
- every non-`input` field is canonically identical;
- the new `input` starts with exactly the previous `input` followed by exactly
  the previous output items, compared as canonical JSON (numbers and strings
  are preserved byte for byte, so output IDs and encrypted reasoning must be
  replayed verbatim);
- Codex only: `OpenAI-Beta: responses_websockets=2026-02-06` and `stream:true`.

Consequences worth knowing: the direct Codex encoder drops reasoning items
and item IDs from replayed history, so its chain typically diverges at the
first assistant output and Codex turns send full frames
(`frame_reason=history_diverged`). That is correct and safe; the diagnostics
exist to make it visible. A reconnect always starts with a full frame
(`first_exchange`).

## 5. Live-smoke checklist (human, not automated)

Not run by CI and not run by agents. Provider quota for the shared accounts
is exhausted until 2026-11-16; do not start before that date.

Preparation

- [ ] Use throwaway state: set `RUSH_GLOBAL_DATA` and `RUSH_GLOBAL_CONFIG` to
  empty temp directories, or a project-local `rush.json`; never your real
  global config.
- [ ] Log in to the provider under test (Codex OAuth, and separately an
  OpenAI API key for `openai`).
- [ ] Start every run with `rush --debug`. Note the log path
  (`<data dir>/logs/rush.log`).

A. Baseline over HTTP

- [ ] Set `responses_transport: "http"`. Run a 3-step task that uses a tool
  twice. Expect `transport=http`, `http_fallback_reason=policy_http`, and no
  `responses websocket` records.

B. WebSocket parity

- [ ] Set `auto`, run the same task in a new session. Expect
  `transport=websocket` on every call, `exchange_seq` 1, 2, 3 on one
  connection, `frame_reason=first_exchange` then `history_diverged` or
  `delta` with the reason you can explain from section 3.2.
- [ ] Compare answer quality, `elapsed_ms`, and `cache_read_tokens`
  (A vs B). Record both in the smoke report. WebSocket must not lose cache
  reads relative to HTTP on the later steps.
- [ ] `grep -c "access_token\|Bearer\|sk-" logs/rush.log` returns 0 for the
  run's lines (no credentials in diagnostics).

C. Cancellation and reconnect

- [ ] Cancel a turn mid-stream. The next prompt in the same session works,
  logs `frame_reason=first_exchange` and `exchange_seq=1` (new connection),
  and the cancelled turn was not replayed (one request for it in the log).
- [ ] Disconnect the network during a turn, restore it, send another prompt:
  the failed turn errors once (no automatic resend), the next prompt works.

D. Idle reuse (decides a backlog item)

- [ ] Finish a turn, wait 30 s, send another; then wait 3 min and again.
  Record whether the later turn reuses the connection (`exchange_seq`
  continues) or fails on a stale connection. Attach the log lines. A failure
  here is evidence for the "stale idle connection" backlog item in the
  tracker.

E. Credentials and policy

- [ ] During a session, switch to `responses_transport: "http"`. The next
  call logs `policy_http` and works. Switch back; the next call reconnects
  (`first_exchange`).
- [ ] Log out of Codex mid-session. The next call fails with a credentials
  error and does not fall back to a cached token.

F. Fallback

- [ ] Point the session at a conversation already busy in another process
  or block `chatgpt.com` WebSocket upgrades at a proxy. Expect an HTTP call
  with `http_fallback_reason=acquire_failed` and a working answer.

G. Chain rejection (only if it happens)

- [ ] If any log contains
  `responses websocket chain rejected before output, retrying with a full
  frame`, check the same call's `responses transport exchange` record: it
  must show `chain_recoveries=1`, `frame_mode=full`, `exchange_seq=1` and
  the turn must have completed without an error shown to the user. Attach
  the lines; this is the first live evidence for that path.

Pass criteria: B parity, C no replay, E honoured, no credentials in logs.
D and G only produce evidence.
