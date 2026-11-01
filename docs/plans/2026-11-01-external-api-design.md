# External API: authenticated REST and WebSocket channels

Status: design, nothing implemented. Written 2026-11-01; revised the same day
after four independent two-consultant reviews (an `oh` agent and the rush
reviewer role answered each open decision separately; the outcomes are recorded
in section 14), and extended the same day with token bindings (session pins and
batch intake, section 17, decisions D7-D9, three more two-consultant reviews).
Tasks: API-1 .. API-12 and API-13 .. API-18 in the task list.

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
`RunWithCredentials` stays SDK-only), isolation between tokens beyond scopes
and the bindings of section 17 (those confine the API surface only; they are
never an execution or filesystem sandbox), remote use of the web UI,
ACME/public certificates, serving several workspaces from one process.

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
every `Cmd*` constant and fails if one has no entry; an unmapped command is
denied to every token, `admin` included (fail closed, as in section 3). The table
is paired with a session-resolution table (section 17.2) that says where a
command's session comes from. Commands that spawn processes or expose files or secrets
(`shutdown_server`, `set_provider_key`, `remove_provider_key`, `add_mcp_server`,
`update_mcp_server`, `add_context_path`, `initialize_project`,
`add_custom_provider`, `update_custom_provider`) are admin-only AND rejected on
external listeners unless the listener sets `allow_admin: true`.

## 6. Tool permissions and workspace for API sessions

Per-token `permission` setting, default `ask` (decision D4):

- `ask`: a tool call that needs permission pauses the run and emits a
  `permission_request` event (SSE and WS). The client answers with
  `POST /api/v1/permissions/{id}` (`allow` once, or `deny`; `allow_session` is not
  offered in v1, see section 17.5) or the WS equivalent. No answer within
  `permission_timeout` (default 5 min) = deny. The permission service has no such
  timer (a request blocks until its context ends), so the API layer runs it and
  calls `Deny`.
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

The pinned-token alias routes (`/session/...`), the batch routes (`/batches`,
`/tasks`) and the busy/404/403 rules are in section 17.

Runs are asynchronous by design: 202 plus polling or SSE. A remote run that stops
on a question reports `awaiting_answer` with the REMOTE resume command, never the
local `rush run --session` guidance (a local resume would work on a LOCAL copy of
that session id, not on the remote one). The OpenAPI document is generated from the route table and a contract
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
route: full                   # "batch": every work submission on this port is batch intake
pins: {}                      # token id -> session id; narrows only (section 17.1)
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
| API-13 | Token bindings (session pin, batch binding), listener `pins` and `route`, narrowing rules | API-1, API-2 |
| API-14 | Session-pin enforcement: hub view filter, session-resolution table, alias routes, IDOR matrix | API-13, API-4, API-6, API-8 |
| API-15 | Batch intake: `/batches`, `/tasks`, service session, allowlist, limits, watcher, durable idempotency, restart semantics | API-13, API-6, API-7 |
| API-16 | Sessions through batches: `child_session_id`, resume of own children, held-question answer endpoint | API-15 |
| API-17 | CLI for bindings and batches (`rush tokens`, `rush serve`, `rush api batch|task`) | API-11, API-13, API-15 |
| API-18 | Binding docs, four-channel x binding matrix, mutants | API-9, API-14, API-16, API-17 |

An operational `rush serve` must not be released before token verification,
authorization and API permission handling (API-4, API-7) exist; API-2 alone may
land behind a refusal to start.

Suggested agent batches: auth core (API-1 + API-4), listeners and TLS
(API-2 + API-3), REST (API-5 + API-6 + API-10), permissions and WS
(API-7 + API-8), client CLI (API-11 + API-12), then verification (API-9). Bindings
follow in three batches: model and pin enforcement (API-13 + API-14), batch intake
(API-15 + API-16, starting with the experiment of section 17.7), then CLI, docs and
matrix (API-17 + API-18).

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
- D7 token binding model and session pinning: both agree the pin belongs to the
  token record and a listener may only narrow it; both found that the WebSocket
  hub broadcasts every session's events to every client (a precondition for
  pinned WS/SSE) and that `allow_session` through `GrantPersistent` writes a
  global grant. They differed on provisioning a `new` pin (a separate global
  bindings file with a provisioning state machine versus a session id generated
  at token creation): decided as the generated id, no extra state.
