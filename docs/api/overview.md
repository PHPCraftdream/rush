# Rush API overview

`rush serve` runs the authenticated, headless API for one explicitly selected workspace. It is separate from `rush web`: it does not mount the UI, SPA, `/auth`, cookies, browser launch, clipboard integration, or the transient UI token. The process requires `--cwd` and at least one usable API token before it opens listeners or constructs the application. It runs in the foreground and handles Ctrl+C and SIGTERM.

Create a token, then start the server:

```sh
rush tokens create --name local --scope run
rush serve --cwd ./project
```

The token command prints the credential once. Keep it secret and supply it to clients as documented in [Authentication](auth.md). The default listener is `127.0.0.1:8080` with `rest` and `ws` surfaces. Configure listeners in global/system configuration or use `--listen`; project configuration cannot open a listener. A non-loopback cleartext listener requires explicit `allow_cleartext`/`--allow-cleartext`. TLS configuration is currently rejected as not implemented; do not treat the HTTPS/WSS options in the design plan as working features.

## Listener surfaces and channels

One listener can expose REST and WebSocket surfaces. The `ui` surface is not permitted by `rush serve`.

| Channel | URL scheme | Surface | Status |
|---|---|---|---|
| HTTP REST | `http://` | `/api/v1/...` | Implemented |
| WebSocket | `ws://` | `/ws` | Implemented |
| HTTPS REST | `https://` | `/api/v1/...` | Not implemented; TLS policy validation rejects configured TLS |
| Secure WebSocket | `wss://` | `/ws` | Not implemented; TLS policy validation rejects configured TLS |

The REST API uses JSON and `application/problem+json` errors. `/api/v1/health` is public; other declared routes require the scope specified by their route. See [Errors](errors.md), [Permissions](permissions.md), [REST reference](rest.md), and [TLS listener status](tls.md). SSE is available at `GET /api/v1/sessions/{id}/events` through the production `NewServeAPI` route wiring.

The listener policy enforces per-IP and per-token request limits, a request-body cap, and a concurrent-connection cap per token. REST event streams are limited to eight per token. WebSocket origins permit an empty Origin for non-browser clients; browser origins must match a configured origin exactly (scheme, host and effective port), and `Origin: null` is denied.

## Scope of access

Tokens are scoped credentials, not workspace sandboxes. A run-capable request can start work using the serving process's permissions; the API does not confine shell execution to the workspace. Each process serves one workspace. Unmapped WebSocket commands are denied, and admin commands are additionally subject to the listener's `allow_admin` policy. Permission modes add a tool-call gate; see [Permissions](permissions.md).

## What is implemented versus design

The design document describes more than the current implementation. In particular, TLS/HTTPS/WSS, token bindings and pinned-session routes, and batch intake are design-plan features, not documented here as available API behavior. `internal/server/listener_policy.go` explicitly rejects TLS with “TLS is not implemented yet”; the current REST route table is the authority for available routes.
