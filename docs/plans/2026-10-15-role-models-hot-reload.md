# Role-model hot reload: `rush models use` reaches every running instance within ~5 s

Date: 2026-10-15
Status: design, not implemented
Operator request: the OAuth token is already re-read from disk cheaply
through an in-memory cache. Do the same for the per-role model selections
(smart / fast / worker / reviewer). A `rush models use ...` must reach ALL
running instances (`rush run` processes, the web server, SDK clients)
within about 5 seconds, and new requests must go to the new models.

Legend: **OBSERVED** means read in the code at the cited line. **INFERRED**
means a conclusion drawn from observed code that no test or run confirmed.
**UNKNOWN** means not verified. All paths are repository-relative. Line
numbers refer to the tree this plan was written against (`d5c3b0d9`).

---

## 0. Executive summary

1. The pieces needed for "new requests use new models" **already exist**.
   Every turn resolves its models from the store's current snapshot,
   overlays the session's explicit per-session overrides, and caches built
   clients under a key that includes the config generation. What is
   missing is a **trigger**: today a running process reloads config from
   disk only as a side effect of the peak-hours check, and that check runs
   only while a turn is in flight.
2. Add one store-lifetime, read-only **config disk poller**
   (`internal/config/store_config_disk_sync.go`), built like
   `store_oauth_disk_sync.go`. It ticks every 5 s with ±20 % jitter. On
   each tick it stats the tracked config files. Only when a stat changes
   (and on every 12th tick regardless) does it read the files and compare
   content digests against the fingerprints recorded in the **published
   snapshot**. On a content change it runs the existing **full reload**,
   through an injected reload function (the App injects
   `mcpOwner.ReloadAndReconcileMCPConfig`).
3. The App starts the poller after `InitCoderAgent` and stops and joins it
   synchronously at the start of shutdown. This covers the web server,
   `rush run` and application-mode SDK clients without touching
   `sdk/sdk.go`, which is exactly at 1000 lines.
4. Two wiring fixes make the semantics correct: (a) `rush run --role
   fast|worker|reviewer` stops copying the role's default into the
   session's explicit smart slot and resolves the role slot live per
   turn; (b) a sub-agent dispatch resolves the worker slot from the live
   snapshot.
5. The web UI gets an `EventConfig` broadcast when a poll-driven reload
   changes the selected models.

---

## 1. Current data path

### 1.1 Where role selections are stored, and in what order they merge

| Source | Path | Written by | Evidence |
|---|---|---|---|
| system config | `systemConfigPath` (none on Windows) | operator | OBSERVED `internal/config/load_files.go:59` |
| global config | `GlobalConfig()` (`RUSH_GLOBAL_CONFIG` / `XDG_CONFIG_HOME`) | operator | OBSERVED `load_files.go:60` |
| **global data config** | `GlobalConfigData()` = `ConfigStore.globalDataPath` | `rush models use` (default scope `global`), web UI | OBSERVED `load_files.go:61`, `internal/config/store_write.go:30-31`, `internal/cmd/models_use.go:99` |
| project files | `rush.json` / `.rush.json`, walking up to the project boundary (outermost first) | operator | OBSERVED `load_files.go:76-95` |
| **workspace config** | `<Options.DataDirectory>/rush.json` (usually `.rush/rush.json`) = snapshot `workspacePath` | `rush models use --workspace` (scope flag) | OBSERVED `store_write.go:24-29`, `internal/config/store_reload.go:421-459` |
| per-invocation flags | `rush run --model/--fast-model/--role` | CLI | OBSERVED `internal/cmd/run.go:475`, `:538`, `:760`; `internal/app/app_run_setup.go:157-168` |
| per-session DB override | `sessions.smart_model_*`, `fast_model_*`, `worker_model_*`, `reviewer_model_*` | web `set_session_models`, `rush run --model` (persisted), session fork | OBSERVED `internal/session/session_update.go:202-244`, `internal/server/handlers_models.go:123-132`, `internal/agent/agent_run.go:589-609`, `internal/session/session_fork.go:124-137` |

