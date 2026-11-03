# Token bindings and listener routes

A token can be **bound** when it is created: a binding narrows what the credential may do on every listener and process that accepts it. A listener can only **narrow** further (a pin or a route); it never widens a token. This page covers both and the permission rules they follow.

A binding confines the API surface, not the agent. If the permission mode lets the agent run a shell, a bound token is not isolated from the workspace, the session database or other sessions. Bindings are not an execution or filesystem sandbox.

## Kinds

| Kind | Created with | The token may |
|---|---|---|
| none (default) | `rush tokens create --name N` | everything its scopes allow: every session of the workspace, including other tokens' sessions and permission requests, and the batch routes |
| session | `--pin-session ID` or `--pin-session new` (requires `--cwd`) | work on one session only |
| batch | `--batch [--tools …] [--max-roots N] [--max-parallel N]` | batch intake only |

A token carries at most one binding, fixed at creation; changing it means revoking the token and creating another. A binding is refused together with the `admin` scope (at creation and at start-up), and a session pin and a batch binding exclude each other. Because the `auto` permission mode requires `admin`, a bound token cannot use `auto`.

`rush tokens show` and `rush tokens list` print the binding.

## Session pin

```sh
rush tokens create --name bot --scope run --cwd ./project --pin-session 7d3c1f02
rush tokens create --name bot --scope run --cwd ./project --pin-session new
```

`--pin-session ID` pins an existing session; `new` generates the id at creation (stored in the token record, nothing else is persisted) and the session is created under that id on the first authorized work request, with origin `api`. The record stores the canonical `--cwd`: the token answers `403` `token-not-for-this-workspace` on a server started with another `--cwd`.

A pinned token on a REST or WebSocket listener:

- lists only its own session; any other id (existing or not) answers the same `404`, so existence is not revealed;
- reads and works on its own session (messages, runs, inject, events, its own run and permission handling);
- is refused (`403`, `pinned-token`) for session creation, forks, deletes, the global and configuration commands, and the batch routes;
- sees only its own session's events and run notices.

## Batch binding

```sh
rush tokens create --name worker --batch --tools view,grep --max-roots 4 --max-parallel 3
```

A batch-bound token reaches batch intake only ([Batch intake](batches.md)): `POST /tasks`, `POST /batches`, their reads, stops and answers. `--tools` lists exact extra tool names the token may submit besides `agent` (no wildcards); `--max-roots` and `--max-parallel` override the server defaults (0 keeps them).

Everything that starts or reshapes conversational work is refused with `403` `batch-only` on REST and `forbidden: batch-only` on WebSocket: runs, injects, session creation, forks and deletes, and every route or command the server does not classify. These stay available: health, version, models, session reads and event streams, the token's own run and permission handling, cancellation and answers to held questions.

## Listener narrowing

A listener can narrow tokens that carry no binding of their own, in the global listener configuration or on the command line.

**Pins.** `rush serve --pin-session TOKEN_ID=SESSION_ID` (repeatable) or `pins: {TOKEN_ID: SESSION_ID}` pins a token on that listener. A listener that declares any pin admits only pinned tokens. A pin that conflicts with the token's own pin, names an unknown token, a token with the `admin` scope or a batch-bound token, or a session the process does not own, refuses start-up. A listener pin does not confine the same token on another listener or process: confinement that must follow the credential is a token binding.

**Route.** `rush serve --route batch` (or `route: batch`) makes batch intake the only work route of the first listener: the batch-only rules above apply to every token that connects there. `--route full` (the default) is unrestricted. `route: batch` cannot be combined with pins on the same listener (start-up refuses it); `rush serve --print-config` shows the effective route and where it came from.

## Permission modes

Within one `rush serve` process every token pinned to the same session must have the same permission mode, or start-up is refused: otherwise one credential could escalate a session another credential runs in.

## Authorization is an intersection

A request is allowed only if the token's scope, its binding, the listener (pins and route) and the permission mode of the run all allow it. No binding grants a right the scope lacks: a pinned `run` token cannot change its session's models, which is the `config` scope.

## Checking it

The rules are enforced on all four channels (plain and TLS HTTP, plain and TLS WebSocket) and tested through the production listener constructor for each kind: unbound, session-pinned, batch-bound and unbound on a `route batch` listener. See the exit codes and flags of the commands in the [CLI reference](cli.md).
