# Module Registry (DRAFT)

**Status: DRAFT — produced after stage C (lane split into `internal/{kit,store,session,config,agent,app,serve}`).**
This registry becomes FINAL only after lane F confirms the layer boundaries. `internal/` currently contains exactly the seven layer directories: `kit`, `store`, `session`, `config`, `agent`, `app`, `serve`.

## Status definitions (plan section 6, decision B4)

- **frozen** — no change without an explicit `Unfreeze:` line in the PR/commit.
- **stable** — changes allowed; changes produce a review warning.
- **active** — changed occasionally (weeks-scale cadence).
- **hot** — changed daily / near-daily in the last 60 days.

Counts below are file-touch counts from `git log --since=60.days --name-only --pretty=format:` (files mentioned across commits touching the module path, current post-stage-C paths; pre-restructure history under legacy paths such as `internal/ktavcodec` is noted in Open questions).

## Registry

| module | status | purpose | public API | layer |
`internal/kit` | hot (1248) | Shared low-level utility library for all layers | aggregates sub-families below | kit
`internal/kit/text` | hot (921) | Text processing family: strings, ANSI, diffs, ktav codec | aggregates sub-packages below | kit
`internal/kit/text/stringext` | frozen (2) | Small string helpers (truncate, capitalize, base64) | `Truncate`, `TruncateEnd`, `TruncateAt`, `Capitalize`, `NormalizeSpace`, `IsValidBase64` | kit
`internal/kit/text/ansiext` | frozen (2) | ANSI escape handling | `Escape`, `TruncateDisplay` | kit
`internal/kit/text/format` | frozen (1) | UI formatting primitives (spinner) | `NewSpinner`, `Spinner` | kit
`internal/kit/text/diff` | frozen (1) | Unified diff generation | `GenerateDiff` | kit
`internal/kit/text/diffdetect` | frozen (2) | Detect unified-diff content in tool output | `Inspect`, `IsUnifiedDiff`, `Signal` | kit
`internal/kit/text/ktavcodec` | hot (913) | KTAV lossless config/text codec (JSON <-> KTAV) | `Tree`, `DecodeKtav`, `EncodeKtav`, `ToJSON`, `ParseJSONLossless`, `ConvertJSONToKtav`, `Equivalent` | kit
`internal/kit/fs` | active (43) | Filesystem family: globbing, paths, locks, home dir | aggregates sub-packages below | kit
`internal/kit/fs/fsext` | active (31) | File utilities, gitignore-aware globbing, atomic writes | `GlobGitignoreAware`, `FastGlobWalker`, `AtomicWriteFile`, `Expand`, `PrettyPath` | kit
`internal/kit/fs/filepathext` | frozen (1) | Path join/abs helpers tolerant of mixed separators | `SmartJoin`, `SmartIsAbs` | kit
`internal/kit/fs/filelock` | active (9) | Cross-platform advisory file locks + host probing | `AcquireFileLock`, `TryAcquireFileLock`, `AcquireFileLockContext`, `FileLock`, `ErrLockContended`, `ProbeShared` | kit
`internal/kit/fs/home` | frozen (2) | Home directory resolution and path shortening | `Dir`, `Config`, `Short`, `Long` | kit
`internal/kit/sync` | active (20) | Concurrency primitives family | aggregates sub-packages below | kit
`internal/kit/sync/csync` | active (12) | Generic concurrent collections (map/slice/queue/value) | `Map`, `Slice`, `LazySlice`, `KeyedQueue`, `Value`, `VersionedMap` + `New*` constructors | kit
`internal/kit/sync/pubsub` | frozen (3) | Generic typed pub/sub broker | `NewBroker`, `Broker`, `Subscriber`, `Payload` | kit
`internal/kit/sync/event` | active (5) | App lifecycle/telemetry event bus | `AppExited`, `AppInitialized`, `SessionCreated`, `PromptSent`, `PromptResponded`, `SessionDeleted` etc. | kit
`internal/kit/sys` | active (146) | OS/system integration family | aggregates sub-packages below | kit
`internal/kit/sys/platform` | active (12) | Platform process launching (window hiding, job objects, source-tree guard) | `Command`, `HideConsoleWindow`, `AssignToNewJobObject`, `GuardSourceTreeRun`, `IsInSourceTree` | kit
`internal/kit/sys/procinfo` | active (15) | Process liveness probing and kill | `Alive`, `Probe`, `StartToken`, `IsProcessAlive`, `KillProcess` | kit
`internal/kit/sys/proctrack` | active (10) | Tracking and tearing down spawned process trees | `TrackProcessTree`, `UntrackProcessTree`, `KillAllTrackedTrees`, `TrackedProcessCount`, `StartProcessUnlessStopping` | kit
`internal/kit/sys/shell` | active (106) | Embedded shell execution, background shells, command policy | `BackgroundShellManager`, `GetBackgroundShellManager`, `BackgroundExecution`, `ArgumentsBlocker`, `BannedCommands`, `CommandBlocked` | kit
`internal/kit/sys/shell/shexec` | active | Embedded sh interpreter exec handlers, process-group isolation | `ProcessGroupExecHandler`, `CoreUtilsExecHandler`, `UseGoCoreUtils`, `IsolateProcess`, `KillRetainedProcess` | kit
`internal/kit/sys/shell/shpolicy` | active | Shell command policy/blocking rules | `ArgumentsBlocker`, `CommandsBlocker`, `BannedCommands`, `CanonicalBlockFuncs`, `ParseCommandCandidates`, `CommandBlocked` | kit
`internal/kit/sys/env` | frozen (2) | Environment variable abstraction | `Env`, `New`, `NewFromMap`, `NewOverlay` | kit
`internal/kit/sys/dns` | frozen (1) | Android DNS workaround wiring | internal init hook (`android.go`, no exported API) | kit
`internal/kit/diag` | active (20) | Diagnostics family: logging, audit, version | aggregates sub-packages below | kit
`internal/kit/diag/log` | active (12) | slog-based logging, HTTP logging transport, goroutine dumps | `NewLogger`, `Initialized`, `RecoverPanic`, `NewHTTPClient`, `DumpGoroutines`, `WriteGoroutineDump`, `RetryTransport` | kit
`internal/kit/diag/audit` | active (6) | Append-only audit event files | `Write`, `Files`, `Prune`, `SetDirFunc`, `Event` | kit
`internal/kit/diag/version` | frozen (2) | Build version strings | `FullVersion`, `VersionLine` | kit
`internal/kit/dev` | active (38) | Developer tooling family (codegen, sync, test guard) | aggregates sub-packages below | kit
`internal/kit/dev/cahgen` | active (35) | CAH (catalog/skill asset) code generator pipeline | `Run`, `RefreshBuild`, `CheckCommitted`, `CheckInvariants`, `Acquire`, `Publish`, `Options`, `Dependencies` | kit
`internal/kit/dev/cahsync` | frozen (1) | CAH sync helper (package main utility) | `main` only | kit
`internal/kit/dev/testguard` | frozen (2) | Test isolation guard | `Main`, `Isolate` | kit
`internal/kit/tasktree` | active (60) | Task-tree batch execution spec/lab framework | `New`, `API`, `Run`, `REPL`, `Service`, `CheckEnvelope`, `ReadCheckpoint`, `ReadScenario` | kit
`internal/kit/tasktree/lab` | active (21) | Scenario/checkpoint codec and runner (lab harness) | `Run`, `REPL`, `Scenario`, `Checkpoint`, `Report`, `ReadCheckpoint`, `WriteCheckpoint` | kit
`internal/kit/tasktree/memory` | frozen (4) | In-memory task-tree store | `NewStore`, `Store` | kit
`internal/kit/tasktree/protocol` | active (12) | Task-tree tool protocol types and limits | `API`, `New`, `Backend`, `Invocation`, `ToolResult`, `MaxPayloadBytes`, `MaxPayloadDepth` | kit
`internal/store` | hot (311) | Persistence layer family | aggregates sub-packages below | store
`internal/store/db` | hot (256) | SQLite store: sqlc-generated queries and schema | `Queries` (+ `*.sql.go` row/params types), `db` facade | store
`internal/store/db/dbconn` | active (9) | DB connection pooling, compaction, integrity | `Connect`, `ConnectRead`, `Release`, `Compact`, `Stats`, `IntegrityCheck`, `CheckpointTruncate` | store
`internal/store/db/dbtest` | stable (6) | Test helpers for opening store DBs | `Open`, `OpenFK`, `OpenSingle`, `DSN`, `ApplyFinalizationFencingSchema` | store
`internal/store/db/migrater` | active (81) | Schema migrations and legacy store import | `OpenDB`, `OpenReadDB`, `ImportLegacyStore`, `LegacyFS`, `ErrLegacyImportFutureVersion` | store
`internal/store/db/sql` | active (21) | sqlc source `.sql` queries (embedded, no Go API) | consumed by `db` via sqlc codegen | store
`internal/store/db/storepath` | frozen (1) | Store file path helpers | `ActivePath`, `StoreV1Path`, `DataDirPath` | store
`internal/store/message` | active (49) | Message content model and CRUD over the store | `Message`, `MessageRole`, `TextContent`, `ToolCall`, `ContentPart`, `PromptWithTextAttachments`, `Service` | store
`internal/store/history` | frozen (3) | File history service (prompt/file snapshots) | `NewService`, `Service`, `File` | store
`internal/store/filetracker` | frozen (3) | Tracked-file registry service | `NewService`, `Service` | store
`internal/session` | hot (1119) | Session state, batch lifecycle, finalization, run queue | `Service` (session), `Batch*Store`, `AgentFinalizationStore`, `RoleContract`, `SessionExecutionRequirement`, finalization/queue types | session
`internal/session/heartbeat` | active (19) | Provider heartbeat/usage recording and context plumbing | `Init`, `AddUsage`, `RecordRequest`, `RecordFailure`, `FromContext`, `WithContext`, `DefaultDir` | session
`internal/session/jobbatch` | hot (152) | Job-batch engine: admission, execution, commit, errors | `Engine`, `NewEngine`, `Executor`, `ControlledExecutor`, `Batch`, `Limits`, `CommitFault` | session
`internal/session/jobbatch/lab` | active (95) | Batch lab harness: scenarios, replay, oracle, process exec | `LoadScenario`, `ValidateScenario`, `Replay`, `NewOracle`, `NewProcessExecutor`, `JournalFormat` | session
`internal/config` | hot (789) | Configuration store and API remote/token management | `Config`, `ConfigStore`, `APIRemote` + `Set/Get/ListAPIRemote*`, `DiscoverAnthropicModels` | config
`internal/config/catalog` | stable (5) | Embedded provider/model catalog constants | `LocalCLIProviderID`, `LocalCLIProviderType`, `LocalCLIModel`, `HyperName`, `HyperBaseURL`, `HyperEmbedded` | config
`internal/config/discover` | active (27) | Model discovery/enrichment per provider | `DiscoverModels`, `Resolver`, `Enricher`, `RegisterEnricher`, `ApplyModelFacts` | config
`internal/config/oauth` | active (14) | OAuth device-auth token flows | `NewClient`, `ExchangeToken`, `InitiateDeviceAuth`, `PollForToken`, `RefreshTokenFromDisk`, `Headers`, `Token` | config
`internal/config/oauth/codex` | stable (4) | Codex-specific OAuth endpoints | codex token/device variants of oauth API | config
`internal/config/oauth/copilot` | stable (7) | Copilot-specific OAuth endpoints | copilot token/device variants of oauth API | config
`internal/config/oauth/hyper` | frozen (1) | Hyper-specific OAuth endpoints | hyper token/device variants of oauth API | config
`internal/agent` | hot (4423) | Agent core: coordinator, turns, tools, batches, finalization | `Agent`, `Coordinator`, `New`, batch/finalization/heartbeat/prompt entry points (many files) | agent
`internal/agent/agentresult` | active (16) | Result/error spec compilation for agent outputs | `Spec`, `Compile`, `DecodeSpec`, `Compiled`, `Error`, `Code`, `Diagnostic` | agent
`internal/agent/provider` | active (133) | Provider layer glue (client building, CLI detection, re-exports) | `BuildHTTPClient`, `BuildTransport`, `ResolveNetworkConfig`, `Available`, `AvailableCLIModels` | agent
`internal/agent/provider/cliprovider` | active (65) | Local CLI provider (claude/codex CLIs) as fantasy.Provider | `New`, `ExternalMCPProxy`, `ExternalMCPTool`, `Available`, `CLISpec`, context keys | agent
`internal/agent/provider/codexprovider` | active (31) | Codex responses/WebSocket provider | `New`, `NewWithSource`, `TokenSource`, `WebSocket*` types | agent
`internal/agent/provider/hyper` | frozen (1) | Hyper hosted provider client | `New`, `Option`, `With*`, `ErrNoCredits`, `BaseURL` | agent
`internal/agent/provider/nettransport` | active (36) | HTTP/WebSocket transport for providers | `BuildHTTPClient`, `BuildTransport`, `DialResponsesWebSocket`, `ResponsesWSConn`, `ResponsesWSOptions` | agent
`internal/agent/runtime` | active (5) | Agent runtime contract bridge (call <-> session data) | conversion helpers (`FromSessionAgentCallData`, `ToSessionCallOptionsSpec`, ...) | agent
`internal/agent/runtime/contract` | active (5) | Contract package grouping for agentcall | groups `agentcall` | agent
`internal/agent/runtime/contract/agentcall` | active | Session-call <-> permission/role contract conversion | `SessionAgentCall`, `CallOptions`, `FromSession*`/`ToSession*`, `BuildSessionExecutionRequirement` | agent
`internal/agent/safety` | active (46) | Safety family grouping (guard, limits, notify, permission) | groups sub-packages | agent
`internal/agent/safety/agentguard` | active (20) | Command safety screening (banned cmds, git writes, window openers) | `Check`, `CheckAll`, `CheckArgs`, `CheckGitWrites`, `CommandBlockFunc`, `DeniedError` | agent
`internal/agent/safety/limitwords` | frozen (4) | Provider rate-limit/limit-word classification | `Classify`, `Input`, `Class`, `GeminiDetails`, `GroqRetryDelay` | agent
`internal/agent/safety/notify` | frozen (1) | Notification payload types for agent notices | `Notification`, `Type` | agent
`internal/agent/safety/permission` | active (21) | Permission service: folder scopes, run allowlists, session policy | `Service`, `PermissionRequest`, `FolderScope`, `BuildFolderScope`, `RunAllowlistSpec`, `RestrictedRun*Authorizer` | agent
`internal/agent/prompt` | active (32) | System prompt assembly and context loading | `prompt.go` API + re-exports of commands/skills (`LoadCustomCommands`, `DiscoverBuiltin`) | agent
`internal/agent/prompt/commands` | frozen (2) | Custom command and MCP prompt loading | `LoadCustomCommands`, `LoadSkillCommands`, `LoadMCPPrompts`, `GetMCPPrompt`, `CustomCommand` | agent
`internal/agent/prompt/skills` | active (9) | Skill discovery, embedding, filtering | `Discover`, `DiscoverWithStates`, `DiscoverBuiltin`, `BuiltinFS`, `Filter`, `Deduplicate`, `Skill` | agent
`internal/agent/tools` | hot (473) | Agent tool implementations (fs, bash, fetch, git, delegation) | `NewAskQuestionTool`, `AgentControl`, tool constructors (`fs_*`, `edit`, `fetch`, `bash`, `mcp_*`) | agent
`internal/agent/tools/hooks` | active (5) | Lifecycle hook runner (external hook scripts) | `NewRunner`, `Runner`, `HookResult`, `BuildPayload`, `HaltExitCode` | agent
`internal/agent/tools/mcp` | active (89) | MCP server lifecycle: init, admission, mutation, leasing | `Initialize`, `InitializeSingle`, `AddServer`, `ReplaceServer`, `EnableServer`, `DisableServer`, `WaitForInit`, `TextAdmission` | agent
`internal/agent/templates` | active (13) | Prompt template assets and rendering | embedded template data | agent
`internal/app` | hot (1204) | Application orchestration: run loop, reviewer, wiring | `App`, `New`, `LoopHost`, `BatchRuntime`, `NewBatchRuntime`, `Option` | app
`internal/app/projects` | frozen (3) | Project registry persistence | `List`, `Load`, `Register`, `Save`, `Project`, `ProjectList` | app
`internal/app/itest` | active (16) | End-to-end integration test suites (test-only package) | `doc.go` + `_test.go` suites only | app
`internal/serve` | hot (891) | Serve layer: API server, CLI, client, ops | aggregates sub-packages | serve
`internal/serve/server` | hot (398) | HTTP API server: routes, handlers, hub, WS/SSE events | `api_*` handlers/mux, `handlers.go` API, `Chain`, `NewAuth`, `NewRateLimiter`, `PrincipalFrom` | serve
`internal/serve/server/apiauth` | active (25) | API auth chain, rate limiting, redaction | `Chain`, `FingerprintAuth`, `Principal`, `PrincipalFrom`, `NewRateLimiter`, `Redact`, `RecordScope` | serve
`internal/serve/server/listener` | active (8) | Listener policies, TLS, token pinning | `ListenerPolicy`, `ValidateListenerSet`, `EffectiveListenerPins`, `ValidateEffectivePins`, TLS helpers | serve
`internal/serve/server/r15bfixture` | frozen (2) | Test fixture server (r15b) | test fixture binaries | serve
`internal/serve/server/wire` | active (8) | Wire protocol payloads for UI/server events | `EventCustomRoles`, `CustomRolesWire`, `PeakHours*Wire` payloads, `protocol.go` constants | serve
`internal/serve/server/wire/finalgate` | active (8) | Finalization-gated stream wrappers | `FinalizationGatedStream`, `FinalizationGatedAPIEventStream`, `FinalizationGatedMessageStream`, `FinalizationGatedSessionStream`, `GateFor` | serve
`internal/serve/client` | active (55) | Serve client grouping (API types + proto) | re-exports of `apiclient` API | serve
`internal/serve/client/apiclient` | active (35) | REST/WS client for the serve API | `Batch`, `BatchNode`, `TaskRequest`, `SessionsOptions`, `BatchTerminalSuccess`, `BatchPollInterval` | serve
`internal/serve/client/proto` | active (20) | Wire DTO conversion between client and message store | `Attachment`, `ContentPart`, `MarshalParts`, `UnmarshalParts`, `AttachmentFromMessage`, `MCPEvent` | serve
`internal/serve/cmd` | hot (430) | CLI command surface (sessions, serve, batch, tokens, worktree) | cobra-style command tree (`run_*`, `sessions_*`, `serve.go`, `schema.go`) | serve
`internal/serve/cmd/queue` | active (3) | Task queue service backed by the store | `NewService`, `NewServiceWithWorkspace`, `Service`, `Task`, `TaskStatus` | serve
`internal/serve/cmd/notification` | frozen (2) | Desktop notification backends | `Backend`, `Notification`, `NewNativeBackend`, `NoopBackend` | serve
`internal/serve/cmd/stats` | frozen (4) | Embedded stats dashboard web assets (no Go API) | embedded `index.html/js/css` | serve
`internal/serve/cmd/gitignore` | frozen (2) | Embedded default gitignore templates | embedded `default`, `old` files | serve
`internal/serve/ops` | active (8) | Operational tooling grouping (deploy/update) | re-exports of deploy/update | serve
`internal/serve/ops/deploy` | active (6) | Self-deploy/install path resolution and locking | `AcquireDeployLock`, `DeployLockPath`, `DefaultInstallPath`, `ClassifyDeployTarget`, `DeployTargetKind` | serve
`internal/serve/ops/update` | frozen (2) | Release update checking | `Check`, `Info`, `Release`, `Client`, `Default` | serve

