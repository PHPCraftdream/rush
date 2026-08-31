# sdk — embed Rush as a Go library

`sdk` is the public, embeddable surface of Rush: the engine behind `rush
run` as a plain Go function call, no CLI process required. This document
is a practical guide for embedding it in your own Go program. Field-level
details live in the godoc comments on `sdk.go`/`library_mode.go` — this
file covers the scenarios and the things that aren't obvious from a
signature alone.

## Two ways to open a Client

### Application mode (the default)

```go
client, err := sdk.Open(ctx, sdk.Options{
    WorkingDir: "/path/to/project",
})
```

This is exactly what `rush run` does internally: `WorkingDir` is
required, and Rush discovers and loads `rush.json` (project, workspace,
global) and `.mcp.json` from it, the same way the CLI does. If
`WorkingDir` doesn't exist yet, `Open` creates it — it stays a required
parameter (there is no fallback to the process's own working directory
or a temp directory), but the caller doesn't have to `mkdir` a
freshly-provisioned workspace by hand first.

Use this mode when your host process manages one or more real,
persistent project directories — the credentials, models, and tool
settings all come from `rush.json` as usual.

### Library mode — explicit config, no files touched

```go
client, err := sdk.Open(ctx, sdk.Options{
    Mode: sdk.ModeLibrary,
    LibraryConfig: &sdk.LibraryConfig{
        Credentials: []sdk.Credential{{
            Provider: "my-provider",
            Type:     sdk.ProviderTypeOpenAICompat,
            APIKey:   apiKey,
            BaseURL:  "https://api.example.com/v1",
            Models: []sdk.CredentialModel{
                {ID: "my-model", ContextWindow: 200000, DefaultMaxTokens: 4096},
            },
        }},
        Models: map[sdk.Role]sdk.ModelChoice{
            sdk.RoleSmart: {Provider: "my-provider", Model: "my-model"},
        },
    },
})
```

`Options.Mode` defaults to `ModeApplication` (the zero value) — every
existing caller that never sets `Mode` is completely unaffected.
`ModeLibrary` skips config-file discovery entirely: no `rush.json`, no
`.mcp.json`, no global config. Every provider and model role comes from
`LibraryConfig` instead. `LibraryConfig.Models` must define at least the
`smart` role; the others (`fast`, `worker`, `reviewer`) are optional.

**`WorkingDir` is optional in this mode, and that choice matters:**

- **Omitted** → an ephemeral session backed by an in-memory SQLite
  connection. Nothing ever touches disk. The session is gone the moment
  `Close()` returns — there is no way to recover it afterward. Use this
  for stateless, one-shot embedding (a request handler that spins up a
  Client, runs one turn, and throws it away).
- **Given** → the directory is created if missing (same as application
  mode), and session data persists under `<WorkingDir>/.rush` — but
  `rush.json` is still never read. `LibraryConfig` remains the only
  config source; only the *persistence* differs from the ephemeral case.

Known v1 limitation of library mode: MCP servers are not started (no
`.mcp.json` to read from), and agent context files (`AGENTS.md`/`RUSH.md`)
are not loaded.

## `Run` vs `RunWithCredentials`

`Client.Run` is the "normal" call: model and provider come from however
the Client was opened (`rush.json` in application mode, `LibraryConfig`
in library mode).

`Client.RunWithCredentials(ctx, req, creds)` is for genuine multi-tenant
use: **one Client, many concurrent calls, each with its own provider
credentials.** Every field `creds.Models` covers gets resolved fresh for
that one call — a new provider client is built per call, never cached,
and nothing is read from or merged with the Client's own configured
providers. A role `creds.Models` doesn't cover falls back to the
ordinary resolution path.

```go
result, err := client.RunWithCredentials(ctx, sdk.RunRequest{
    Prompt:            "summarise the attached diff",
    ContinueSessionID: "tenant-42-session-7",
}, sdk.CredentialSet{
    Credentials: []sdk.Credential{{
        Provider: "tenant-provider",
        Type:     sdk.ProviderTypeAnthropic,
        APIKey:   tenantAPIKey,
    }},
    Models: map[sdk.Role]sdk.ModelChoice{
        sdk.RoleSmart: {Provider: "tenant-provider", Model: "claude-opus-4-8"},
    },
})
```

