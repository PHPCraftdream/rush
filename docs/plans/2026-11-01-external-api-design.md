# External API: authenticated REST and WebSocket channels

Status: design, nothing implemented. Written 2026-11-01; revised the same day
after four independent two-consultant reviews (an `oh` agent and the rush
reviewer role answered each open decision separately; the outcomes are recorded
in section 14). Tasks: API-1 .. API-12 in the task list.

## 1. Goal

Let a remote program (CI job, orchestrator, another agent host, a script) drive
Rush over the network, authenticating with a token carried in a request header,
over four channels:

| Channel | Transport | Scheme | Authentication |
|---|---|---|---|
| C1 | HTTP REST (+ SSE for streams) | `http://` | `Authorization: Bearer <token>` |
| C2 | WebSocket | `ws://` | `Authorization: Bearer <token>` (or subprotocol, see 8) |
| C3 | C1 over TLS | `https://` | bearer + server certificate |
| C4 | C2 over TLS | `wss://` | bearer + server certificate |

One listener serves the REST surface and the WebSocket surface; TLS on or off
decides between C1/C2 and C3/C4. "Certificate" means the TLS SERVER certificate.
Verifying client certificates (mTLS) is an optional extra gate on top of the
bearer token, never a replacement for it (decision D3).

Non-goals for v1: OAuth/OIDC, per-request provider credentials (the SDK's
`RunWithCredentials` stays SDK-only), isolation between tokens beyond scopes,
remote use of the web UI, ACME/public certificates, serving several workspaces
from one process.

## 2. What exists today (facts, internal/server and internal/cmd)

- `rush web` and bare `rush` start one HTTP server (`server.go`): routes `/auth`,
  `/auth/check`, `/ws` (guarded) and the embedded React UI. There is NO REST API;
  all work goes through the ~70 WebSocket commands in `protocol.go`.
- One random 160-bit token per process start (`auth.go`), printed on the terminal
  and copied to the clipboard. Accepted from the session cookie, `Authorization:
  Bearer`, or `?token=`. Any holder has full control: no scopes, expiry,
  revocation, persistence or failed-attempt limit.
- No TLS anywhere: `srv.Serve(ln)` is plain. The default bind is `localhost`,
  but `rush web --host 0.0.0.0` (documented in `web.go`) already exposes a
  cleartext listener whose `/ws` rejects remote browsers (origin check) yet
  accepts any non-browser client with the weak token.
- Web sessions auto-approve every tool permission: `AutoApproveSession` is armed
  at six call sites (handlers_sessions.go, handlers_agent.go,
  handlers_agent_rerun.go) and lives in process memory. Anyone who can send a
  prompt can run shell commands and edit files as the operator, so exposing the
  API is a remote code execution surface; this drives most of the design.
- `rush web` and bare `rush` already differ: `mcpAppOptions` restricts MCP
  servers to `enabled_in_cli` only for subcommands (`root.go` ~477), so
  `rush web` starts a different MCP set than bare `rush`.
- One App serves exactly one workspace/data directory (`app.go` ~347).
- The signal handling of the CLI registers `os.Interrupt` only; `rush run`
  passes `os.Kill` where its comment says SIGTERM (`run.go` ~680).
- Dangerous WebSocket commands exist: `shutdown_server`, `set_provider_key`,
  `add_mcp_server`/`update_mcp_server` (spawns a process), `add_context_path`,
  `initialize_project`, `add_custom_provider`.

## 3. Threat model

A valid token is equivalent to a shell as the operator. Therefore:

1. Default is loopback, authenticated, least privilege.
2. A token carries scopes; unmapped commands are denied (fail closed).
3. Cleartext (C1/C2) on a non-loopback address needs an explicit flag and warns
   at every start; the UI surface does not exist in `rush serve`.
4. Tokens are stored hashed, shown once, revocable, optionally expiring, and the
   store location can never be chosen by a repository (section 4).
5. Tool permissions for API-originated sessions are NOT auto-approved by default.
6. Everything is audited; secrets never reach logs.