- D8 batch intake: both agree on one root batch per request under a per-token
  service session, a recursive tool allowlist checked before translation and
  durable owner-scoped reads. They differed on idempotency (in-memory versus a
  separate ledger versus the root's submit reference): decided as the submit
  reference, durable and with no new storage.
- D9 sessions through batches: both agree an `agent` item creates a child of the
  batch owner and resumes only direct children (enforced twice in the existing
  code), that `child_session_id` must be exposed, that a live held question is
  answered through a dedicated endpoint (not by another `agent` item), and that
  unbound tokens stay workspace-wide in v1 without an ownership tag.

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
- Bindings (section 17): `rush tokens create --pin-session ID|new`, `rush tokens
  create --batch [--tools agent,...] [--max-roots N]`, `rush serve --pin-session
  TOKEN_ID=SESSION_ID` (repeatable) and `rush serve --route batch`; client side
  `rush api task [--resume SESSION] "prompt"` (a one-prompt batch), `rush api batch
  run|show|wait|stop|output|answer`. They follow the same exit codes (a held
  question is exit 6) and the same documentation rules as the rest of `rush api`.
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

## 17. Token bindings: session pins and batch intake (decisions D7-D9)

Operator requirement (2026-11-01): a token may lead straight to batches ("a port
is set and incoming requests on it go to batches", with the ability to create and
continue sessions there), or a start-up option of the port may fix a session to a
token. Each open decision was answered independently by an `oh` agent and the rush
reviewer role. Everything below is a design; nothing is implemented.

### 17.1 Binding kinds and the narrowing rule

A token carries at most one binding, fixed at creation (changing it means revoking
the token and creating a new one):

| Kind | Meaning |
|---|---|
| none (default) | Section 4 to 11 unchanged. An unbound `run` token reaches every session in the workspace, including other tokens' sessions and permission requests; this is documented in `--help` and the README, and there is no ownership tag in v1. It may also use the batch routes (17.3). |
| session | `pin {session, workspace, create}`: the token operates one session. |
| batch | `batch {tools, max_roots, max_parallel}`: the token's only work route is batch intake; ordinary session runs, session creation and forks are denied. |

Rules:

1. The binding lives in the token record (user-global file), because a credential
   must carry its confinement to every listener and process. A pin to a session of
   one workspace is meaningful only for that workspace: the record stores the
   canonical `--cwd` (absolute, symlinks resolved, case-folded on Windows; not
   `config.WorkspaceRoot`, which is empty outside git), and the token answers 403
   `token-not-for-this-workspace` on a server started with another `--cwd`.
2. A listener may only NARROW: `rush serve --pin-session TOKEN_ID=SESSION_ID`
   (repeatable) or `pins:` in the listener config pins a token that has no pin on
   that listener; a pin that conflicts with the token's own pin refuses start-up; a
   listener that declares any pins admits only pinned tokens. `rush serve --route
   batch` (or `route: batch`) makes batch intake the only work route on that port:
   session runs are denied there while reads, permission answers, cancellation and
   held-question answers stay available. A listener pin does not confine the same
   token on another listener or process; confinement that must follow the credential
   is a token-record binding.
3. A binding is refused together with `admin` scope (global administrative
   operations defeat confinement), at creation and at start-up. A pin and a batch
   binding exclude each other: combining a conversation session with batch intake
   mixes permission state and busy-session conflicts (both consultants rejected
   it); deferred.
4. Authorization is the intersection of scope, binding, listener policy and the
   permission mode of the run; no binding grants a right the scope lacks (a pinned
   `run` token cannot change the session's models: that is `config`, and only for
   its own session).
5. Within one `rush serve` process every token pinned to the same session must have
   the same permission mode, or start-up is refused (17.5, escalation).
6. A binding confines the API surface, not the agent. If the permission mode lets
   the agent run a shell, a bound token is not isolated from the workspace, the
   session database or other sessions. This sentence must appear in `--help` of
   `rush tokens create` and `rush serve`, and in the README.

### 17.2 Session pin (D7)

- Token creation: `rush tokens create --name bot --scope run --cwd DIR --pin-session
  ID` pins an existing session; `--pin-session new` generates the session id at
  creation (stored in the record, nothing else is persisted) and the session is
  created under that id on the first authorized work request, with origin `api` and
  the permission policy of the token. Creation stays store-only (no App, no
  database). `--pin-session` requires `--cwd`.
- Start-up resolves every effective pin. A listener pin must name an existing,
  top-level session that the process owns (`session.Owns`: workspace root plus home
  flag; a data directory can be shared by linked worktrees); anything else refuses
  start-up. A record pin whose session is missing only logs a warning, so one bad
  token does not take the others down.
- Missing session: an explicit pin answers 404 problem type `pinned-session-missing`
  (never auto-created, so clients do not retry forever); a `new` pin is created again
  with the same id on the next request (deleting it resets the conversation; revoke
  the token to stop). A foreign or child row is never adopted (409
  `pinned-session-foreign`).
- Allowed for a pinned token: health, version, models, openapi; a session list with
  one entry (the pinned session); get, messages and events of the pinned session;
  runs, inject, cancel, rename, summarize, rerun and message edits on the pinned
  session only; permission decisions for requests of the pinned session or of its
  descendants. The pinned session's sub-agent children are visible for reads,
  cancellation and permission answers; they are never targets of runs, inject,
  rename, rerun, fork or delete (they cannot be continued directly).
- Denied (403 `pinned-token`, even for the pinned session): create, fork, delete,
  delete-other sessions; global or scoped model settings; logs; theme, keep-alive,
  debug; context and skill paths; project init; provider, key and peak-hours
  changes; every MCP mutation; shutdown; the UI diagnostic commands.
- Per-run overrides: a model override needs `config`; a role override is allowed
  under `run` only from the listener's configured role set; attachments are bounded
  by `max_body` and never name server-side paths.
- Alias routes for pinned tokens: `/api/v1/session`, `/session/runs`,
  `/session/events` and so on resolve to the pin; for an unpinned token they answer
  404 `not-pinned`.
- Order of checks: first visibility (a foreign or nonexistent id, run id, message
  id, permission id or batch id answers an identical 404), then operation class
  (403). Every indirect id is authorized through its actual owner: run to session,
  message to session, permission to the request's session, batch node to its owner.
- Busy sessions: a run is admitted atomically or refused: 409 `session-busy` when
  the session is processing, 409 `session-owned-externally` when another process
  holds the session's OS lock (the holder's PID is not disclosed), otherwise 202.
  Queuing (`when_busy: queue`) is deferred until queued work has a durable run
  identity, a fixed permission policy and cancellation.
