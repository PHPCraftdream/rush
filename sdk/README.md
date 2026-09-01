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
  connection. Nothing ever touches disk. A graceful `Close()` releases
  the in-memory handles, and the session data is gone with them. On a
  forced `Close()` — work that was still busy after cancellation, which
  includes background agent work (session title generation, cache
  keep-alive replays) and a busy run-queue pump worker, not just the
  calls you had in flight — the handles are deliberately left open and
  the data survives under them until you reclaim it with
  `CloseEphemeralConnsForced()` (see "Shutting down" below). Either
  way, once the handles are closed there
  is no way to recover the session. Use this for stateless, one-shot
  embedding (a request handler that spins up a
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
providers. Strict isolation is the default: the `smart` role is **required** in
`creds.Models`, and a `smart`/`fast` role the set doesn't cover is a
hard **error** before any provider traffic — Rush will not quietly
serve a missing role from the Client's configured providers. If you
genuinely want an uncovered role to fall back to the Client's own
configured model (e.g. `fast` for background title generation), set
`AllowConfiguredRoleFallback: true` on the `CredentialSet`. That is a
deliberate crossing of the tenant-credential boundary: the fallback
role runs on **your** (the operator's) provider, with whatever tenant
data that role carries.

```go
result, err := client.RunWithCredentials(ctx, sdk.RunRequest{
    Prompt:            "summarise the attached diff",
    ContinueSessionID: sessionIDForTenant(tenantID), // opaque, unguessable -- see the trust-model section below
}, sdk.CredentialSet{
    Credentials: []sdk.Credential{{
        Provider: "tenant-provider",
        Type:     sdk.ProviderTypeAnthropic,
        APIKey:   tenantAPIKey,
    }},
    Models: map[sdk.Role]sdk.ModelChoice{
        sdk.RoleSmart: {Provider: "tenant-provider", Model: "claude-opus-4-8"},
        sdk.RoleFast:  {Provider: "tenant-provider", Model: "claude-haiku-4-5"},
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
just picks its own `ContinueSessionID` and passes its own
`CredentialSet` on each call — no separate directory or sub-Client per
tenant is needed.

Pick the id as an unguessable value whose mapping to your callers *you*
own — the next section explains why.

## Trust model: there is no tenant authorization

Stated as plainly as possible because it matters: **Rush performs no
authorization and no ownership checks whatsoever.** Not on
`ContinueSessionID`, not on `Messages`, not on `Session`, not on the
subscribe streams.

- `RunRequest.ContinueSessionID` has get-or-create semantics keyed on
  the literal id. Any caller who knows — or guesses — an existing
  session id can continue that session, reading and writing its full
  history, with **any** credentials they pass. A per-call
  `CredentialSet` isolates *which provider serves the turn*; it is not
  an authentication mechanism and grants no exclusivity over the
  session.
- `Client.Messages(ctx, id)` and `Client.Session(ctx, id)` return any
  session's full history / metadata to whoever asks, no questions
  asked.
- `SubscribeMessages` / `SubscribeSessions` stream
  create/update/delete events for **every** session the Client knows
  about, with no tenant filtering. If you forward events to
  per-tenant consumers, filter the stream yourself on
  `ev.Payload.SessionID` before it leaves your process.

The host owns this boundary. A multi-tenant host should:

1. Generate session ids that are opaque and unguessable (a UUID or
   equivalent) — never predictable ids like `"tenant-42"`.
2. Keep its own ownership map (session id → caller) and consult it
   *before* handing a `ContinueSessionID` to Rush.
3. Filter subscribe streams per tenant before forwarding anything.

If you need enforced in-process tenant isolation today, put it in your
own authorization layer and only hand Rush ids that the caller is allowed
to touch.

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
  persists to query later. Call it before `Close()`: once a graceful
  `Close()` has released the in-memory handles, an ephemeral session's
  data is gone for good (a forced close keeps the data alive until you
  reclaim the handles — see "Shutting down").
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
returns the first call's result. It runs in three ordered phases:

1. **Admission closes.** From the instant `Close()` is called, new
   `Run`, `RunWithCredentials`, `Messages`, and `Session` calls return
   `sdk.ErrClientClosed`, and the Subscribe methods return an
   already-closed channel.
2. **Drain, cancelling on stall.** The calls admitted before that
   instant get one grace period (`agent.DefaultCancelAllGrace`, five
   seconds) to finish against the fully live App — a call that finishes
   inside the window is never cancelled. Work still running when the
   window expires is cancelled immediately, while every resource is
   still open: a run stuck on a non-cancellable provider or tool call
   unwinds here instead of blocking `Close()` forever. A run that
   unwinds once cancelled keeps the shutdown graceful; work that
   ignores cancellation entirely makes the shutdown forced, and
   `Close()` stops waiting — it returns within a bounded time (at most
   a couple of grace windows) instead of hanging. "Work" here is
   broader than the calls you admitted: even a drain that finishes
   cooperatively still runs one round of cancellation to join
   background agent work (session title generation, cache keep-alive
   replays) that no call was blocked on, and the run-queue pump is
   stopped with its own grace — either of those still busy after its
   grace period forces the shutdown too. After agent work has
   fully unwound, the residual wait for any remaining non-agent calls
   (a `Messages` read, which cancellation cannot reach) is unbounded: a
   host that needs a total bound should cancel the contexts it handed
   to its own in-flight calls.
3. **Release.** Bounded parallel cleanup always; the database — and, on
   an ephemeral client, the in-memory handles — only if the shutdown
   was graceful.

`CloseResult.Forced` is therefore `true` only when something was
*still busy after cancellation* — and that something is not limited to
the calls you had in flight: background agent work (session title
generation, cache keep-alive replays) that joined too late, or a
run-queue pump worker that ignored its own grace, forces the shutdown
with every admitted call long gone. A stuck-but-cooperative run produces a
graceful result: it is cancelled, it unwinds, and its state is flushed
before anything is released. A forced result means the database was
deliberately **not** released (closing it under a live writer risks
corruption), and on an ephemeral client the in-memory handles were
deliberately left open too: the session data is still alive under
those handles, held by the writer that refused to die. Once the host
knows every writer has finished, it reclaims the handles — and with
them the held memory — with `CloseEphemeralConnsForced()`; from that
call on, the ephemeral data is gone for good.
`CloseEphemeralConnsForced()` refuses to run until `Close()` has
*finished* — not merely started: while `Close()` is still draining,
calls it admitted earlier are still executing against the in-memory
handles, and a reclaim inside that window would close the database
under them. Before `Close()` is called at all the same refusal
applies, because the still-open Client would admit new calls against
closed handles. It is a no-op after a graceful close. `CloseResult.CleanupErrors` collects any non-fatal errors from
cleanup (also logged internally).

After `Close()` returns, the Client is permanently closed: `Run`,
`RunWithCredentials`, `Messages`, and `Session` return
`sdk.ErrClientClosed` (check with `errors.Is`), and `SubscribeMessages` /
`SubscribeSessions` return an already-closed channel. `Close()` itself
stays idempotent — the second call just returns the first call's result.

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

- **One application-mode `Client` per process.** MCP client state is
  process-wide (one registry keyed by server name, plus shared
  initialization-complete signaling), so two simultaneous
  application-mode Clients would share a single MCP layer rather than
  each owning one. Library mode never starts MCP servers, and multiple
  simultaneous library-mode Clients are supported and tested — each
  ephemeral client gets its own isolated in-memory database. Run one
  process per workspace for application mode, the same model `rush run`
  itself uses.
- **The host's logger is untouched unless you opt in** via
  `Options.SetupLogging` — and that call is itself a process-wide
  singleton, so only the first `Open` with `SetupLogging: true` in a
  process wins.
- Rush's own internal code still logs through package-level
  `slog.Info`/`Warn`/`Error` in many places; those lines go wherever the
  host process's current `slog.Default()` points, regardless of
  `SetupLogging`.