## 4. Tokens and their store (decision D2: a user-global file, not SQLite)

- Format `rush_<id>.<secret>`: `id` 8 hex chars (lookup, safe to log), `secret`
  43 chars base64url of 32 random bytes. The `rush_` prefix makes leaks
  greppable by secret scanners.
- Stored per token: `id`, name, scopes, permission mode (section 6), created,
  expires (nullable), revoked_at, and SHA-256 of the WHOLE token string (not only
  the secret, so a stored hash cannot be moved to another id). SHA-256 is enough
  because the secret has 256 bits of entropy; lookup by id, then constant-time
  comparison. The plaintext is printed once at creation, to stdout.
- Location: one versioned file `api-tokens.ktav` (or `.json`, following the
  Ktav-for-new-files rule) in the USER-GLOBAL data directory,
  `filepath.Dir(config.GlobalConfigData())`, honouring `--global-dir` and
  `RUSH_GLOBAL_DATA`. It is NEVER read from a project `.rush`, project or
  workspace config, `--data-dir` or `options.data_directory`: the session
  database is per project and its location can be supplied by a cloned
  repository, so a repository could otherwise ship its own `run`-scope token.
  A test fails if a token file placed in a project directory is ever consulted.
- Written with the config layer's primitives (sidecar lock, atomic commit, mode
  0600, user ownership on Unix); a standalone store type in package `config`,
  outside the ConfigStore reload machinery. On Windows `0600` and the owner check
  do nothing: the profile directory permissions are the protection, and the file
  ACL is verified with the Windows security API where practical.
- Read path: every REST request and every WS command checks the file's stat
  fingerprint and reloads when it changed; no time-based cache. An unreadable or
  malformed file denies every token (fail closed). Revoke takes effect on the
  next request; open WebSocket and SSE streams re-check on an explicit
  auth-recheck interval (default 15 s, not tied to the ping interval, which
  allows about 54 s), and a revoked or expired token closes its streams.
- `last_used` is telemetry, never authorization state: kept in memory, flushed at
  most once a minute to a separate `api-tokens.state` file (monotonic merge under
  its own lock), never on the request path, and it is also written to the audit
  log. The token file itself is not rewritten by use.
- CLI (store-only startup, no App, no database, no MCP): `rush tokens create
  --name ci --scope run --expires 30d [--permission ask]`, `list`, `show`,
  `revoke <id>`. It works while the server runs.
- Add-ons: a per-listener allow-list of token ids (`tokens: [id...]`), and an
  optional bootstrap token for containers: `RUSH_SERVE_TOKEN_SHA256=<id>:<hex>`
  with its scope in a separate variable (default `run`, never `admin`), read once
  and `os.Unsetenv`'d before any tool subprocess starts, kept in memory only.
- The per-process UI token keeps its behaviour for `rush web` and is never valid
  on a `rush serve` listener.
- No schema migration is involved, so there is no database decision for the
  operator and rollback to an older binary costs nothing.

## 5. Scopes and command authorization

| Scope | Allows |
|---|---|
| `read` | health, version, list/get sessions, load messages, models, live-work/batch status, event streams |
| `run` | `read` + create/fork/rename/delete sessions, send, inject, interrupt, cancel, rerun, summarize, permission decisions |
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

## 6. Tool permissions and workspace for API sessions

Per-token `permission` setting, default `ask` (decision D4):

- `ask`: a tool call that needs permission pauses the run and emits a
  `permission_request` event (SSE and WS). The client answers with
  `POST /api/v1/permissions/{id}` (`allow`, `allow_session`, `deny`) or the WS
  equivalent. No answer within `permission_timeout` (default 5 min) = deny.
- `readonly`: only a fixed allowlist of read-only tools; everything else is denied
  without prompting.
- `auto`: today's web behaviour. Allowed only for tokens with `admin` scope, or
  when the listener sets `allow_auto_approve: true`. Both are logged at start.

