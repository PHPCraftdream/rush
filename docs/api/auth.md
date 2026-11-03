# Authentication

## Token format and storage

The token shape is `rush_xxxxxxxx.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` (the suffix is a 43-character placeholder, not a credential). The token store persists a SHA-256 digest of the complete token, never the plaintext credential. `rush tokens create` returns the plaintext once; store it in an appropriately protected secret manager.

Tokens are user-global, not project-scoped. The token store is outside project configuration and is resolved from the global configuration data directory. Records contain the token name, scopes, permission mode, creation time, optional expiry, and revocation state. Revocation and expiry cause authentication to fail. Do not put credentials in source control or URLs.

## HTTP bearer authentication

For REST and standard WebSocket clients, send exactly one Authorization header:

```http
Authorization: Bearer <token>
```

The server accepts `read`, `run`, `config`, and `admin` scopes in increasing privilege order. Higher scopes include lower scopes (`run` includes `read`; `config` includes `run`; `admin` includes `config`). Routes have their own required scope. The public health route is the exception; otherwise an insufficient scope returns 403.

## WebSocket authentication

Connect to `/ws` using `Authorization: Bearer <token>`. A browser client that cannot set Authorization may provide `Sec-WebSocket-Protocol: rush.bearer.<token>, rush.v1`; the server requires the `rush.v1` protocol with the bearer subprotocol. Do not send both header and bearer subprotocol; ambiguous credentials are rejected. The server's origin policy allows a missing Origin, denies `null`, and otherwise requires an exact configured scheme/host/port match.

## Credentials in URLs and cookies are refused

`?token=` and `?access_token=` are rejected, and serve authentication does not use cookies. URLs commonly enter browser history, proxy/access logs, and monitoring systems; headers avoid this routine exposure. The `rush serve` process does not use the UI's cookie or transient UI token authentication.

## Failed authentication, throttling, and redaction

Failed credentials receive a uniform 401 response with `WWW-Authenticate: Bearer`; responses do not reveal whether an id exists. After repeated failures, the source IP is temporarily backed off with exponential duration. Requests may also receive 429 Too Many Requests with `Retry-After` when the per-IP/per-token rate limit, connection slots, or event-stream cap is reached. The default API middleware limits are 20 requests/second, burst 40, eight connections per token, and a 1 MiB body cap; listener settings can override limits.

Authorization credentials and secrets are redacted from audit/error logs. Never include real tokens in examples, tickets, or diagnostic output.

## Create and use a token

```sh
rush tokens create --name local --scope run
rush serve --cwd ./project
```

Token creation defaults to `run` scope and permission mode `ask`. For more on the effect of scopes and permission modes, see [Permissions](permissions.md).
