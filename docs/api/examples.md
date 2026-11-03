# REST, SSE and WebSocket examples

These examples assume a `rush serve` REST/WS listener with production routes wired and that `RUSH_API_TOKEN` contains a token with required scopes. Use a placeholder token in documentation; never paste a real credential into source or commit it. `BASE` is the listener origin, for example `http://127.0.0.1:PORT`; production deployments should use TLS only when the server listener supports it. The current `rush serve` listener rejects TLS configuration, so its production URLs are HTTP and WS. The `internal/apiclient` can configure TLS for other compatible servers. The WebSocket endpoint path is `/ws`.

## curl

```sh
curl --fail-with-body -H "Authorization: Bearer $RUSH_API_TOKEN" "$BASE/api/v1/sessions?limit=20"
curl --fail-with-body -H "Authorization: Bearer $RUSH_API_TOKEN" -H 'Content-Type: application/json' -H 'Idempotency-Key: example-1' -d '{"prompt":"Summarize this workspace"}' "$BASE/api/v1/sessions/SESSION_ID/runs"
curl --fail-with-body -H "Authorization: Bearer $RUSH_API_TOKEN" "$BASE/api/v1/runs/RUN_ID?wait=10"
curl --no-buffer -H "Authorization: Bearer $RUSH_API_TOKEN" -H 'Last-Event-ID: 42' "$BASE/api/v1/sessions/SESSION_ID/events"
```

`SESSION_ID` and `RUN_ID` are placeholders from actual response bodies. The run request returns 202; poll or wait for its result. A repeated idempotency key must use the same request body.

## Go REST client

`internal/apiclient` is an internal package: this example is for Go code within the Rush module, not an external import promise. It assumes an already configured `apiclient.Client` whose `apiclient.Options` provide `BaseURL` and `Token`; do not hard-code a secret.

```go
ctx := context.Background()
page, err := client.ListSessions(ctx, apiclient.SessionsOptions{Limit: 20})
if err != nil {
    return err
}
if len(page.Items) == 0 {
    return nil
}
accepted, err := client.CreateRun(ctx, page.Items[0].ID,
    apiclient.RunRequest{Prompt: "Summarize this workspace"}, "example-run-1")
if err != nil {
    return err
}
run, err := client.WaitRun(ctx, accepted.RunID, 2*time.Minute)
if err != nil {
    return err
}
_ = run
```

Imports needed: `context`, `time`, and `github.com/PHPCraftdream/rush/internal/apiclient`. `CreateRun` generates a secure key if the key argument is empty; `WaitRun` may return a paused-run or timeout error. The client package is standard-library based and does not expose WebSocket commands.

## Go WebSocket

The gorilla/websocket dialer needs a `ws://` or `wss://` URL, not the HTTP `BASE` value. For the HTTP listener shown above, use `ws://127.0.0.1:PORT/ws`. The server selects/echoes `rush.v1`; the bearer subprotocol is sent only as a credential and should never be logged. This sample assumes no browser Origin header is sent.

```go
dialer := *websocket.DefaultDialer
dialer.Subprotocols = []string{"rush.v1", "rush.bearer." + token}
conn, resp, err := dialer.Dial(wsURL, http.Header{})
if err != nil {
    if resp != nil && resp.Body != nil {
        resp.Body.Close()
    }
    return err
}
defer conn.Close()
var hello map[string]any
if err := conn.ReadJSON(&hello); err != nil {
    return err
}
requestID := "request-1"
if err := conn.WriteJSON(map[string]any{
    "id": requestID, "type": "list_sessions", "payload": map[string]any{},
}); err != nil {
    return err
}
var response map[string]any
if err := conn.ReadJSON(&response); err != nil {
    return err
}
if response["id"] != requestID {
    return fmt.Errorf("response id mismatch: %v", response["id"])
}
```

Imports needed: `fmt`, `net/http`, and `github.com/gorilla/websocket`. The token variable is assumed to be loaded from a secret store. The hello envelope includes protocol, token id and scopes; command-specific response fields depend on the command.

Derivation: `internal/apiclient/api_sessions.go`, `api_runs.go`; WebSocket protocol and dispatch in `internal/server/protocol.go`, `server.go`, `ws_serve_auth.go`, and `ws_serve_scope.go`. The design §15 defers a WebSocket `rush api` client, so this Go sample is a direct protocol illustration, not an `apiclient` feature. The server listener currently supports cleartext HTTP/WS only; TLS listener behavior is proposed in [tls.md](tls.md).