- WebSocket and SSE: today the hub has no subscription model; every broadcast, the
  2000-event replay buffer, sticky events and the direct `list_sessions` reply reach
  every connection, and the MCP snapshot carries configured environment and headers.
  Pinned WS and SSE therefore need ONE egress view filter at the hub: events carry
  typed metadata (session id, event class) derived once at broadcast through an
  exhaustive event-to-session map (with an exhaustiveness test like the scope table);
  each connection has an immutable view; the filter applies to live delivery, replay
  and sticky sends; unknown or untagged events never reach a pinned view; foreign
  session, message, busy, live-work, wake and permission events are suppressed;
  broad config and MCP snapshots are replaced by sanitized projections or dropped;
  `list_sessions` has a pinned-aware handler. REST responses and SSE reuse the same
  resource-policy code. An outbound filter cannot undo an unauthorized mutation, so
  the session-resolution step (one table: command or route to where its session
  comes from, with an exhaustiveness test over every `Cmd*` constant) runs before
  every handler, in REST and in the WS dispatcher.
- Streaming of descendant sessions to a pinned WS client is not in v1: children are
  visible through live-work reads.

### 17.3 Batch intake (D8)

Facts (observed in the code): a batch item is `{tool, input}` resolved in the
pinned coder toolset of the process that owns the batch (`BatchCLIBuilder`); the
allowed work tools are `agent`, `agentic_fetch`, the I/O tools (which include
`bash`, `run_command`, file writes and network tools) and `mcp_*`; control-plane
tools are refused; items can be added to a batch only from the owning process and
only while it is not terminal; a batch becomes terminal the moment its last direct
child does; the node budget is cumulative per root (engine defaults: 16 concurrent
slots, depth 8, 256 nodes); after a restart unfinished nodes become `interrupted`
(`host_lost`) and nothing is re-run.

Rules:

1. Every accepted submission creates exactly one new root batch. There is no
   long-lived or appendable batch per port, and API `add` is not offered in v1.
   `POST /api/v1/batches` takes the external structural schema; `POST /api/v1/tasks`
   is a one-prompt convenience (one `agent` item through `OneItem`). Both answer 202
   with `root_id`; `owner_session_id` is not returned to a batch-bound token.
