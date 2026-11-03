# API errors

API errors generally use `Content-Type: application/problem+json` and an RFC 7807-style object. `type` is `about:blank` in the generic problem writer; `title` and `status` identify the HTTP error. `detail` and `instance` are optional. Responses also carry `X-Request-Id` and `Cache-Control: no-store`. The generic `WriteAPIProblem` writer sanitizes status 500 or greater by forcing title `Internal Server Error` and omitting detail/instance. This is not a guarantee for every 5xx response: direct writers and middleware may construct their own bodies and titles.

Example:

```json
{"type":"about:blank","title":"Forbidden","status":403,"detail":"insufficient scope"}
```

## Error statuses and titles

The following are the status/title combinations explicitly returned by the current authentication middleware, REST handlers, route mux, and policy placeholders. Endpoint-specific details vary; the exact titles below come from the implementation.

| Status | Title | Typical cause |
|---|---|---|
| 400 | `Bad Request` | Invalid request body, query, permission decision, idempotency key, cursor, or WebSocket credentials |
| 401 | `Unauthorized` | Missing, malformed, expired, revoked, disallowed, or invalid token; includes `WWW-Authenticate: Bearer` |
| 403 | `Forbidden` | Insufficient scope or auto-approve disallowed by listener policy |
| 404 | `Not Found` | Unknown route, method path, session, run, or permission resource |
| 405 | `Method Not Allowed` | Known route with unsupported method; includes `Allow` |
| 409 | `Conflict` | Busy session/run, already-decided permission, active/in-progress idempotency request, or no active run |
| 413 | `Request Entity Too Large` (API handler) / `Request Too Large` (authentication middleware) | Request exceeds the relevant body limit |
| 422 | `Unprocessable Entity` | Idempotency key reused for a different request |
| 429 | `Too Many Requests` | Authentication backoff, rate/connection limit, or event-stream limit; includes `Retry-After` |
| 500 | `Internal Server Error` | Unexpected handler failure or panic |
| 501 | `Internal Server Error` | Route declaration's handler is not wired; the generic writer replaces all 5xx titles with `Internal Server Error` |
| 503 | `Service Unavailable` (auth middleware or direct run gate writer) / `Internal Server Error` (generic API writer) | Token verifier/store or run/permission/idempotency service unavailable; direct run gate response includes `detail: "permission gate unavailable"` |

Authentication middleware uses fixed `about:blank` bodies with these exact titles: `Bad Request`, `Unauthorized`, `Service Unavailable`, `Request Too Large`, and `Too Many Requests`. The generic REST `WriteAPIProblem` writer uses `Request Entity Too Large` for 413 and sanitizes status 500 and above, setting title `Internal Server Error` and omitting detail/instance. That behavior does not cover all 5xx paths: `writeAPIRunGateUnavailable` directly writes 503 with title `Service Unavailable` and detail `permission gate unavailable`. Placeholder handlers' passed titles `Not Implemented` and `Service Unavailable` are not the wire title when they use the generic writer. A missing API route produces 404 with an empty detail; resource misses use details such as `session not found`, `run not found`, and `permission not found`.

Some route declarations are placeholders and return `Not Implemented` or `Service Unavailable`; a declaration in the route table does not by itself mean that the operation is available. A 5xx response written directly by a handler or middleware may retain a service-specific title and detail, unlike responses passed through the generic problem writer. Do not rely on design-plan error types or status mappings unless present in the running implementation.

All responses include a generated or validated `X-Request-Id`. For retries, retain the same `Idempotency-Key` for the same logical run submission; a changed body under the same key returns 422.
