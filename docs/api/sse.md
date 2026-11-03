# SSE

> **Status:** Token revocation and expiry rechecks for open streams are required by the design and are being implemented. (being implemented)

The session event stream is `GET /api/v1/sessions/{id}/events` and requires `read` scope. Authenticate as for REST. Success has `Content-Type: text/event-stream`, `Cache-Control: no-store`, and disables proxy buffering. The stream begins with `retry: 3000` and uses standard SSE records:

```text
id: 42
event: run
data: {"state":"running"}

```

The event data is JSON; event IDs are monotonically allocated by the route-set event log. Events are scoped to a session and the ring retains at most 1,000 events per session (with a bounded 200-session log). This is a bounded replay ring, not durable event storage.

## Resume and termination

Send `Last-Event-ID: 42` to resume after that event. The current handler also accepts `last_event_id` as a query fallback when the header is absent. IDs must be unsigned decimal integers; malformed values return 400. If the requested cursor predates retained history, the server first emits `event: reset` with `{"reason":"history_truncated"}`, then emits retained later events. Consumers should treat reset as a gap and reconcile via REST.

If the subscriber queue overflows, an `overflow` event with `{}` is attempted and the connection ends; reconnect with the last processed ID. If the session is deleted, the server emits `closed` with `{"reason":"session_deleted"}` and ends. Every 15 seconds absent other traffic, a comment heartbeat `: keep-alive` is sent. Initial retry hint is 3 seconds.

At most 8 simultaneous streams are admitted per token; excess streams receive 429 and `Retry-After: 5`. Slots are released when a stream ends. The design requires token revocation/expiry to be rechecked while a stream is open and to close the stream; that behavior is **(being implemented)** and is not yet present in the inspected event handler.

```sh
curl --no-buffer -H "Authorization: Bearer $RUSH_API_TOKEN" -H 'Last-Event-ID: 42' "$BASE/api/v1/sessions/SESSION_ID/events"
```

The cursor is resumed after event 42; use the last event ID actually processed when reconnecting. `internal/server/api_events_route.go`, `api_events_handler.go` (including event names, heartbeat and per-token limit), and `api_events.go` (ring retention/replay). Design comparison: plan §7 line 184 promises heartbeat and replay; §4 lines 98–103 requires open streams to recheck token revocation. The handler inspected has no token recheck loop; revocation is marked **(being implemented)**.