2. All of a token's roots are owned by one service session in the serving workspace:
   id `api-<tokenid>-<nonce>` generated at token creation, origin `api`, created
   lazily, never accepting conversational runs, never used by another token. The
   owner id is the only confinement tag (no schema migration). The submit reference
   keeps the `batch-cli:` family prefix, so API roots do not wake the owner or inject
   notices into a conversation; their notice debt is settled by the server.
3. Submitted tools are validated recursively, before translation and before any row
   is written, against the token's allowlist (default exactly `agent`). `bash`,
   `run_command`, write tools, network tools and `mcp_*` only when listed by exact
   name. `agentic_fetch` stays off by default (its child sessions are auto-approved
   unconditionally today). `readonly` batch tokens may submit read-only I/O tools
   only: `agent`, `agentic_fetch` and `mcp_*` are refused, because the reviewer
   read-only filter skips sub-agents and a leaf's run allowlist is not read by any
   leaf. An `agent`-only token is NOT harmless: it hands the caller a sub-agent with
   whatever the permission mode allows.
4. Limits, all enforced in the API layer before submission, aggregated across
   listeners: at most `max_roots` live roots per token (default 2; pending, starting,
   running and cancelling count), `max_parallel` forced onto every root (default 2),
   all API leaves together under half of the engine's slots (the pool is shared with
   every tool call of every model turn in the process, and a leaf waiting for a
   permission answer keeps its slot), at most 64 nodes and depth 3 per request, body
   up to the listener's `max_body`, a mandatory item timeout (default 30 min, token
   maximum). Overload answers 429 with `Retry-After`; there is no API-side queue.
5. Permissions: API batches never inherit the CLI's blanket approval
   (`rush batch run` arms auto-approve for its owner session; the API must not call
   that for any mode but `auto`, and must re-arm lazily after a restart). `ask`
   works for batch leaves: a direct leaf publishes its request against the service
   session, a delegated child against the child session; both reach the token's
   stream, authorized by "service session or descendant". The timeout of 5 minutes
   is enforced by the API (Deny). `allow_session` is refused for service-session
   prompts in v1.
6. Reads come from the durable read model, not from the in-memory CLI owner (that
   one fails after a restart): `BatchStore.Read` and exact `BatchCLIGetNode` scoped
   to the service session. `BatchCLIFind` (prefix matching across owners) is never
   used. Responses are explicit DTOs (state, outcome code and summary, envelope,
   `child_session_id`), never raw rows (they hold internal payload and registry
   data). Full output is best-effort: live process only, about 10 minutes, bounded;
   the response says `output_expired` instead of silently omitting it. Finished work
   is kept for the 7-day purge. `sessions gc` currently deletes sessions older than 7
   days with zero messages, and the batch rows cascade with the owner: it must keep
   any session that owns batch rows.
7. A per-root watcher in the serving process pumps stop requests (cross-process
   control reaches a root only while something pumps it), settles the notice debt
   and releases the root when terminal, independent of whether a client ever polls.
8. Idempotency-Key is durable and body-bound without new storage: it is encoded in
   the root's submit reference (`batch-cli:api:<tokenid>:<keyhash>:<bodyhash>:<rootuuid>`,
   within the 128-byte limit, unique per root so the live-index attach path cannot
   fault the runner). A replay returns the original root; the same key with a
   different body is 422. Two serve processes on the same workspace do not share the
   in-process lock; documented. No exactly-once guarantee for external effects.
9. Restart: unfinished nodes are `interrupted` with code `host_lost` and are never
   re-run; a read triggers owner-scope recovery first (the periodic pass can lag up
   to about a minute). The client reconciles external effects and resubmits under a
   new key. Children keep their history and are continued by a new batch with
   `resume_session_id`.
10. When to use which: a pinned session (or `/sessions/{id}/runs`) for one ordered
    conversation in a top-level coder context; batch intake for independent, fan-out
    or queued jobs: a parallelism cap, per-item state and output, stop per node or
    root, sequential `on_fail`, and continuation of sub-agent children. Batch intake
    costs a sub-agent per `agent` item, transient output and no resume after a
    restart.

### 17.4 Creating and continuing sessions through batches (D9)