The six `AutoApproveSession` call sites must take the policy from the listener
(injected into `Server`), not from package globals; otherwise opening an API
session in the local UI of the same process would silently arm auto-approve for
it. Sessions created through the API carry a new origin `api`
(`message.OriginAPI`), next to cli, web and sdk. Durable API work must keep its
permission policy if another App claims it (release requirement).

One `rush serve` process is one App and one workspace: `serve` requires an
explicit `--cwd`, and a request cannot choose a working directory in v1 (there is
no `workdir_roots`). Shell tools are not confined by a root check, so the
workspace pin is a convenience, not a sandbox.

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
| `GET /runs/{id}` (and a wait variant with a server cap) | read |
| `POST /runs/{id}/cancel` | run |
| `POST /sessions/{id}/inject` (`merge` or `interrupt`) | run |
| `GET /sessions/{id}/events` (SSE with heartbeats; `Last-Event-ID` replay) | read |
| `POST /permissions/{id}` | run |
| `GET /models` | read |
| `GET /openapi.json` | read |

Runs are asynchronous by design: 202 plus polling or SSE. A remote run that stops
on a question reports `awaiting_answer` with the REMOTE resume command, never the
local `rush run --session` guidance (it would create a local session with the
same id). The OpenAPI document is generated from the route table and a contract
test fails when a route and the document disagree. The server needs no REST CORS
by default; browser use is out of scope.

## 8. WebSocket channel

Same `/ws` endpoint and command protocol as the UI, with:

- Auth by `Authorization: Bearer` on the upgrade request. Browsers cannot set it,
  so also accept `Sec-WebSocket-Protocol: rush.bearer.<token>` (the server echoes
  the subprotocol `rush.v1`). `?token=` and the cookie are NOT accepted on
  `rush serve` listeners (URLs end up in proxy and access logs).
- Origin is per listener: empty Origin allowed (non-browser); browser origins only
  by exact scheme+host+port match against the listener's `allowed_origins`
  (default empty), `null` rejected. The loopback UI keeps its current function.
- Scope enforcement per command (section 5); a denied command returns an error
  envelope and keeps the connection.
- Per-token cap on concurrent connections; a `hello` frame carrying the protocol
  version, token id and scopes; revocation closes streams within the auth-recheck
  interval (section 4).

## 9. TLS (C3/C4)

- `--tls-cert`, `--tls-key` (PEM). The server calls `ServeTLS(ln, "", "")` with
  `TLSConfig.GetCertificate` reading an `atomic.Pointer` snapshot.
- HTTP/1.1 ONLY on the TLS listener (`Server.Protocols` with only HTTP1, so ALPN
  offers only `http/1.1`). Go enables HTTP/2 on TLS listeners by default, and
  gorilla/websocket cannot upgrade over HTTP/2: a client that offers `h2` would
  negotiate it and then fail on the upgrade. Tests must use the production
  constructor, because `httptest.NewTLSServer` is HTTP/1.1-only and hides this.
- TLS 1.3 minimum by default, session tickets disabled. Optional
  `min_version: "1.2"` restricted to ECDHE with AES-GCM or ChaCha20-Poly1305 for
  old clients (the Go defaults still contain CBC suites). The renegotiation
  setting is dropped (Go servers never renegotiate).
- Reload: every 60 s re-read both files, compare CONTENT hashes (mtime is
  unreliable: preserved timestamps, coarse granularity, two-file replacement),
  validate the pair (key matches, validity window, SAN, `serverAuth` EKU, key
  strength) and swap the snapshot atomically; a bad candidate keeps the last good
  certificate and retries. Warn at 30/7/1 days before expiry and log an error
  after it; never fall back to cleartext.
- `--tls-self-signed`: ECDSA P-256, random serial, `serverAuth` EKU, leaf not CA,
  `NotBefore` one hour back, 365 days, key persisted in the data directory with
  restricted permissions and REUSED on renewal (so pins survive). SAN: localhost,
  127.0.0.1, ::1, the host name, the concrete bound address, plus `--tls-san`.
  A wildcard bind (`0.0.0.0`, `::`) with a self-signed certificate and no
  explicit `--tls-san` is refused. An expired existing certificate is never
  silently regenerated.
