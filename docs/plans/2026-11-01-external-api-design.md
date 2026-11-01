# External API: authenticated REST and WebSocket channels

Status: design, nothing implemented. Written 2026-11-01. Tasks: API-1 .. API-10 in the task list.

## 1. Goal

Let a remote program (CI job, orchestrator, another agent host, a script) drive
Rush over the network, authenticating with a token carried in a request header,
over four channels:

| Channel | Transport | Scheme | Authentication |
|---|---|---|---|
| C1 | HTTP REST (+ SSE for streams) | `http://` | `Authorization: Bearer <token>` |
| C2 | WebSocket | `ws://` | `Authorization: Bearer <token>` (or subprotocol, see 7) |
| C3 | C1 over TLS | `https://` | bearer + server certificate |
| C4 | C2 over TLS | `wss://` | bearer + server certificate |

One listener serves the REST surface and the WebSocket surface; TLS on or off
decides between C1/C2 and C3/C4. "Certificate" means the TLS server certificate
(decision D3 below); verifying client certificates (mTLS) is an optional add-on
on top of the bearer token, never a replacement for it.

Non-goals for v1: OAuth/OIDC, per-request provider credentials (the SDK's
`RunWithCredentials` stays SDK-only), multi-tenant isolation between tokens,
remote use of the web UI, ACME/automatic public certificates.

## 2. What exists today (facts, internal/server and internal/cmd)

- `rush web` starts one HTTP server (`server.go`): routes `/auth`, `/auth/check`,
  `/ws` (guarded) and the embedded React UI. There is NO REST API; all work goes
  through the ~70 WebSocket commands in `protocol.go`.
- One random 160-bit token per process start (`auth.go`), printed on the terminal.
  Accepted from the session cookie, `Authorization: Bearer`, or `?token=`. Any
  holder has full control; there are no scopes, no expiry, no revocation, no
  persistence, no rate limit on failed attempts.
- No TLS anywhere: `srv.Serve(ln)` is plain. The default bind is `localhost`;
  `--host 0.0.0.0` is the only exposure switch and it exposes cleartext.
- WebSocket `CheckOrigin` accepts an empty Origin (non-browser clients) and
  browser origins only from localhost on the bound port.
- Web sessions auto-approve every tool permission (`autoApproveWebSession`,
  handlers.go). Anyone who can send a prompt can therefore run shell commands
  and edit files as the operator. Exposing the API makes this a remote code
  execution surface, which drives most of the design below.
- Dangerous WebSocket commands exist: `shutdown_server`, `set_provider_key`,
  `add_mcp_server`/`update_mcp_server` (spawns a process), `add_context_path`,
  `initialize_project`, `add_custom_provider`.

## 3. Threat model

A valid token is equivalent to a shell as the operator. Therefore:

1. Default is loopback, authenticated, least privilege.
2. A token carries scopes; unmapped commands are denied (fail closed).
3. Cleartext (C1/C2) on a non-loopback address needs an explicit flag and warns
   at every start; the UI surface is never served on a non-loopback listener.
4. Tokens are stored hashed, shown once, revocable, optionally expiring.
5. Tool permissions for API-originated sessions are NOT auto-approved by default.
6. Everything is audited; secrets never reach logs.

## 4. Tokens

- Format `rush_<id>.<secret>`: `id` 8 hex chars (lookup, safe to log), `secret`
  43 chars base64url of 32 random bytes. The `rush_` prefix makes leaks
  greppable by secret scanners.
- Stored: `id`, name, scopes, permission mode (see 6), created, expires (nullable),
  revoked_at, last_used (coalesced, at most one write per minute), SHA-256 of
  the secret. SHA-256 is sufficient because the secret has 256 bits of entropy;
  comparison is constant time. The plaintext is printed once at creation.
- CLI: `rush tokens create --name ci --scope run --expires 30d [--permission ask]`,
  `rush tokens list`, `rush tokens revoke <id>`. Works while the server runs
  (separate process, shared store).
- The existing per-process UI token keeps its behaviour and is accepted ONLY on
  the loopback UI listener; it is never valid on an external listener.
- Storage (decision D2): a table `api_tokens` in the existing SQLite store
  (one new migration). Alternative: a 0600 file with the config layer's sidecar
  lock. DB is recommended: multi-process safe, transactional revoke, no
  new file format. A migration needs the operator's OK.

## 5. Scopes and command authorization