1. In a batch leaf the current session is the batch owner, regardless of structural
   nesting. `agent` with a prompt creates a child of the owner; `agent` with
   `resume_session_id` continues a direct child of the owner and nothing else (not
   the owner, not a grandchild, not another owner's child). The existing code
   enforces this twice (the resume parent check and the durable child-bind check);
   the API pre-check only returns early, friendlier errors.
2. `child_session_id` is exposed on every node result and event once bound (it is
   already stored in the batch node row; it was never put in the CLI projection).
   Clients never parse prose for it.
3. A question held by a child inside a batch is answered in the same node
   incarnation through `POST /api/v1/batches/{root}/nodes/{node}/answer` (the
   existing held-answer delivery; a stale question is 409, after which the client
   resumes the child in a new batch). Submitting another `agent` item with
   `resume_session_id` does not answer it and leaves the original execution blocked.
   Held-question events carry `{root_id, node_id, child_session_id, question}`.
4. The owner's own conversation is continued only by a run on a session, never by
   an `agent` item; child sessions are never run targets; runs never create a
   session with a client-chosen id (use Idempotency-Key).
5. The unbound `run` token keeps workspace-wide reach for session routes. For batch
   routes the service session confines it to its own roots and children.

### 17.5 Defects of the current code found while reviewing (not caused by this feature)

These constrain the design and are tracked in API-7, API-14 and API-15:

- The WebSocket hub broadcasts to all clients (17.2).
- `GrantPersistent` stores a grant with an empty session id: it matches every
  session in the database and outlives revocation of the token. API `allow_session`
  must not use it; v1 offers `allow` (once) and `deny` only.
- `AutoApproveSession` is a never-cleared in-memory map keyed by session and is
  inherited by sub-agents: an `auto` token running once on session S makes later
  `ask` runs of other tokens on S auto-approved. The API never calls it; `auto` is
  applied per run; mixed modes on one session are refused (17.1 rule 5).
- The permission service has no timeout (17.3 rule 5) and no ownership check on
  `Grant`/`Deny` by id: every decision endpoint resolves the pending request to its
  session and authorizes against the binding.
- `agentic_fetch` auto-approves its child sessions whatever the mode.
- The WS `get_logs` returns a log containing every session's prompts: admin only.
  The `mcp_state` broadcast carries MCP environment and headers: never to a
  non-admin client on an external listener.
- The WS `delete_session` has no ownership check (`delete_other_sessions` has one):
  with a shared data directory a `run` token could delete rows of another worktree.
  The REST route checks ownership.
- `rush batch run` arms auto-approve for its owner session (CLI only; stays).

### 17.6 v1 cut

Ships: bindings none/session/batch with the narrowing rules; the pin enforcement
with the hub view filter and the session-resolution table; batch intake with the
service session, allowlist, limits, watcher, durable idempotency and restart
semantics; `child_session_id`; the held-question answer endpoint; CLI, docs, tests.

Deferred: ownership tags for unbound tokens; pin plus batch on one token; `when_busy:
queue` and any durable queued admission; API `add` to a live batch; streaming of
child sessions to pinned WS clients; answering questions of grandchildren; inject or
interrupt on children; `allow_session`; token-wide active-leaf caps (needs
hierarchical scheduler accounting); durable full output; `rush tokens pin` re-pinning.

### 17.7 First tests

- Premise experiment before building API-15 (no existing test drives an `agent`
  item through `BatchSubmit`): `TestBatchSubmit_AgentItemCreatesThenResumesChild`
  with a fake provider: a completed leaf with `child_session_id` whose parent is the
  owner; a resume of that child grows its messages; a resume of another owner's
  child fails with the "not a child of the current session" text. Also whether a
  held child question on an API-owned root wakes the owner's model (expected: no, as
  for CLI roots).
- `TestHub_PinnedViewNeverReceivesForeignSessionEvents` (live, replay, sticky, direct
  reply; an unpinned control sees everything; an MCP snapshot with a sentinel header
  is absent; reverting the filter in deliver or in replay must fail).
- `TestBatchIntake_AgentOnlyRejectsNestedRunCommandBeforeConstruction` (nested
  `run_command` under an `agent`-only allowlist: 403, zero rows, no owner session
  created, no live-root slot consumed; a valid control reaches submission; removing
  the recursive check must fail).
- `TestBatchLeafResults_CarriesChildSessionID`.
- `TestAPIBatchQuestion_AnswerContinuesExistingIncarnation` (same node incarnation
  completes, one child, no second delegation; another token gets 404).
- Foreign root, node, permission id and run id answer the same 404 as a nonexistent
  one; a pin with another `--cwd` answers 403; idempotency replay across a restart;
  `interrupted` after a restart and no re-run; mixed permission modes on one session
  refuse start-up; a blocked `ask` leaf does not exceed the global intake cap.
- Each rule above that is a security rule gets a revert-check mutant (filter,
  allowlist, ownership, workspace check, narrowing, body hash).