- Pins: `rush serve` prints the public-key pin (SHA-256 of SubjectPublicKeyInfo)
  in hex and in curl form `sha256//BASE64`, and the leaf-certificate fingerprint
  separately; the two are not interchangeable. The key pin survives renewal with
  the same key.
- mTLS: `--client-ca <pem>` alone enables `RequireAndVerifyClientCert` (no
  separate require flag; contradictory combinations are rejected; an empty or
  unparseable CA pool is refused). Never `RequestClientCert`, `RequireAnyClientCert`
  or `VerifyClientCertIfGiven`. The client certificate admits the connection; the
  bearer token alone selects scopes, expiry and revocation. The middleware never
  reads `r.TLS.PeerCertificates` to establish identity; the audit line records
  both the token id and the verified-certificate fingerprint. There is no
  CRL/OCSP check: use short-lived client certificates plus token revocation.
  "Two required credentials", not "two independent factors".
- Startup refuses: certificate without key or the reverse, mismatched pair,
  expired or not-yet-valid certificate, no SAN, missing `serverAuth`, RSA below
  2048 bits, unparseable client CA, group/world-readable key on Unix; it warns
  about broad key ACLs on Windows, a self-signed trust distribution, and nearing
  expiry. `ReadHeaderTimeout` is set (it also bounds the handshake) and
  `http.Server.ErrorLog` goes to slog with rate limiting.
- No plain-HTTP redirect listener.

## 10. Commands, listeners and configuration (decision D1: `rush serve`)

New headless command `rush serve`, built on the shared `internal/server` package
(not a copy). `rush web` and bare `rush` keep their behaviour, with these
explicit consequences: `rush web --host 0.0.0.0` remains a cleartext
legacy path (a loud start-up warning is added, refusing it is a separate
operator decision), and `serve` selects its MCP policy explicitly instead of
inheriting it from the command-tree position.

`rush serve`:
- never creates the per-process UI token, never mounts `/auth`, cookies or the
  SPA, never opens a browser or touches the clipboard;
- requires an explicit `--cwd`; one process is one App and one workspace;
- refuses to start (non-zero, no listener, no App, no MCP) unless at least one
  usable token exists, and prints the exact `rush tokens create` command with the
  same `--cwd`/global-directory selection; it never creates a token by itself,
  and revoking the last token while running leaves protected requests denied;
- handles SIGTERM as well as Ctrl+C, converging on: stop admitting HTTP/SSE/WS,
  cancel and join handlers, then App shutdown (the result of `Shutdown` is
  inspected, not discarded);
- runs in the foreground under a service manager; native Windows service
  installation is not part of v1.

Listener configuration `server.listeners[]` (Ktav-capable) is read ONLY from the
system and global config scope and from flags, never from project or workspace
config (a cloned repository must not be able to open an interface), and is a
start-up snapshot: changing it needs a restart.

```
addr: 127.0.0.1:8443
surfaces: [rest, ws]          # the ui surface does not exist in serve
tls: {cert: ..., key: ..., client_ca: ..., min_version: "1.3"}
allow_cleartext: false        # needed for C1/C2 on a non-loopback address
allow_admin: false
allow_auto_approve: false
allowed_origins: []
tokens: []                    # optional allow-list of token ids
limits: {rps: 20, burst: 40, max_conns_per_token: 8, max_body: 1MiB}
```

Flags override the first listener. Startup refuses: non-loopback without TLS and
without `allow_cleartext`; a missing `--cwd`; a missing token. All listeners are
validated before any is served and a late bind failure closes the ones already
bound. Shared server code takes an immutable per-listener policy (surfaces,
authenticator, origin rule, permission mode); a single "api enabled" boolean is
not enough, and "headless" is never inferred from `staticFS == nil` (that path
redirects to port 3000).

## 11. Abuse protection and audit

- Failed auth: uniform `401` with `WWW-Authenticate: Bearer`, no oracle for
  "id exists", exponential back-off per source IP after 10 failures in a minute.