| Scope | Allows |
|---|---|
| `read` | health, version, list/get sessions, load messages, models, live-work/batch status, event streams |
| `run` | `read` + create/fork/rename/delete own-root sessions, send, inject, interrupt, cancel, rerun, summarize, permission decisions |
| `config` | `run` + models per session/scope, system prompt, skills, theme-like settings |
| `admin` | everything, including provider keys, MCP servers, context paths, project init, shutdown |

Implementation: one table `command -> scope` next to `protocol.go`, consulted in
the WebSocket dispatcher and by REST routes. An exhaustiveness test enumerates
every `Cmd*` constant and fails if one has no entry; the default for an unmapped
command is `admin`. Commands that spawn processes or expose files or secrets
(`shutdown_server`, `set_provider_key`, `remove_provider_key`, `add_mcp_server`,
`update_mcp_server`, `add_context_path`, `initialize_project`,
`add_custom_provider`, `update_custom_provider`) are admin-only AND rejected on
external listeners unless the listener sets `allow_admin: true`.

## 6. Tool permissions for API sessions

Per-token `permission` setting, default `ask`:

- `ask`: a tool call that needs permission pauses the run and emits a
  `permission_request` event (SSE and WS). The client answers with
  `POST /api/v1/permissions/{id}` (`allow`, `allow_session`, `deny`) or the WS
  equivalent. No answer within `permission_timeout` (default 5 min) = deny.
- `readonly`: only a fixed allowlist of read-only tools; everything else is denied
  without prompting.
- `auto`: today's web behaviour. Allowed only for tokens with `admin` scope, or
  when the listener sets `allow_auto_approve: true`. Both are logged at start.

Sessions created through the API run inside the listener's configured
`workdir_roots`; a request naming a path outside them is rejected (`400`, no
traversal via `..` or symlinks; resolved with `EvalSymlinks` before the check).

## 7. REST surface v1

Base path `/api/v1`, JSON bodies, errors as RFC 7807 `application/problem+json`,
cursor pagination (`?after=<id>&limit=`), request ids in `X-Request-Id`.

| Method and path | Scope |
|---|---|
| `GET /health`, `GET /version` | none (health) / read |
| `GET /sessions`, `GET /sessions/{id}` | read |
| `POST /sessions` | run |
| `POST /sessions/{id}/fork`, `DELETE /sessions/{id}` | run |
| `GET /sessions/{id}/messages?after=` | read |
| `POST /sessions/{id}/runs` (prompt, role, model override, `Idempotency-Key`) -> `202 {run_id}` | run |
| `GET /runs/{id}` | read |
| `POST /runs/{id}/cancel` | run |
| `POST /sessions/{id}/inject` (`merge` or `interrupt`) | run |
| `GET /sessions/{id}/events` (SSE; `Last-Event-ID` replay) | read |
| `POST /permissions/{id}` | run |
| `GET /models` | read |
| `GET /openapi.json` | read |

Runs are asynchronous by design (agent turns last minutes): 202 plus polling or
SSE. `Prefer: wait=<seconds>` may block up to a server cap for short runs. The
OpenAPI document is generated from the route table and a contract test fails
when a route and the document disagree.

## 8. WebSocket channel

Same `/ws` endpoint and command protocol as the UI, with:

- Auth by `Authorization: Bearer` on the upgrade request. Browsers cannot set it,
  so also accept `Sec-WebSocket-Protocol: rush.bearer.<token>` (the server echoes
  the subprotocol `rush.v1`). `?token=` is accepted only on the loopback UI
  listener, never on external ones (URLs end up in proxy and access logs).
- Origin: empty Origin allowed (non-browser); browser origins only from the
  listener's `allowed_origins` list (default empty on external listeners).
- Scope enforcement per command (section 5); a denied command returns an error
  envelope and keeps the connection.
- Per-token cap on concurrent connections; a `hello` frame carrying the protocol
  version, token id and scopes; revoking a token closes its sockets within one
  ping interval.

## 9. TLS (C3/C4)

- `--tls-cert`, `--tls-key` (PEM). The files are re-read when their mtime
  changes (poll every 60 s; no SIGHUP, Windows has none); a bad reload keeps the
  previous certificate and logs an error.
- `--tls-self-signed`: generate an ECDSA P-256 certificate on first start into
  the data directory (SAN from `--tls-san`, default localhost and bound IPs,
  1 year), print its SHA-256 fingerprint for pinning.
