# REST API

Base URL paths below are relative to `/api/v1`. Requests use JSON; errors use RFC 7807 `application/problem+json`. Responses carry `X-Request-Id`; clients may send an `Idempotency-Key` on supported writes. Bearer authentication is described in [auth.md](auth.md). `admin` implies lower scopes; `read` and `run` do not imply `config`.

## Routes

| Method | Path | Scope | Query / headers | Request → success response | Statuses / idempotency |
|---|---|---|---|---|---|
| GET | `/health` | none | — | `{}` → `{"status":"ok"}` | 200 |
| GET | `/version` | read | — | `{}` → `{"version":"…"}` | 200; build version varies |
| GET | `/openapi.json` | read | — | `{}` → generated OpenAPI document | 200 |
| GET | `/sessions` | read | `after` session ID, `limit` 1–200 (default 50) | `{}` → `{"items":[session…],"next_after":"…"}` (cursor may be null) | 200; 400 invalid cursor/limit |
| GET | `/sessions/{id}` | read | `id` path | `{}` → session projection | 200; 404 missing |
| GET | `/sessions/{id}/messages` | read | `after` message ID, `limit` 1–200 (default 50) | `{}` → `{"items":[message…],"next_after":"…"}` | 200; 400 invalid cursor/limit; 404 session missing |
| GET | `/models` | read | — | `{}` → array of `{id,name,provider,role?}` | 200 |
| POST | `/sessions` | run | optional `Idempotency-Key` | `{"title":"optional"}` (empty body allowed) → safe session projection; empty title defaults to `New Session` | 201 and `Location`; 400 malformed/unknown fields; 413 oversized; key replays equivalent request |
| POST | `/sessions/{id}/fork` | run | `id` path; optional `Idempotency-Key` | `{"title":"optional"}` → safe session projection | 201 and `Location`; 400, 404; key replays equivalent request |
| DELETE | `/sessions/{id}` | run | `id` path | empty → no body | 204; 404 missing/not owned; 409 busy; not idempotency-key protected |
| POST | `/sessions/{id}/runs` | run | `id` path; optional `Idempotency-Key` | `{"prompt":"text","role":"optional","model":"optional"}` → accepted run `{ "run_id":"…", "session_id":"…", "state":"running" }`; fetched run includes `id`, `session_id`, `state`, `created_at`, optional `started_at`, `finished_at`, `error:{code,summary}`, and `message_id` | 202 and `Location`; 400 invalid body/role/model; 404; 409 busy; 503 unavailable; idempotent replay uses same key and body |
| GET | `/runs/{id}` | read | `id` path; optional `wait=1..30` seconds | `{}` → run object with `id`, `session_id`, `state` (`queued`, `running`, `succeeded`, `failed`, `cancelled`, or `awaiting_answer`), `created_at`, optional `started_at`, `finished_at`, `error:{code,summary}`, and `message_id` | 200; 400 invalid wait; 404 missing/not owned |
| POST | `/runs/{id}/cancel` | run | `id` path | empty → current run state or `{"state":"…"}` | 200 if already inactive, 202 cancellation accepted; 404 |
| POST | `/sessions/{id}/inject` | run | `id` path | `{"message":"text","mode":"merge|interrupt"}` (mode defaults to `merge`) → `{"state":"accepted"}` | 202; 400 invalid; 404; 409 no active run |
| GET | `/sessions/{id}/events` | read | `id` path; `Last-Event-ID` header (or `last_event_id` query fallback) | SSE stream; see [sse.md](sse.md) | 200 stream; 400 invalid event ID; 404; 429 stream limit with `Retry-After: 5` |
| GET | `/permissions` | run | optional `session` filter (at most once) | JSON array of `{id,session_id,run_id,tool,action,description,path?,created_at}` | 200; 400 repeated `session`; 404 if broker/principal unavailable |
| POST | `/permissions/{id}` | run | `id` path | `{"decision":"allow|deny"}` → `{"decision":"allow|deny"}` | 200; 400 malformed/unsupported; 404 unknown or unavailable broker; 409 already decided. No idempotency-key handling. |

Session JSON contains `id`, `title`, `created_at`, `updated_at`, `message_count`. Message JSON contains `id`, `session_id`, `role`, `parts`, `created_at`; text, tool-call and tool-result parts are projected, binary attachments omitted. Times are RFC3339 UTC. Pagination sorts sessions by newest update then ID; message order follows store order. `after` must identify an item in that collection; unknown cursors return 400 rather than silently restarting. A page returns `next_after`, the final item ID when more results exist, otherwise null.

Run records expose `id`, `session_id`, `state`, and UTC `created_at`; `started_at` and `finished_at` appear when available, `error` (with `code` and `summary`) appears on an error, and `message_id` appears when supplied. The run registry is process-local, retains at most 1,000 runs, and expires terminal runs after 24 hours. Idempotency applies to run creation when `Idempotency-Key` is present: exactly one key of 1–128 ASCII letters, digits, `.`, `_`, `:`, or `-` is accepted. It is scoped per token and compares request body, method, and URL path. The same key/request replays a completed response (including status, `Content-Type`, `Location`, and body, with `Idempotent-Replayed: true`); completed responses including 4xx are retained, while concurrent use returns 409, reuse for a different request returns 422, and a full store returns 503. Entries are process-local and non-durable; completed entries may be evicted before 24 hours, so 24 hours is the retention ceiling, not a guarantee. Keys are optional; without one the request proceeds normally. These rules describe the shared idempotency middleware on supported writes, not DELETE or permission decisions.

`docs/api/openapi.json` is a snapshot of the document served at `GET /api/v1/openapi.json` (`info.version` is replaced by `build-version`, it embeds the build time). `TestOpenAPIDocsSnapshotMatchesServedDocument` fails when the routes change without the snapshot; regenerate it with `go test ./internal/server -run OpenAPIDocsSnapshot -args -update-openapi-docs`.

## Examples

Replace `BASE` with the listener origin and supply a token from a secure environment variable; never put a real token in source control or shell history. These examples assume routes are wired on the listener and the caller has the required scope.

```sh
curl --fail-with-body -H "Authorization: Bearer $RUSH_API_TOKEN" "$BASE/api/v1/sessions?limit=20"
curl --fail-with-body -H "Authorization: Bearer $RUSH_API_TOKEN" "$BASE/api/v1/runs/RUN_ID?wait=10"
curl --no-buffer -H "Authorization: Bearer $RUSH_API_TOKEN" -H 'Last-Event-ID: 42' "$BASE/api/v1/sessions/SESSION_ID/events"
```

Derivation: route declarations in `internal/server/api_routes*.go`; handler behavior and response serialization in `api_*handlers*.go` and `api_runs.go`; idempotency behavior in `api_idempotency.go`. The design §7 lists the core routes and §17.5 discusses intended follow-up fixes; this reference describes the observed implementation.