- Token-bucket rate limit per token and per IP; request body cap; header and
  read timeouts (`ReadHeaderTimeout`); slow-loris safe.
- Audit line per request: token id, scope used, method, route template, status,
  session id, duration, remote address, certificate fingerprint when present.
  Tokens, Authorization headers and `?token=` values are redacted by one central
  helper; a test greps the log of a full end-to-end run for the plaintext token
  and fails if it appears.

## 12. Test plan

- Token store: create/parse/verify, expiry, revoke visible to a second store
  instance with no sleep, constant-time comparison, concurrent creation, hash
  never equals secret, project-directory token file ignored, malformed file
  denies all (mutants: a TTL cache; reading the working directory).
- Scope table exhaustiveness (all `Cmd*`), fail-closed default, admin-only gating
  on external listeners (mutant: flip the default to `read`).
- `rush serve` start-up: no usable token (empty, expired-only, revoked-only,
  read error) means zero listener binds, zero App constructions, no browser or
  clipboard, an actionable message; a valid-token control reaches the listener.
  `TestServeListener_NoUISurfaceNoUIToken`: no `GET /`, no `/auth`, `/ws` with a
  cookie or `?token=` is 401, and `Token()` is empty.
- Four-channel matrix through the PRODUCTION server constructor: plain HTTP,
  plain WS, TLS HTTP, TLS WS; a gorilla dialer offering `h2,http/1.1` negotiates
  `http/1.1` and upgrades (mutant: enable HTTP/2), a TLS 1.2 client is rejected,
  expired and wrong-SAN certificates are rejected by a verifying client, content
  hash reload (including preserved mtime and a mixed cert/key generation), mTLS
  accept and reject (a self-made client certificate is rejected; mutant:
  `RequireAnyClientCert`; a valid client certificate without a bearer is 401;
  mutant: identity taken from the certificate), established connections after
  certificate expiry.
- REST contract: OpenAPI vs routes, problem+json shape, idempotency replay,
  pagination, SSE replay by `Last-Event-ID`, permission ask/deny/timeout, API
  session in the UI of the same process does not arm auto-approve.
- Security: token redaction in logs, no `?token=` or cookie on `serve`
  listeners, Origin checks, brute-force back-off, oversize body.

## 13. Work breakdown

| Task | Content | Depends on |
|---|---|---|
| API-1 | Token store (user-global file), format, `rush tokens`, env bootstrap | - |
| API-2 | Listener policy in `internal/server`, `rush serve`, start-up refusals, SIGTERM, `OriginAPI` | - |
| API-3 | TLS: HTTP/1.1-only, 1.3 minimum, reload, self-signed, mTLS | API-2 |
| API-4 | Auth middleware, scope table, rate limit, audit, redaction | API-1 |
| API-5 | REST skeleton, read endpoints, problem+json, OpenAPI | API-2, API-4 |
| API-6 | REST run endpoints, SSE, idempotency | API-5 |
| API-7 | Permission modes, listener-injected auto-approve policy | API-4, API-6 |
| API-8 | WS hardening: header/subprotocol auth, origins, limits, hello | API-4 |
| API-9 | Four-channel e2e matrix, security tests, mutants | API-3, API-6, API-7, API-8 |
| API-10 | Docs, OpenAPI file, client examples, CHANGELOG | API-6 |
| API-11 | Client CLI `rush api` and `internal/apiclient` | API-6 |
| API-12 | Docs-lint, command-tree golden, coverage gate | API-1, API-2, API-11 |

An operational `rush serve` must not be released before token verification,
authorization and API permission handling (API-4, API-7) exist; API-2 alone may
land behind a refusal to start.

Suggested agent batches: auth core (API-1 + API-4), listeners and TLS
(API-2 + API-3), REST (API-5 + API-6 + API-10), permissions and WS
(API-7 + API-8), client CLI (API-11 + API-12), then verification (API-9).

## 14. Decisions