## Open questions (judgement calls for the orchestrator)

All counts are 60-day file-touch counts from `git log --since=60.days --name-only`.

1. **Frozen picks on judgement** (no/1–4 touches in 60 days; chose the more protective `frozen` over `stable`): `internal/kit/text/stringext` (2), `ansiext` (2), `format` (1), `diff` (1), `diffdetect` (2), `internal/kit/fs/filepathext` (1), `home` (2), `internal/kit/sync/pubsub` (3), `internal/kit/sys/env` (2), `dns` (1), `internal/kit/diag/version` (2), `internal/kit/dev/cahsync` (1), `testguard` (2), `internal/kit/tasktree/memory` (4), `internal/store/db/storepath` (1), `internal/store/history` (3), `filetracker` (3), `internal/config/catalog` (5), `oauth/hyper` (1), `internal/agent/safety/limitwords` (4), `notify` (1), `internal/agent/prompt/commands` (2), `internal/agent/provider/hyper` (1), `internal/app/projects` (3), `internal/serve/server/r15bfixture` (2), `internal/serve/cmd/notification` (2), `stats` (4), `gitignore` (2), `internal/serve/ops/update` (2). Confirm each; any can be downgraded to `stable` with a single-word edit.
2. **Pre-restructure history**: `internal/kit/text/ktavcodec` (913 touches at its current path) and several other counts include legacy paths (e.g. old `internal/ktavcodec`, `internal/db`) before the stage-C move. If counts should exclude pre-move history, `ktavcodec`, `store/db`, and the hot layers should be re-derived and several statuses may drop from hot to active.
3. **Boundary-level rows**: rows for family directories (`internal/kit`, `internal/kit/text`, `internal/agent/safety`, `internal/serve/client`, ...) aggregate their children; confirm family-level rows should carry a status at all or only leaf rows.
4. **Test-only packages**: `internal/app/itest` (16) and `internal/store/db/dbtest` (6) are test infrastructure; they cannot be "frozen" without blocking all test changes — proposed `active`/`stable`, please confirm.
5. **Asset-only packages**: `internal/serve/cmd/stats`, `gitignore`, `internal/agent/templates` carry data, not Go APIs — status `frozen` assumes template edits should be deliberate; confirm.