The merge contract runs from lowest to highest priority: system, then
global config, then global data, then project files (outer to inner),
then the workspace file. OBSERVED: `load_files.go:91-95` ("lowest
priority first") and `store_reload.go:446-458`, which merges the
workspace file last. No environment variable overrides `models.*`
(OBSERVED: `internal/config/load_env.go` has no `Models` reference). After
the merge, `configureSelectedModels` fills catalog defaults (max tokens,
reasoning effort). If a configured model is in neither the catalog nor the
provider config, it **falls back to the provider default, in memory
only, with a warning**. OBSERVED: `internal/config/load_providers.go:850-901`,
fallback at `:870-872`.

`rush models use` writes all requested slots in one `SetConfigFields`
call. It then publishes in memory and runs `autoReloadAfterWrite`.
OBSERVED: `internal/config/store_models.go:81-107`; `store_reload.go:618-664`.

There is also a models-integrity rule: a `models` block without a valid
`cache_rev` signature counts as "edited outside rush". This is checked
**once at Load** for `globalDataPath` and `workspacePath`, and a mismatch
refuses process startup. OBSERVED:
`internal/config/settings_models_integrity.go:115-168`,
`internal/cmd/root.go:461-463`. A reload does **not** re-check it
(`settings_models_integrity.go:164-166`, "never recomputed by reload").

### 1.2 Where the merged snapshot is cached

- `ConfigStore.snap` is an `atomic.Pointer[storeSnapshot]`: an immutable
  snapshot with `config`, `generation`, `knownProviders`,
  `trackedConfigPaths` and per-path `snapshots` (file fingerprints).
  OBSERVED `internal/config/store.go:33-58`, `:87-89`.
- Every publication goes through `publishLocked`, which increments
  `generation`. OBSERVED `store.go:256-259`. Readers take `Snapshot()`
  for a consistent `(cfg, generation)` pair (`store.go:277-280`).
- A full reload records fingerprints (size, mtime, sha256 digest,
  identity, discovery chain) for every tracked path. Tracked paths are
  all config candidates, `.mcp.json` candidates, `workspacePath` and
  `globalDataPath`. OBSERVED `store_reload.go:538-540`. Candidates include
  **not-yet-existing** files (`load_files.go:186-193`), so a file that
  appears later is still detected.
- A reload rebuilds `cfg.Models` from disk and carries forward only
  `prev.overrides` (`SkipPermissionRequests`). OBSERVED
  `store_reload.go:527-537`. Any in-memory-only model mutation, such as
  `SetSelectedModelRuntime` (`store_models.go:123-131`), is therefore lost
  on the next reload. OBSERVED: no production caller of
  `SetSelectedModelRuntime` remains (grep). The `rush run --model` path
  uses context overrides instead.

### 1.3 Where the model is resolved: per request, per turn, per sub-agent

| Path | Resolution | Evidence |
|---|---|---|
| Turn without an explicit override (`Run`, drain, summarize, interrupt, reserved-ownership without overrides) | `resolveSessionModelsInternal`: `c.cfg.Snapshot()` → `cfg.Models[smart/fast]`, then overlay `sess.SmartModelID/FastModelID` when non-empty, then the model-pair cache keyed by **generation** + provider/model/effort | OBSERVED `internal/agent/coordinator_models.go:106-215`; callers `internal/agent/coordinator.go:536`, `agent_drain.go:84`, `coordinator_interrupt.go:532`, `coordinator_summarize.go:65` |
| Turn with explicit overrides (`RunWithOverrides`, ExecuteRun with `--model`, reviewer pass) | `applyModelOverrides(ctx, smart, fast)`: live snapshot for the defaults; an omitted slot inherits the session's durable override | OBSERVED `coordinator_models.go:357-450`, `coordinator_run.go:745-780`, `:866-885` |
| Pinning | The resolved models, prompt and tools are pinned onto the `SessionAgentCall`; an in-flight turn never re-reads shared model state | OBSERVED `coordinator_models.go:456-476` |
| Sub-agent (`agent` tool) | The sub-agent is built inside the parent call's per-call toolset (`pinCallTools` → `buildTools` → `buildAgent`) from the parent's **pinned** cfg; worker slot when `workerSubAgentActiveForCall`. At dispatch: per-call credentials, else the session's `WorkerModelID`, else the model built at the parent's turn start | OBSERVED `coordinator_models.go:610-645`, `:794-796`; `internal/agent/coordinator_subagents.go:152`, `:186-193` |
| Reviewer pass | `Config().Models[reviewer]` read live when the pass is built, then passed as a call override | OBSERVED `internal/app/app_run_reviewer.go:890-906` |
| `rush run --role X` | `foldRoleModel`: for fast/worker/reviewer without `--model`, the session's worker/reviewer pin or `Config().Models[X]` is turned into a **`provider/model` string in the smart slot** | OBSERVED `internal/cmd/run_role_model.go:59-76`, `internal/cmd/run.go:760` |
| ExecuteRun override lifetime | A requested override is armed on ctx for **every turn of the invocation** and persisted into the session's smart/fast slots on the first turn | OBSERVED `app_run_setup.go:157-168`, `:419-427`; `agent_run.go:589-609` |
| Shared default agent | `UpdateModels` rebuilds `currentAgent` models and tools from the latest snapshot. It is called only after a credential refresh | OBSERVED `coordinator_models.go:930-958`; `internal/agent/coordinator_providers_auth.go:250`, `:267`; `internal/app/app_agent_setup.go:22-27` |

### 1.4 What a running process does today when another process runs `rush models use`

- The writer's store publishes immediately. Other processes have their
  own `ConfigStore`. Nothing watches the files **except** the peak-hours
  check.
- Every agent built by `buildAgent` gets a non-nil `PeakHoursCheck`
  closure (OBSERVED `internal/agent/coordinator_tools.go:133-135`). It
  runs every `peakHoursPollInterval = 10s` during a turn
  (`internal/agent/agent.go:172`, `agent_turn_peakhours.go:73-90`) and
  once per step (`agent_turn_step.go:786`). `checkLivePeakHours` calls
  `ConfigStaleness()`. If any tracked file's fingerprint differs, it runs
  `mcpOwner.ReloadAndReconcileMCPConfig`, which is a full
  `ReloadFromDisk` (OBSERVED `coordinator_providers_auth.go:96-116`,
  `internal/agent/tools/mcp/uncertainty.go:129-150`).
- Result (INFERRED from the paths above; no test asserts it):
  - **A process with a turn in flight** reloads within about 10 s or at
    the next step. Its **next** turn then sees the new `cfg.Models` and a
    new generation, so the cache misses and new clients are built. The
    in-flight turn keeps its pinned models. Sessions with an explicit
    override keep their override.
  - **An idle process** (the web server between messages, an SDK client
    between calls, `rush run` waiting on a job) **never reloads**. Its
    first new turn after the change resolves from the **stale** snapshot.
    That turn starts a peak-hours watcher, which reloads about 10 s
    later, so the **second** turn is correct. The web UI's config view
    (`EventConfig`) stays stale until something in-process broadcasts
    (`internal/server/handlers_config.go`, `handlers_models.go:202-204`).
  - `rush run --role worker|fast|reviewer`: the role default is folded
    at CLI start into a fixed override for the whole invocation and
    persisted into the session's smart slot. It never picks up a change
    during the invocation, and later web turns on that session keep the
    copied model (see §2.2).
  - Sub-agents: the worker model is fixed at the parent turn's start, so
    a long orchestrator turn keeps dispatching on the old worker model.
- Also OBSERVED: the OAuth poller is started only by `setupApp`
  (`internal/cmd/root.go:457`). The SDK's `openApplication`
  (`sdk/sdk.go:442-470`) and `setupAppLite` (`root.go:539`) do not start
  it. Its only stop/join, `stopOAuthJournalWorkers`
  (`internal/config/store_oauth_journal_worker.go:37-45`), has **no
  production caller** (test-only; the integration test says "No public
  poller join exists", `internal/agent/task1314_oauth_disk_integration_test.go:181`).

---

## 2. Semantics to implement

### 2.1 Rules

1. **In-flight requests are never changed.** This already holds: models,
   prompt and tools are pinned per call (`coordinator_models.go:456-476`),
   and a reload only publishes a new snapshot without touching pinned
   `Model` values. Durable run-queue rows replay their own persisted
   `CallOptionsSpec`/model config (OBSERVED
   `internal/session/session_runqueue.go:138-160`). A queued call that
   was already resolved stays on its resolved models. INFERRED: this is
   acceptable as "in flight".
2. **The next request of a running session uses the new role model**,
   unless the session has an **explicit** per-session choice for that
   slot. This already holds once the snapshot is fresh
   (`coordinator_models.go:118-136`). The poller provides the freshness.
3. **An explicit choice always wins**: web `set_session_models`, `rush run
   --model/--fast-model` (persisted), SDK `RunOverrides.SmartModel`,
   per-call `CredentialSet`, and session forks of those.
4. **A role-derived value is not an explicit choice.** It must be
   resolved live and must not be persisted (see §2.2).

### 2.2 Is a session's persisted model an override or a copy?

- Session creation copies **nothing**. OBSERVED:
  `internal/session/session_lifecycle.go:47` `Create(ctx, title)`, and no
  code path in `internal/session` sets `SmartModel*` except
  `UpdateModels` (`session_update.go:202-223`) and fork
  (`session_fork.go:124-137`). An empty `smart_model_id` means "inherit"
  (`internal/db/sql/sessions.sql:52-64`).
- Explicit writers: web `set_session_models` (user action,
  `web/src/store.ts:591-607`), ExecuteRun persistence of
  `--model/--fast-model` (`app_run_setup.go:162-168`,
  `agent_run.go:589-609`). Both are deliberate choices.
- **The one copy:** `rush run --role fast|worker|reviewer` without
  `--model` goes through `foldRoleModel` (`run_role_model.go:59-76`).
  That function turns `Config().Models[role]` (or the session's
  worker/reviewer pin) into a `provider/model` string in the **smart**
  slot. ExecuteRun then treats it exactly like `--model`: it is armed for
  every turn of the invocation and persisted into
  `sessions.smart_model_*` (`app_run_setup.go:157-168`, `:419-427`). From
  then on the session looks as if the operator had explicitly pinned
  that model. Any later web turn, or `rush run --role smart --session S`,
  stays on it after a role change.
- Proposed distinction (minimal state change, **no migration**). A role
  fold produces **no** model override and **no** persistence. The role
  already travels as `CallOptions.ModelRole` (`app_run_setup.go:372-374`)
  and is already persisted in durable rows
  (`session_runqueue.go:147-151`). Add one resolution rule in the agent
  layer: when `CallOptions.ModelRole ∈ {fast, worker, reviewer}` and the
  call has no explicit smart override, the smart slot is
  `sess.<Role>ModelID` if set, else `cfg.Models[role]` from the call's
  pinned snapshot, with `fast` using the session's `FastModelID`. This
  moves `foldRoleModel`'s cascade (session pin, then config default) from
  the CLI into `resolveSessionModelsInternal`/`applyModelOverrides`, so
  it is evaluated **per turn against the live snapshot**. `foldRoleModel`
  keeps only its validation ("no worker model configured" error).
- Existing sessions that already hold a copied smart model cannot be told
  apart from explicit pins: no marker exists (UNKNOWN whether any
  heuristic is safe). They stay pinned. The operator clears them through
  the web "clear override" (`clearSessionModelSlot`,
  `web/src/store.ts:618-643`).
- Alternative needing a migration (operator decision D2): add a nullable
  `sessions.smart_model_role` column that records "smart slot follows
  role X" for web continuation of a CLI `--role worker` session. Not
  recommended. `--role` is mandatory on every `rush run`
  (`run.go:535-537`), so the CLI re-declares the role on every
  invocation anyway.

### 2.3 Per role

- **smart / fast**: live via `resolveSessionModelsInternal` once the
  snapshot is fresh. No code change beyond the poller.
- **worker (sub-agent)**: a dispatch is a new request. Proposed: at
  dispatch time in `runSubAgent`, when there are no per-call credentials
  and no session `WorkerModelID`, and the model came from the config
  slot (heartbeat source `hbSourceSlotStart`), compare
  `params.Agent.Model().ModelCfg` (provider, model, effort) with the
  **live** `cfg.Models[worker]` (or smart, when worker is inactive). On a
  mismatch, rebuild through `buildModelsFromCfg`. The parent turn's
  pinned prompt and toolset stay as they are (the "worker available"
  flag is decided at turn start). Operator decision D3: dispatch-time
  (recommended) or parent-turn-time (today).
- **reviewer**: already read live per pass (`app_run_reviewer.go:891`).
  It becomes correct once the snapshot is fresh.
- **`rush run --role fast|worker|reviewer`**: §2.2. Each turn of the
  invocation (follow-ups, drains, rebuilt durable calls) re-resolves the
  role slot from the live snapshot.
- **`rush run --model X`, `--fast-model`**: explicit. Unchanged: fixed
  for the invocation and persisted.
- **Per-call `CredentialSet` (SDK)**: explicit tenant models. Unchanged.

### 2.4 Web UI notification

When a poll-driven reload changes the effective `cfg.Models`, the server
broadcasts `EventConfig` (`buildConfigWire`, which already carries
`Models`, `internal/server/handlers_config.go:64-79`). Sessions that
inherit their models display the new defaults. No new event type and no
toast; a toast is a non-goal.

---

## 3. Design

### 3.1 Full reload or a light models-only reload: **full reload**

Reasons:

1. `configureSelectedModels` depends on the whole merged config:
   provider entries, catalog, default-model fallback, unverified
   passthrough (`load_providers.go:850-901`). A models-only path would
   have to re-implement the 5-tier merge (`load_files.go:53-96`,
   `store_reload.go:421-459`) and provider validation. That would be a
   second source of truth for precedence.
2. `buildAndPublishReload` already provides stable reads, ABA rejection,
   generation and disk-write CAS, the pending-OAuth overlay, and MCP
   uncertainty bookkeeping (`store_reload.go:401-582`). The peak-hours
   path already triggers it on staleness. The poller adds **a trigger,
   not new reload semantics**.
3. Through `ReloadAndReconcileMCPConfig`, MCP invariants INV-15 and
   INV-24 (`docs/mcp-invariants.md`) already guarantee that an unrelated
   or no-op reload does not disturb published MCP sessions.

Cost: a reload re-runs `configureProviders`, which resolves `$(...)` API
key templates up to `resolveTimeout` each. It runs only when a tracked
file's **content** changed. Expected frequency: model changes, recent-model
writes from the web UI, and OAuth rotations by other processes, roughly
hourly per provider. Risk R4.

### 3.2 What is fingerprinted, and how

- Path set: the published snapshot's `trackedConfigPaths` (§1.2). This
  includes `globalDataPath` and `workspacePath` (both scopes of `rush
  models use`), project candidates (including absent files) and
  `.mcp.json` candidates.
- Two levels, mirroring the OAuth poller
  (`store_oauth_disk_sync.go:79-90`):
  1. **Cheap stat probe** on every tick: `os.Stat` per path, compared
     with the poller's own last stat (`os.SameFile`, size,
     `ModTime().Equal`). If nothing differs and it is not a forced tick,
     the tick does nothing.
  2. **Exact check** when the stat changed, or on every 12th tick (about
     60 s, as in the OAuth poller, for coarse-mtime filesystems and
     same-size rewrites): `readReloadFingerprint(path)` (stable read plus
     sha256, `store_reload.go:692-698`), compared with
     `sn.snapshots[path].fingerprint` on **`exists` and `digest` only**.
     Metadata-only changes (mtime, inode from an atomic rewrite with
     identical bytes, nlink) do **not** trigger a reload. The forced tick
     also checks that `lookupConfigCandidates(workingDir)` +
     `mcpJSONCandidatePaths` still equals the tracked set
     (`sameReloadPathSet`, `store_reload.go:741-759`), for example a new
     `rush.json` in a parent directory or a changed project boundary.
- It is compared against the **published snapshot**, not against the
  poller's previous read. That makes the poller level-triggered. After an
  in-process write, `autoReloadAfterWrite` has already published fresh
  fingerprints, so the next tick sees equal digests and does not reload
  (dedup for free). If an `autoReload` was skipped by `TryLock`
  (`store_reload.go:604-611`, the documented "missed write" window), the
  digests still differ on the next tick and the poller reloads. This
  closes that pre-existing gap.
- The poller **never publishes on its own**. No `RefreshStalenessSnapshot`
  (`store_staleness.go:89-119` publishes a generation and would flush
  every model cache).

### 3.3 Interval and jitter

`configDiskSyncInterval = 5 * time.Second`. Each delay is `interval ±20 %`
(4–6 s, `oauthDiskSyncDelay` shape), and the first delay is uniform in
`[0, interval)` so N processes do not tick in lockstep. Worst-case
detection is 6 s plus reload time. If "within 5 s" must be strict, use a
4 s interval, which gives at most 4.8 s (operator decision D1).

### 3.4 Lifecycle

- New store fields, kept **separate** from the OAuth worker fields
  (`store.go:142-148`): `configSyncMu`, `configSyncCtx`,
  `configSyncCancel`, `configSyncWG`, `configSyncStarted`,
  `configSyncOpts`.
- `func (s *ConfigStore) StartConfigDiskSync(opts ConfigDiskSyncOptions)`
  is idempotent. It is a no-op when `workingDir == ""` (library store,
  `NewLibraryStore` `store.go:663-673`, or test store) or when stopped.
- `func (s *ConfigStore) StopConfigDiskSync()` cancels and **joins**
  (`configSyncWG.Wait`). It is idempotent, and a later Start after Stop
  is refused (as with OAuth `ctx.Err()`, `store_oauth_disk_sync.go:34-36`).
- Owner: the **App**. `app.New` starts the poller right after a
  successful `InitCoderAgent` (`internal/app/app.go:508`). That covers
  `rush` (web), `rush run`, and SDK application mode (OBSERVED
  `sdk/sdk.go:511` calls `app.New`; library mode also calls it at
  `sdk/library_mode.go:316`, but its `NewLibraryStore` has no disk paths,
  so Start is a no-op). It does **not** cover
  `setupAppLite` commands (`skipAgentSetup` returns earlier,
  `app.go:497-500`), which is correct because they are short-lived.
  Library mode is a no-op because the store has no working dir and no
  disk.
- Stop: synchronously at the top of `releaseResources`, **before** the
  concurrent `cleanupFuncs` loop (`internal/app/app_lifecycle.go:351-360`
  runs them in parallel, including `mcpOwner.Close`). No reload can then
  run against a closing MCP owner. The reload function is called with
  the poller's ctx, so cancellation aborts an in-flight shell
  substitution (`store_reload.go:487-497`).
- Why not reuse the OAuth poller's ctx/WG: `stopOAuthJournalWorkers` also
  cancels the OAuth journal workers, and it has no production caller. A
  shutdown path that joins the config poller must not start owning OAuth
  journal teardown as a side effect. Wiring the OAuth poller's own stop
  into shutdown is a separate decision (D6).

### 3.5 One poller or two (relation to the OAuth poller)

**Two pollers.** The OAuth poller (a) runs only when an OAuth provider is
configured (`store_oauth_disk_sync.go:69-78`), (b) adopts tokens through
a targeted COW publish with rotation authority
(`adoptCodexDiskAuthority`, `applyOAuthToken`) that must keep working
**even when a full reload fails** (for example invalid JSON in the
workspace file), and (c) reads one file. Merging the two would couple
credential freshness to reload health. The cost of two pollers is one
extra `os.Stat` per tracked file every 5 s.

Interaction: when another process rotates a token, the OAuth poller
adopts it (new generation), and the config poller then sees a digest
change on `globalDataPath` and runs a full reload (another generation).
This is redundant but correct: the reload reads the same token, and
`overlayPendingOAuthLocked` (`store_reload.go:573`) protects in-process
pending rotations. Possible later optimization (P3 backlog, not in this
work): skip the reload when the only changed keys are `providers.*.oauth`.

### 3.6 Concurrency of publication

- The poller is one goroutine, so ticks are serial. It holds no store
  lock between ticks.
- The reload takes `reloadMu`, then `publishMu`, then `diskWriteMu`
  (`store.go:189-197`) and publishes only if neither the snapshot
  generation nor the disk-write generation moved
  (`store_reload.go:556-565`). Concurrent in-process writers and the
  peak-hours reload are therefore safe. A losing candidate retries
  (`runReloadLocked`, `store_reload.go:344-386`).
- Readers pick up the new generation lock-free on their next
  `Snapshot()`. In-flight calls hold pinned values.
- Change notification happens after the reload returns. The poller
  compares `before.config.Models` (captured before the reload) with the
  current `Snapshot()` using `reflect.DeepEqual` over the four slots plus
  the referenced providers' `ID`/`Type`. If they differ, it calls the
  `OnModelsChanged` callbacks synchronously on the poller goroutine.
  Callbacks must be cheap or hand off to their own goroutine. A
  concurrent in-process write can make a callback fire for a change the
  process made itself. Callbacks are idempotent (re-broadcast, re-check),
  so that is harmless.

### 3.7 Provider client rebuild cost, and when to rebuild

- Turn clients: rebuilt lazily. The model-pair cache key includes the
  generation (`coordinator_models.go:189-215`), so the first turn per
  distinct `(smart, fast)` pair after a new generation builds clients
  once. Because the poller reloads **only on content change**, unrelated
  ticks cause no rebuild. A content change in an unrelated part of the
  file (recent models, MCP) still bumps the generation and costs one
  client build per active pair. That is acceptable: `buildProvider` is
  in-process construction, and CLI providers (`cliprovider`) build a
  struct with no subprocess until `Stream` (INFERRED from
  `internal/agent/cliprovider/provider.go` naming; verify).
- Shared default agent (`currentAgent`): refresh with
  `coordinator.UpdateModels` **only when `OnModelsChanged` fires** (the
  selected models or their providers changed), never on every reload.
  `UpdateModels` also rebuilds the shared toolset (`buildTools`), which
  is the expensive part. Whether any production turn still reads the
  shared agent's models without a pin is UNKNOWN; every entry point
  listed in §1.3 pins. If phase B proves no unpinned reader exists, drop
  this call and refresh only `Model()`. `Model()` already reads live
  (`coordinator_models.go:902-920`).
- Sub-agents (§2.3): rebuild a sub-agent's model at dispatch only when
  the live slot differs from the built model.

### 3.8 Failure handling

- A stat error, for example a Windows sharing violation during an atomic
  replace: treat the tick as unsettled, do nothing, and retry next tick
  (same as OAuth `st.settled = false`, `store_oauth_disk_sync.go:80-84`).
- A reload error (invalid JSON, `ErrConfigReloadUnstable`, hook
  validation, provider configuration): the current snapshot stays
  published, because a failed `buildAndPublishReload` never publishes.
  The poller remembers the **failed digest set** and does not retry
  until a digest changes again. It logs `slog.Warn` **once** per
  distinct failed digest set, and logs `slog.Info` once on recovery.
- A partial write (half-written file) is prevented by the atomic-rename
  writers (`internal/config/atomicwrite.go`) and by
  `readStableConfigFile`'s double read (`store_reload.go:60-154`). A torn
  read simply fails the digest comparison and is retried.
- **Models integrity (P1 requirement of this design).** Today a running
  process silently adopts a hand-edited `models` block if a peak-hours
  reload happens to run, while a fresh process refuses to start
  (§1.1). The poller would make that adoption systematic within 5 s,
  which is a settings-lock bypass for any agent that can write
  `.rush/rush.json`. The reload candidate must therefore re-verify
  integrity on the **candidate's stable bytes** for `globalDataPath` and
  `workspacePath`. Refactor `verifyModelsIntegrityAtLoad` into a pure
  `modelsIntegrityOf(docs, lockExists) error`. On a mismatch, the
  candidate keeps `prev.config.Models` for the four role slots, the rest
  of the candidate publishes, and a warning is logged once. This
  applies to **every** reload path, peak-hours included, because it
  lives in `buildAndPublishReload`. D4: keep the old models (recommended)
  or reject the whole candidate.
- Unknown model id on disk: `configureSelectedModels` falls back to the
  provider default in memory with a warning (`load_providers.go:870-872`).
  This behavior is unchanged. `rush models use` validates before
  writing, so this mainly concerns hand edits, which the integrity rule
  above already blocks.

---

## 4. File-level implementation plan

All target files stay under 1000 lines. Files that are close to the limit
are marked ⚠.

### Package A: `internal/config` poller, with unit tests

| File | Lines now | Change |
|---|---|---|
| `internal/config/store_config_disk_sync.go` (new) | 0 → ~220 | `ConfigDiskSyncOptions{Reload func(context.Context) error; OnModelsChanged func(ModelsChange)}`, `StartConfigDiskSync`, `StopConfigDiskSync`, `PollConfigDiskOnce(ctx) (ConfigDiskSyncResult, error)` (exported: one synchronous tick with a forced exact check; used by cross-package tests and as a "refresh now" API), `runConfigDiskSync`, `syncConfigFromDisk(ctx, *configDiskSyncState)`, `configDiskSyncDelay()`. Package vars `configDiskSyncInterval`, `configDiskSyncWait = waitCodexRefresh`, `configDiskSyncStat = os.Stat`, `configDiskSyncAfterTick func(*ConfigStore, ConfigDiskSyncResult)` (test hook). The default `Reload` is `s.ReloadFromDisk`. |
| `internal/config/store.go` | 715 | +~10: the `configSync*` fields next to the OAuth worker block (`:142-148`). |
| `internal/config/settings_models_integrity.go` | 169 | Extract the pure `modelsIntegrityOf(...)`. `verifyModelsIntegrityAtLoad` calls it, and its behavior is unchanged. |
| `internal/config/store_reload.go` | 818 | +~15 in `buildAndPublishReload`: after the documents are read (`:460`), run the integrity check on the `configDocuments` bytes for the two paths. On a mismatch, set `cfg.Models` to the previous raw selections before `configureSelectedModels` (`:517`), then log once. |
| `internal/config/store_config_disk_sync_test.go` (new) | ~350 | Tests below. |

Unit tests (deterministic; no sleeps). Ticks are driven by replacing
`configDiskSyncWait` with a channel-fed wait, and results are observed
through `configDiskSyncAfterTick` or by calling `syncConfigFromDisk`
directly with a state value (OAuth-test style,
`store_oauth_disk_sync_test.go:50-110`). Every test uses
`isolateAllGlobalConfigPaths(t)` (`internal/config/testutil_isolate_test.go:28`).

1. `TestConfigDiskSyncReloadsOnForeignModelsWrite`: two stores on one
   data dir. Store B runs `UpdatePreferredModels(ScopeGlobal, worker=X)`.
   One tick on A gives `A.Config().Models[worker] == X` and a higher
   generation. *Catches:* a missing trigger. *Revert-check:* stubbing
   `Reload` to a no-op fails the test.
2. `...WorkspaceScope`: the same with `ScopeWorkspace`. *Catches:*
   `workspacePath` missing from the tracked set.
3. `...NoReloadOnMetadataOnlyChange`: rewrite identical bytes (new
   inode, new mtime); the tick does not call `Reload` (counted) and the
   generation is unchanged. *Catches:* generation churn that flushes the
   model caches.
4. `...NoReloadAfterInProcessWrite`: A writes itself
   (`autoReloadAfterWrite` publishes); the next tick makes zero `Reload`
   calls. *Catches:* duplicate reloads.
5. `...RecoversSkippedAutoReload`: hold `reloadMu` while B writes, so
   A's `autoReload` `TryLock` skips; release, tick, and the change is
   adopted. *Catches:* the lost-update window at `store_reload.go:604-611`.
6. `...InvalidJSONKeepsSnapshotAndLogsOnce`: corrupt the workspace file;
   three ticks keep the same generation and make exactly one `Reload`
   attempt and one warning (log capture); after fixing the file, one
   reload and recovery. *Catches:* retry storms and snapshot loss.
7. `...ForcedTickCatchesSameStat`: set `configDiskSyncStat` to return a
   frozen `FileInfo` while the bytes change; ticks 1–11 do not reload
   and tick 12 does. *Catches:* missing coarse-mtime protection.
8. `...NewCandidateFileAppears`: create a project `rush.json` after Load;
   a forced tick reloads. *Catches:* path-set drift.
9. `...OnModelsChangedOnlyOnModelDiff`: a recent-models write fires
   `Reload` but not `OnModelsChanged`; a `models.smart` write fires both.
10. `TestReloadRejectsUnsignedModelsEdit`: hand-edit `models.smart` in
    the workspace file without `cache_rev` while a settings lock exists;
    after `ReloadFromDisk`, `Models[smart]` is unchanged and the rest of
    the candidate (for example `peak_hours`) is adopted. *Catches:* the
    settings-lock bypass (§3.8).
11. `TestConfigDiskSyncLifecycle`: Start twice gives one goroutine; Stop
    joins (`goleak.VerifyNone(t, goleak.IgnoreCurrent())`, as used in
    `internal/agent/wake_scheduler_test.go:449`); Start after Stop is a
    no-op; Stop during a reload blocked in `reloadResolverExpander`
    returns with ctx cancelled and leaks no goroutine.
12. `...LibraryStoreNoop`: `NewLibraryStore(...).StartConfigDiskSync`
    starts nothing.

### Package B: wiring, so new requests use new models

| File | Lines now | Change |
|---|---|---|
| `internal/app/app.go` | 579 | After `InitCoderAgent` succeeds (`:508`): call `app.startConfigDiskSync()` (+~3). |
| `internal/app/app_config_sync.go` (new) | ~90 | `startConfigDiskSync`: builds options with `Reload: func(ctx) error { return app.mcpOwner.ReloadAndReconcileMCPConfig(ctx, app.config) }` (nil-owner safe; `Owner` has a nil receiver, `uncertainty.go:129-132`). `OnModelsChanged` fans out to registered listeners (server) and calls a best-effort, timeout-bound `AgentCoordinator.UpdateModels` on its own goroutine, tracked by a WaitGroup and joined in stop. Also `stopConfigDiskSync`, and `SubscribeModelsChanged(ctx) <-chan struct{}` (buffer 1, coalescing). |
| `internal/app/app_lifecycle.go` | 457 | `releaseResources`: call `app.stopConfigDiskSync()` first, before the `cleanupFuncs` loop (`:351`) (+~3). |
| `internal/server/events.go` | 158 | In `subscribeAndBroadcast` (`:25`): a goroutine that reads `a.SubscribeModelsChanged(ctx)` and runs `h.Broadcast(EventConfig, buildConfigWire(a))` (+~15). |
| `internal/agent/coordinator_role_slot.go` (new) | ~120 | `roleSlotModel(cfg, sess, role) (config.SelectedModel, source string, ok bool)` (the session pin, then the config slot; the cascade moved from `run_role_model.go:22-44`), and `liveSubAgentModel(ctx, built Model) (Model, bool, error)` for dispatch-time worker refresh. |
| `internal/agent/coordinator_models.go` ⚠ | **981** | Must not grow past 1000. First a **pure-move** commit: move `buildModelsFromCfg` (`:804-896`, ~93 lines) to `internal/agent/coordinator_models_build.go`, proven with the `func/type/var/const` sorted-diff oracle from CLAUDE.md. Then +~12: in `resolveSessionModelsInternal` (`:117-136`) and `applyModelOverrides` (`:367-393`), when `callOptionsFrom(ctx).ModelRole` is non-smart and the smart override is nil, take the smart slot from `roleSlotModel`. |
| `internal/agent/coordinator_subagents.go` | 521 | After `:193`, when `hbSource == hbSourceSlotStart && callCreds == nil`, call `liveSubAgentModel`; on a difference, adopt the rebuilt model and set heartbeat source `hbSourceSlotLive` (+~12). Only if D3 = dispatch-time. |
| `internal/cmd/run_role_model.go` | 77 | `foldRoleModel` returns `smartModel` **unchanged** (empty) for fast/worker/reviewer and only validates that the role is configured, either in the session pin or in `Config().Models`. The `useLast` session resolution stays. |
| `internal/cmd/run.go` ⚠ | **997** | No net growth allowed. The call at `:760` keeps its signature. Leave the comment lines untouched or shorten them. |
| `internal/app/app_run_setup.go` | 481 | No change expected: with `SmartModel == ""`, `modelOverrideRequested` stays false (`:161`), so nothing is persisted and no fixed ctx override is armed. `ModelRole` already rides `CallOptions` (`:373`). Verify that `ReasoningEffort` with `RoleSmart == false` (`:172-178`) still applies: the effort must move onto the role-slot resolution. Phase-B detail; see R6. |
| `sdk/sdk.go` ⚠ | **1000** | **Do not touch.** Wiring goes through `app.New` (`sdk/sdk.go:511`). If a fix to the Close path is needed (R9), put it in a new `sdk/` file after an equal-size extraction from `sdk.go`. |

Integration tests (package B). Use a fake OpenAI-compatible provider: an
`httptest.Server` that records the `model` field of every request body,
as in `internal/agent/task1314_oauth_disk_integration_test.go:155-172`.
Run two independently loaded `ConfigStore`/App instances on one isolated
data dir (`isolateAllGlobalConfigPaths`,
`internal/agent/testutil_isolate_test.go:35`). Ticks come from
`ConfigStore.PollConfigDiskOnce(ctx)` (exported), never from sleeps or
`Eventually` loops on the real clock.

1. `TestRoleModelHotReload_NextTurnUsesNewSmart`: instance A runs session
   S1, and the fake records `m1`. Instance B runs
   `UpdatePreferredModels(global, smart=m2)`. Then `A.Store().PollConfigDiskOnce`,
   A runs S1 again, and the fake records `m2`. *Revert-check:* removing
   the `StartConfigDiskSync`/poll trigger makes request 2 record `m1`.
2. `..._ExplicitSessionChoiceSurvives`: S2 has
   `Sessions.UpdateModels(smart={p, mX})`. After the role change and a
   poll, S2 records `mX`, and a fresh S3 records `m2`.
3. `..._InFlightNotInterrupted`: the fake blocks request 1 on a channel
   until released. While it is blocked, B changes smart to `m2` and A
   polls (the generation advances). Release the channel: request 1
   completes **with the `m1` body** and no cancellation; request 2
   records `m2`.
4. `..._RoleWorkerNotPersistedAndLive`: ExecuteRun with
   `ModelRole=worker` and no `SmartModel`. The fake records the worker
   default `w1`; afterwards `sess.SmartModelID == ""`. Change worker to
   `w2` and poll: the next turn of the same session with
   `ModelRole=worker` records `w2`, and a turn with `ModelRole=smart`
   records the smart default. *Catches:* the §2.2 copy.
5. `..._SubAgentDispatchUsesLiveWorker` (only if D3 = dispatch-time):
   reuse the `newWorkerToolTestCoordinator` fixture family. Build the
   parent toolset with worker `w1`, change to `w2`, poll, and dispatch:
   the child records `w2`.
6. `..._WebBroadcastsConfig` (`internal/server`): the hub test client
   receives `EventConfig` with the new `Models` after a poll that
   changed the models, and receives none after a poll that changed only
   `recent_models`.
7. `TestAppShutdownJoinsConfigDiskSync` (`internal/app`): after
   `ShutdownWithResult`, `goleak.VerifyNone` (or the
   `runtime.NumGoroutine` settle pattern of
   `internal/agent/work_ledger_timeout_test.go:54-70`), and
   `StopConfigDiskSync` was observed before `mcpOwner.Close` (ordering
   hook).

Note for CI: `agent-leak-smoke.yml` measures cumulative goroutine growth
across all of `internal/agent`. Every test that constructs an App or
store with the poller must call Stop, through `t.Cleanup(app.Shutdown)`
or an explicit Stop.

---

## 5. Risks and operator decisions

### Risks

- **R1: more frequent full reloads in idle processes.** Peak-hours
  reloads already happen during turns. Now every process reloads on any
  content change of any tracked file, including web recent-model writes
  and other processes' OAuth rotations. Each reload re-resolves
  `$(...)` API-key templates (shell processes). Mitigation: reload only
  on digest change, and remember failures. Residual: one shell call per
  template per change per process.
- **R2: loss of in-memory runtime mutations on reload.** This is
  pre-existing, but the poller makes it reachable in idle processes.
  `run.go:756` mutates `Config().Options.TurnStallTimeoutSeconds` **in
  place** on the published config, which is also a data race. A reload
  reverts it to the disk value. ExecuteRun reads it once into
  `CallOptions` (`app_run_setup.go:417`), so the first run is
  unaffected. Whether any later per-turn reader exists is UNKNOWN. P2:
  move it to `CallOptions` or `RuntimeOverrides`. Also
  `SetProviderRuntimeAPIKeyIfTemplate` (`store_oauth.go:171-191`): a
  reload re-resolves the template anyway, so this is benign (INFERRED).
  `UpdateAgentAllowedTools` from `app_run_gates.go:57-73` has no
  production caller (`disableSubAgentToolsInConfig` is dead code), so it
  is benign.
- **R3: models-integrity bypass** (§3.8). This is real today on the
  peak-hours path and becomes systematic with the poller. It is fixed in
  package A; it must not ship without that fix.
- **R4: OAuth double publication** (§3.5). Correct but redundant;
  backlog item.
- **R5: `sessions cost` attribution.** Cost groups by
  `sessions.smart_model_id` (`internal/db/sql/messages.sql:358`). Once
  role folds stop persisting, `--role worker` sessions show the
  inherited/empty smart id instead of the worker model. Per-message
  attribution (`SumMessageUsageByModelInRange`) is unaffected.
- **R6: per-invocation reasoning effort for role runs.** Today `--effort`
  with `--role worker` is attached to the folded smart override
  (`app_run_setup.go:172-178`). After the change it must attach to the
  role-slot resolution. Phase B must carry it (for example
  `CallOptions.RoleReasoningEffort`) or keep a narrow override for
  effort only. UNKNOWN which is less invasive until written.
- **R7: Windows stat behavior** during atomic replace (transient
  `ERROR_SHARING_VIOLATION`). This is treated as an unsettled tick. Not
  reproduced; UNKNOWN frequency.
- **R8: `UpdateModels` on `OnModelsChanged`** rebuilds the shared
  toolset, including MCP tool lists, in the background while turns run.
  Pinned turns are unaffected (R3-1), but an MCP listing failure would
  log noise. Gated to model changes only. It may be dropped (§3.7).
- **R9: SDK wiring** goes through `app.New` (OBSERVED `sdk/sdk.go:511`),
  so `sdk.go`, which is at the 1000-line limit, needs no change. Whether
  `sdk.Client.Close` always reaches `App` shutdown's `releaseResources`
  on every path, including `CloseEphemeralConnsForced`
  (`sdk/sdk.go:968`), is UNKNOWN. A path that skips it would leak the
  poller goroutine.
- **R10: Review Stop Rule.** The poller is concurrent code over shared
  state. Reviews of it follow the P0/P1-only rule in CLAUDE.md. Most of
  the hard concurrency is reused (reload CAS), and the poller itself is
  one serial goroutine.

### Operator decisions

- **D1: interval.** 5 s ±20 % (worst case about 6 s, mirrors OAuth) or
  4 s ±20 % (strictly under 5 s). Recommendation: 5 s.
- **D2: `rush run --role fast|worker|reviewer` persistence.** (a) Stop
  persisting the folded model and resolve the role live per turn
  (recommended, no migration). (b) Add a `sessions.smart_model_role`
  column (migration) so a web continuation of a CLI role session keeps
  following the role. (c) Keep today's copy (role changes never reach
  such sessions).
- **D3: sub-agent worker model.** Resolve at dispatch time
  (recommended: dispatches inside a long orchestrator turn are new
  requests) or keep it fixed at the parent turn's start (today).
- **D4: integrity mismatch on reload.** Keep the previous role models
  and adopt the rest (recommended), or reject the whole reload candidate.
- **D5: existing sessions that already carry a copied role model.** No
  marker distinguishes them. Leave them as they are (recommended;
  operator can clear per session), or provide a bulk "clear smart
  override" for sessions whose title or origin is CLI. That is a
  heuristic and could misfire.
- **D6: OAuth poller lifecycle.** It has no production stop/join, and
  the SDK does not start it (§1.4). Fold its start/stop into the same
  App-owned lifecycle in this work or leave it for a separate item.
  Recommendation: separate item. It is outside this request, but it
  should be recorded.
- **D7: web notification.** `EventConfig` only (recommended), or also a
  user-visible toast "default models changed by another process".