- TLS 1.2 minimum, 1.3 preferred, Go's default modern cipher suites, no
  renegotiation, `http/1.1` and `h2` ALPN.
- mTLS: `--client-ca <pem>` plus `--require-client-cert` rejects peers without a
  valid client certificate before any HTTP is parsed. The bearer token is still
  required (two independent factors).
- No plain-HTTP redirect listener.

## 10. Listeners and CLI

New headless command `rush serve` (decision D1; `rush web` is unchanged and
stays loopback UI). Config key `server.listeners[]` (Ktav-capable like the rest):

```
addr: 127.0.0.1:8443
surfaces: [rest, ws]          # ui only allowed on loopback
tls: {cert: ..., key: ..., client_ca: ..., require_client_cert: false}
allow_cleartext: false        # needed for C1/C2 on a non-loopback address
allow_admin: false
allow_auto_approve: false
allowed_origins: []
workdir_roots: [/srv/work]
limits: {rps: 20, burst: 40, max_conns_per_token: 8, max_body: 1MiB}
```

Flags override the first listener for quick use. Startup refuses: non-loopback
without TLS and without `allow_cleartext`; `ui` on non-loopback; a listener with
no valid token and no way to create one (prints how to run `rush tokens create`).

## 11. Abuse protection and audit

- Failed auth: uniform `401` with `WWW-Authenticate: Bearer`, no oracle for
  "id exists", exponential back-off per source IP after 10 failures in a minute.
- Token-bucket rate limit per token and per IP; request body cap; header and
  read timeouts; slow-loris safe (`ReadHeaderTimeout`).
- Audit line per request: token id, scope used, method, route template, status,
  session id, duration, remote address. Tokens, Authorization headers and
  `?token=` values are redacted by one central helper; a test greps the log of a
  full end-to-end run for the plaintext token and fails if it appears.

## 12. Test plan

- Token store: create/parse/verify, expiry, revoke while connected, constant-time
  comparison, concurrent creation, hash never equals secret.
- Scope table exhaustiveness (all `Cmd*`), fail-closed default, admin-only gating
  on external listeners (mutant: flip the default to `read`).
- Four-channel matrix: one test body run against plain HTTP, plain WS, TLS HTTP,
  TLS WS (httptest servers, gorilla dialer), plus mTLS accept and reject, TLS 1.1
  client rejected, expired and wrong-SAN certificate rejected by a verifying
  client, certificate hot reload.
- REST contract: OpenAPI vs routes, problem+json shape, idempotency replay,
  pagination, SSE replay by `Last-Event-ID`, permission ask/deny/timeout.
- Security: token redaction in logs, no `?token=` on external listeners,
  Origin checks, brute-force back-off, oversize body, path-root escape attempts.

## 13. Work breakdown

| Task | Content | Depends on |
|---|---|---|
| API-1 | Token core, store, migration, `rush tokens` | D2 |
| API-2 | Listener model, `rush serve`, cleartext/loopback policy | - |
| API-3 | TLS: certs, reload, self-signed, mTLS | API-2 |
| API-4 | Auth middleware, scope table, rate limit, audit, redaction | API-1 |
| API-5 | REST skeleton, read endpoints, problem+json, OpenAPI | API-2, API-4 |
| API-6 | REST run endpoints, SSE, idempotency, workdir roots | API-5 |
| API-7 | Permission modes and decision endpoint | API-4, API-6 |
| API-8 | WS hardening: header/subprotocol auth, origins, limits, hello | API-4 |
| API-9 | Four-channel e2e matrix, security tests, mutants | API-3, API-6, API-7, API-8 |
| API-10 | Docs, OpenAPI file, client examples, CHANGELOG | API-6 |

Suggested agent batches: auth core (API-1 + API-4), listeners and TLS
(API-2 + API-3), REST (API-5 + API-6 + API-10), permissions and WS
(API-7 + API-8), then verification (API-9).

## 14. Decisions

- D1: new `rush serve` command (recommended) versus flags on `rush web`.
  A separate command keeps the loopback UI path unchanged and makes the exposed
  mode explicit.
- D2: token storage in SQLite with a new migration (recommended) versus a file.
- D3: "certificate" read as the TLS server certificate; mTLS stays optional.
- D4: default permission mode `ask`, `auto` only for admin tokens or an explicit
  listener flag.
- D5: remote web UI is out of scope; the UI surface is loopback only.
