# API tool permissions

API token permission mode controls tool calls made during API-originated runs. The token's selected mode is captured for the run and applied through the server's permission policy; it is separate from the request's API scope.

| Mode | Behavior |
|---|---|
| `ask` | Default. A tool permission request is surfaced as a `permission_request` event. The caller can allow that request once or deny it. If no decision arrives within five minutes, the broker denies it. |
| `readonly` | Does not prompt. Allows only the fixed read-only tool list and read-like actions; other tool/action combinations are denied. |
| `auto` | Automatically allows tool calls for the run. Token creation requires `admin` scope for this mode; the run backend also permits it for a non-admin token only when the listener sets `allow_auto_approve`. Use with care. |

The readonly allowlist currently consists of `fs_find`, `fs_grep`, `fs_list`, `fs_read`, `git_read`, `glob`, `grep`, `ls`, `rush_info`, `rush_logs`, `sourcegraph`, and `view`. The action must be empty, `read`, or `list`; other actions are not treated as read-only. The allowlist is implemented in `internal/permission/session_policy.go` and may not coincide with tools called “read-only” in other contexts.

## Permission broker and REST endpoints

The API permission broker observes requests registered to an active API run, emits permission requests, and applies the decision through the existing permission service. Pending requests are scoped to the owning token, unless the caller has `admin` scope.

- `GET /api/v1/permissions` lists pending requests visible to the caller. Optional `?session=<id>` filters by session; it must occur no more than once.
- `POST /api/v1/permissions/{id}` accepts JSON `{"decision":"allow"}` or `{"decision":"deny"}`. Both routes require `run` scope.
- `allow_session` is not accepted. An unsupported decision receives 400.
- A missing request receives 404; a request already decided receives 409. If the broker or principal is unavailable, the handlers return 404 (not 503).
- A pending request is denied when the run registration ends, the broker is unavailable/stops, or the five-minute timer expires. The permission service itself has no API timeout; the broker owns this timeout.

## Workspace and approval safety

The API operates against the one workspace selected when `rush serve` starts. Permission modes are not a filesystem or process sandbox. In particular, `readonly` is constrained by the API session policy's tool/action allowlist; do not assume a token binding or workspace selection confines an allowed tool's effects. `auto` is logged when used. Avoid auto-approval unless the remote caller and workspace are fully trusted.

An admin scope alone does not silently change an `ask` token to `auto`; the token must use `auto` mode, and the listener policy must still allow it unless the token has admin scope. Conversely, non-admin `auto` mode is rejected at run start unless `allow_auto_approve` is enabled on that listener.