Each decision was answered independently by an `oh` agent and the rush reviewer
role; the outcome and the points where they differed:

- D1 `rush serve` as a separate command over shared server code: both agree.
  Both corrected the loopback claim, the MCP policy, `workdir_roots`, the
  start-without-token rule and SIGTERM (applied above).
- D2 token store: both rejected SQLite in favour of a user-global file; the
  recommendation of the first draft (SQLite table plus migration) is withdrawn.
  Reason: the session database is per project and project-controlled.
- D3 "certificate" is the server certificate, mTLS optional and additive: both
  agree; both found the HTTP/2 versus WebSocket defect and asked for content-hash
  reload. They differed on the TLS minimum (1.3 versus 1.2 with restricted
  suites): decided as 1.3 by default with an explicit `min_version: "1.2"`.
- D4 permission default `ask`, `auto` only for admin tokens or an explicit
  listener flag: unchanged.
- D5 no remote web UI: holds by construction because `serve` has no UI surface.
- D6 client CLI as a `rush api` group with global-scope remote profiles: both
  agree; `rush run --remote` is deferred until `run.go` (988 lines) is split.

## 15. Command-line interface (operator requirement: convenient, documented, tested)

The API must be as easy to use from a terminal as `rush run`. Three command
groups, all documented in `--help`, in the README and in `docs/`:

Server side
- `rush serve` (section 10) with `--listen`, `--cwd` (required), `--tls-cert`,
  `--tls-key`, `--tls-self-signed`, `--tls-san`, `--client-ca`, `--allow-cleartext`;
  `--print-config` prints the effective listener table and the pins without
  starting.
- `rush tokens create|list|show|revoke` (section 4), `--json`, `--expires`,
  `--scope`, `--permission`, `--name`; the secret goes to stdout once, warnings to
  stderr.

Client side: `rush api` (lightweight REST/SSE client in the package
`internal/apiclient`; cobra wiring in thin `internal/cmd/api_*.go`; it never
calls `setupApp`, so it opens no database, registers no project and creates no
`.rush/`; it does NOT restore the removed upstream client mode that auto-spawned a
server).
- Remote profiles: `rush api remote add|list|show|rm|test`. Profiles live under
  `api.remotes` / `api.default_remote` in the GLOBAL scope only; a project or
  workspace config can never define or override a remote (otherwise a cloned
  repository could point `prod` at an attacker URL and reuse the operator's token
  reference). A token source is bound to its profile URL and never combined with
  another URL. Profile selection: `--remote NAME`, then `RUSH_REMOTE`, then the
  explicit default; no implicit first profile.
- Credentials, exactly one source per profile: `token_env` (the NAME of the
  variable, conventional `RUSH_API_TOKEN`, not to be confused with the existing
  `RUSH_TOKENS`), or `token_file`. `remote add --token-stdin` reads the token
  from a hidden prompt or a pipe and writes `<global data dir>/remotes/<name>.token`
  (mode 0600) that the profile references. There is no `--token VALUE` flag; any
  argument that looks like a token (`rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}`) is
  refused with exit 2 and advice to rotate it. Token files with group/other bits
  are refused on Unix; on Windows the file must live under the user profile.
- Commands: `rush api sessions list|get|create|fork|delete`, `messages SESSION
  [--after ID]`, `run [-s ID] --role R [--detach] [--wait-timeout D] [--timeout D]
  [--json|--stream] "prompt"` (flags, shorthands and the `--json` envelope mirror
  local `rush run`, plus `run_id`; it waits by default), `runs get|wait RUN`,
  `events SESSION --follow [--format text|ndjson]`, `cancel RUN`, `inject SESSION
  -m TEXT|-f FILE [--interrupt]`, `permissions list|allow|deny`, `models`,
  `raw METHOD PATH [--data FILE]` (path relative to `/api/v1`, no header flags),
  `openapi`.
- Output: tables by default, `--json` for scripting (one object per line for
  lists, like `sessions list`); progress and diagnostics on stderr; streaming
  commands print NDJSON (id, type, session, run, data), `--quiet` never hides
  warnings or errors.