Two independent, concurrently-safe calls to `RunWithCredentials` (each
with its own `ContinueSessionID` and its own `CredentialSet`) on the
**same** `Client` never interfere with each other — this was verified
under `-race` specifically because the underlying `sessionAgent`/tool
machinery is shared coordinator-wide by default; per-call credentials
route around that sharing rather than depending on it.

`ModelChoice.Model` is **not** validated against `Credential.Models` —
an unrecognised model id fails on the first real provider call, exactly
like `rush run --model` does today. OAuth-based providers (e.g. GitHub
Copilot) are out of scope for `CredentialSet` — only a literal API key.

## Sessions and credentials are independent axes

`RunRequest.ContinueSessionID` identifies a conversation with
get-or-create semantics — an unknown id creates a new session with that
exact id, an existing one continues it. This is completely orthogonal to
credentials: a session doesn't "belong" to a `CredentialSet`. A tenant
just picks its own `ContinueSessionID` (e.g. `"tenant-42"`) and passes
its own `CredentialSet` on each call — no separate directory or
sub-Client per tenant is needed.

## Session-busy behaviour is different from `rush run`

`rush run` and the web server intentionally **queue** a second message
sent to a session that already has a turn in flight (this is what makes
"type a follow-up while the agent is still answering" work in the web
UI). `Client.Run` and `RunWithCredentials` do **not** queue: a second
concurrent call on the *same* `ContinueSessionID` fails immediately with
an error wrapping `agent.ErrSessionBusy` (check with `errors.Is`). Two
calls on *different* session ids remain fully concurrent and unaffected.

## Reading what the agent wrote

- **`RunResult.FinalText`** — the turn's last assistant message only.
- **`Client.SubscribeMessages(ctx)`** / **`SubscribeSessions(ctx)`** —
  live event streams (create/update/delete) across every session the
  Client knows about. Subscribe *before* starting the call you care
  about: the channel carries no backlog, so events that happened before
  you subscribed are gone.
- **`Client.Messages(ctx, sessionID)`** — the full message history of a
  session, in chronological order, on demand, *after* a call has already
  returned. This is the only way to retrieve history if you didn't
  subscribe in advance — and for an ephemeral in-memory session (see
  above), it is the *only* way to see history at all, since nothing
  persists to query later. Call it before `Close()`: once closed, an
  ephemeral session's data is gone for good.
- **`Client.Session(ctx, sessionID)`** — a session's metadata (title,
  token/cost counters), not its messages.

## Origin — where a session or message came from

Every session and every individual user message carries an `Origin`
(`sdk.OriginCLI` / `sdk.OriginWeb` / `sdk.OriginSDK`), set once at
creation. `Client.Run` and `RunWithCredentials` tag both with
`OriginSDK` by default — set `RunRequest.Origin` explicitly before
calling if you need something else. Session origin and message origin
are independent: a session opened through the SDK can still receive a
message injected later through `rush sessions inject` (CLI), and each
message keeps the origin of whatever actually created it.

## Shutting down

```go
defer client.Close()
```

`Close()` is idempotent — calling it more than once is safe and always
returns the first call's result. `CloseResult.Forced` is `true` when
some agent turn didn't finish within its grace period; in that case the
database is deliberately **not** released (closing it under a live
writer risks corruption), so a long-lived host process should treat a
forced close as "some in-flight work may not have been persisted,"
not as an ordinary clean shutdown. `CloseResult.CleanupErrors` collects
any non-fatal errors from cleanup (also logged internally).

## Minimal example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/PHPCraftdream/rush/sdk"
)

func main() {
    ctx := context.Background()

    client, err := sdk.Open(ctx, sdk.Options{WorkingDir: "./my-project"})
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close()

    result, err := client.Run(ctx, sdk.RunRequest{
        Prompt: "list the TODO comments in this repo",
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.FinalText)
}
```

## v1 boundaries

A few embedding limitations are stated honestly in `sdk.go`'s package
doc comment and worth repeating here:

- **One `Client` per process** for application/library MCP startup — MCP
  server initialization is guarded by a process-wide `sync.Once`, so a
  second `Open` in the same process won't start its own MCP servers.
  Run one process per workspace, the same model `rush run` itself uses.
- **The host's logger is untouched unless you opt in** via
  `Options.SetupLogging` — and that call is itself a process-wide
  singleton, so only the first `Open` with `SetupLogging: true` in a
  process wins.
- Rush's own internal code still logs through package-level
  `slog.Info`/`Warn`/`Error` in many places; those lines go wherever the
  host process's current `slog.Default()` points, regardless of
  `SetupLogging`.
