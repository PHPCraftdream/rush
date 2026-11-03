# WebSocket API

The protocol supports `ws://` and, on a TLS listener ([TLS listeners](tls.md)), `wss://`. On external serve listeners, authenticate with `Authorization: Bearer TOKEN`, or use WebSocket subprotocols `rush.v1` and `rush.bearer.TOKEN`; the server selects/echoes `rush.v1`. Missing version, ambiguous credentials, invalid authentication, or rejected Origin fails the upgrade. Query-string tokens and cookies are not accepted on serve listeners. Empty Origin is accepted; `Origin: null` is rejected; browser origins require an exact configured scheme, host and port match.

Immediately after upgrade, the server sends a JSON `hello` event whose payload is `{ "protocol":"rush.v1", "token_id":"…", "scopes":[…] }`. Treat the token id as an identifier, not a credential. For illustration, a non-real token would be `rush_xxxxxxxx.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx`; the secret segment is 43 placeholder characters. Never substitute an actual credential in documentation.

## Client command envelope

Send a JSON object with a `type` command name and command-specific `payload`; include an `id` to correlate the response. The response echoes the supplied id. Example shape (payload fields depend on command):

```json
{"id":"request-1","type":"list_sessions","payload":{}}
```

Authorization denial is returned as an error response without closing the connection:

- `forbidden: scope` — required scope not granted.
- `forbidden: admin commands are disabled on this listener` — admin scope command blocked by listener policy.
- `forbidden: unknown command` — unmapped command, denied fail-closed even to admin.

## Command scope table

This table reflects every entry in `internal/server/scope_table.go` (`commandScopes`). `admin` scope includes lower scopes for authorization.

| Command | Scope | Command | Scope |
|---|---|---|---|
| `send_message` | run | `interrupt_and_send` | run |
| `inject_message` | run | `cancel_agent` | run |
| `create_session` | run | `fork_session` | run |
| `delete_session` | run | `delete_other_sessions` | run |
| `list_sessions` | read | `load_messages` | read |
| `get_config` | read | `get_logs` | admin |
| `set_theme` | config | `set_keep_alive` | config |
| `rename_session` | run | `set_session_models` | config |
| `remove_recent_model` | run | `track_model_usage` | run |
| `delete_message` | run | `delete_messages` | run |
| `update_message_content` | run | `update_message_thinking` | run |
| `get_system_prompt` | read | `set_system_prompt` | config |
| `summarize_session` | run | `cancel_queued_summarize` | run |
| `delete_message_part` | run | `update_message_part` | run |
| `toggle_pin_message` | run | `rerun_message` | run |
| `get_scoped_models` | read | `set_scoped_model` | config |
| `clear_scoped_model` | config | `get_session_live_work` | read |
| `get_session_wake_schedules` | read | `get_session_batch_work` | read |
| `batch_status` | read | `batch_output` | read |
| `batch_stop` | run | `batch_add` | run |
| `batch_drain` | run | `cancel_wake_schedule` | run |
| `shutdown_server` | admin | `set_debug` | config |
| `remove_context_path` | admin | `get_skills` | read |
| `add_skills_path` | admin | `remove_skills_path` | admin |
| `remove_custom_provider` | admin | `set_provider_peak_hours` | admin |
| `update_todos` | run | `log_client_event` | run |
| `log_client_error` | run | `set_mcp_disabled` | admin |
| `remove_mcp_server` | admin | `set_lsp_disabled` | admin |
| `add_lsp_server` | admin | `remove_lsp_server` | admin |
| `update_lsp_server` | admin | `set_provider_key` | admin |
| `remove_provider_key` | admin | `add_mcp_server` | admin |
| `update_mcp_server` | admin | `add_context_path` | admin |
| `initialize_project` | admin | `add_custom_provider` | admin |
| `update_custom_provider` | admin | | |

On external listeners, admin commands additionally require listener `allow_admin`. Non-admin clients do not receive `mcp_state` events; the filter permits them only when both admin scope and listener admin enablement are present. Other event types are not withheld by this specific event filter. The `get_logs` command is admin-only because logs may contain prompts.

The serve connection cap is per token; token revocation/expiry closes the WebSocket with policy close code 1008. Authentication and upgrade failures use HTTP rejection (not a WebSocket close code). The UI listener is distinct and retains its existing behavior.

Derivation: `internal/server/scope_table.go`, `ws_serve_scope.go`, `ws_serve_origin.go`, `server.go` hello write and listener policy. Design comparison: plan §8 lines 199–214 describes header/subprotocol auth, hello and cap; it agrees on the broad behavior, but actual scope table includes all listed batch, LSP, message and configuration commands. Plan §17.5 lines 772–777 identifies `get_logs` and `mcp_state` disclosure; observed filter withholds `mcp_state` only, while `get_logs` is denied through its admin scope.