- Exit codes within `rush api`: 0 success (including an accepted detached
  submission), 1 failed run or network/TLS/server error, 2 usage or configuration
  error, 3 authentication or scope refused, 4 not found, 5 timeout, 6 run paused
  for a human (permission decision or `awaiting_answer`), 130 interrupted. This
  needs an `exitCodeError` type handled in `Execute` and the command journal;
  local `rush run` keeps 0/1/130.
- Ctrl+C: during a run this invocation submitted, send cancellation (fresh short
  context, brief wait for the acknowledgement, exit 130; if delivery fails print
  the run id and say it is unconfirmed); during `events --follow` only detach.
  Connection loss never cancels remotely. SSE reconnects with bounded backoff and
  `Last-Event-ID`, duplicates suppressed, an expired cursor is reported as a gap,
  a 401/403 is final.
- Transport safety: system roots and hostname verification by default; `--ca`;
  `--pin` (curl-style `sha256//BASE64` public-key pin, or `cert-sha256:HEX` for the
  leaf fingerprint) checked in `VerifyConnection` and combined with `--ca` when
  both are given; `--client-cert`/`--client-key` for mTLS; no redirects followed;
  `http://` to a non-loopback host refused unless `--allow-cleartext` on the
  client; URL userinfo and `?token=` refused. `--insecure-skip-verify` is
  per-invocation, loopback only, never stored, warns every time.
- v1 conveniences: `--after`, `--limit`, bounded `--all` pagination, stdin
  prompts, one retry of a POST with an automatic `Idempotency-Key` on connection
  errors, retries of GETs, SSE reconnect. Deferred: embedded jq/templates, retries
  of non-idempotent 5xx, interactive permission prompts, credential helpers,
  trust-on-first-use pinning, network completion of session ids, WebSocket client
  commands, `rush run --remote`.
- Shell completion covers commands, profile names and fixed value lists.

Documentation rules, enforced by tests (section 16): every command has `Short`,
`Long` and an `Example`, every runnable leaf an "Exit codes:" block, every local
flag a non-empty usage; every command appears under a `### rush ...` heading in
`docs/api/cli.md` and in the README section "Remote API"; the root help lists
`rush api`, `rush serve`, `rush tokens`.

## 16. Definition of done for every API task

- Unit tests for each new function with a decision in it; integration tests over
  real listeners built by the production constructor (plain and TLS); CLI tests
  against the in-process real server for happy paths and a fake server only for
  fault injection (401, 404, a stream cut mid-way, a slow server).
- `TestAPIDocsLint` (internal/cmd) walks the `api`, `serve` and `tokens` subtrees
  and fails when: a command lacks Short/Long/Example or a leaf lacks an "Exit
  codes:" block; a local flag has empty usage; a command has no heading in
  `docs/api/cli.md`; a flag is missing from its section or documented but absent;
  an exit code of the Go table is missing from the docs table; the README section
  does not name all three groups; a `rush ...` line in an `Example` or in a docs
  `sh` block (parsed with `mvdan.cc/sh/v3/syntax`, already a dependency) does not
  resolve to a command or fails flag/arg validation on a fresh `newAPICmd()`.
- `TestAPICommandTreeGolden` (with an `-update` flag, like the run JSON envelope
  golden) snapshots path, `Use`, args kind and flag name/shorthand/type/default,
  but not usage text; rendered `--help` is NOT snapshotted (it depends on terminal
  width, colour profile and the help renderer version).
- Revert-check mutants for every security rule (scope default, redaction, TLS
  minimum and ALPN, cleartext refusal, token comparison, project-scope token or
  remote ignored, redirect refusal, pin check).
- Coverage of the new packages measured with `go test -cover`, floor 85% of
  statements, recorded in the hand-over; uncovered branches are listed with the
  reason.
- The log of every integration test is scanned for the plaintext token.
- Source files stay under 1000 lines (`run.go` is 988 and `root.go` is 951:
  nothing new goes there beyond the exit-code plumbing).
