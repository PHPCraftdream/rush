# Rush-owned configuration migration to Ktav 0.8.1

**Status:** design proposal; implementation and runtime verification pending.  
**Review baseline:** the repository root, current `main` checkout, 2026-10-22.

**Evidence labels:** **OBSERVED** means inspected source or filesystem state, not executed behavior. **PROPOSED** means the implementation contract recommended here. **UNVERIFIED** identifies facts that require checking before implementation.

No files were changed. No builds or tests were run. Only read-only repository/filesystem inspection was performed.

## 1. Decisions and backend boundary

The following operator decisions are fixed:

- Migrate Rush-owned configuration, including settings, settings protection, model-integrity metadata, provider files, and OAuth recovery journals.
- Automatically convert active legacy JSON files at startup, publish Ktav files, and rename their JSON originals to `.json.bak`.
- Rollback is manual.
- Keep `.mcp.json`, hook stdin/stdout, session/export JSON, API payloads, and other shared protocols as JSON.
- `crush.json` remains an input accepted only by `rush migrate`.

The **only permitted Ktav import** is:

```text
github.com/ktav-lang/golang/pure
```

at version **v0.8.1**. The root `github.com/ktav-lang/golang` package, its Rust C-ABI implementation, and `github.com/ebitengine/purego` are forbidden in shipped dependency graphs.

### Backend availability

**OBSERVED:** the inspected default module cache contains only `github.com/ktav-lang/golang@v0.6.4`; its download cache likewise contains only v0.6.4. No v0.8.1 pure implementation was found there. The repository has no visible vendor directory. A broader home-directory search was incomplete and is not evidence about an overridden `GOMODCACHE`.

**UNVERIFIED:** v0.8.1 function signatures, strict-parser entry point, generic tree types, number representation, writer entry point, error types, deterministic ordering, resource limits, and module/submodule layout. Do not infer these from v0.6.4 or the root wrapper.

The operator-supplied `LoadsInto` support for `json` tags is accepted, but it does **not** establish compatibility with `json.Unmarshaler`, `json.RawMessage`, `time.Time`, custom maps, or `omitempty`.

**First implementation gate:** inspect the actual v0.8.1 pure source and spec fixtures in CI before selecting concrete API calls. The codec interfaces below are Rush-owned conceptual interfaces, not claimed upstream APIs.

## 2. Reader/writer map

### 2.1 Core persistence boundaries

Paths below are relative to the repository.

| Location | Operation | Migration responsibility |
|---|---|---|
| `internal/config/load_paths.go:16`, `:56` | READ/discovery | Global config and global data config filenames; preserve separate environment overrides and locations. |
| `internal/config/config_unix.go:7` | READ/discovery | `/etc/rush/rush.json`; change preferred filename without losing legacy discovery. |
| `internal/config/load_files.go:53`, `:82` | READ/discovery | System, home config, home data, bounded project walk, visible/hidden names. |
| `internal/config/load_files.go:251`, `:298` | READ | Stable owned reads, validation, document collection and merging. Decode serialization here without changing raw-byte fingerprints. |
| `internal/config/load_files.go:371` | READ/semantic decode | JSON merge, then typed `Config` decoding. Preserve existing merge semantics. |
| `internal/config/load.go:59`, `:85`, `:110` | READ | Initial discovery plus separately resolved data-directory/workspace config. |
| `internal/config/store_reload.go:70`, `:421`, `:447` | READ | Stable file reader and reload’s independent workspace-read path. |
| `internal/config/store_write.go:49` | READ | `HasConfigField`, currently a direct JSON-path existence query. |
| `internal/config/store_write.go:324`, `:371`, `:392` | EDIT-IN-PLACE / WRITE | Sorted multi-key `sjson.Set`, guarded RMW, verified atomic commit. |
| `internal/config/store_write.go:533`, `:547` | EDIT-IN-PLACE / WRITE | `sjson.Delete`, then verified atomic commit. |
| `internal/config/store_models.go:37`, `:54` | READ | Direct, unmerged per-scope model selection. |
| `internal/config/store_models.go:81` | WRITE, indirect | Model writes through `SetConfigFields`; recent-model bookkeeping must follow the same boundary. |
| `internal/config/store_options.go:7`, `:19`, `:54`, `:70` | WRITE, indirect | Compact mode, transparency, theme and keep-alive through generic persistence. |
| `internal/config/settings_lock.go:80`, `:98`, `:172`, `:230` | READ | Password precheck, alternate/canonical/natural settings sources, fail-closed authorization. |
| `internal/config/settings_lock.go:187`, `:303`, `:342`, `:349` | READ / EDIT-IN-PLACE / WRITE | `sync_rev` lookup, lock/unlock edits and atomic persistence. |
| `internal/config/settings_lock_sources.go:10`, `:17`, `:27` | READ/discovery | Canonical global and natural workspace protection sources. |
| `internal/config/settings_lock_sources_windows.go:11` | READ/discovery | Windows known-folder canonical source, independent of environment overrides. |
| `internal/config/settings_lock_hashes.go:11` | READ | Collect every protection hash; no source may silently override another. |
| `internal/config/settings_models_integrity.go:47`, `:75`, `:115` | READ / EDIT-IN-PLACE | Canonicalize models, maintain `cache_rev`, perform startup integrity check. |
| `internal/config/store_oauth.go:361`, `:379`, `:402` | READ / EDIT-IN-PLACE / WRITE | Credential CAS, OAuth/API-key mutation and atomic commit. |
| `internal/config/store_oauth.go:496`, `:505` | READ | Direct disk OAuth lookup. |
| `internal/config/store_oauth_disk_sync.go:90` | READ | Background sampling, feeding the OAuth disk-token decoder. |
| `internal/config/store_oauth_journal.go:102`, `:127`, `:182`, `:201`, `:308` | READ / WRITE | Journal naming, record serialization, config token decoding and readback. |
| `internal/config/store_oauth_journal_outbox.go:124` | READ/verification | Decode and sync an external journal, then prove unchanged identity and bytes. |
| `internal/config/store_oauth_journal_quarantine.go:20` | WRITE/preservation | Preserve corrupt journal evidence; do not reinterpret quarantined bytes as Ktav. |
| `internal/config/store_oauth_journal_worker.go:31`, `:74`, `:236` | READ / lock orchestration | Refresh sidecar identity, recovery and config synchronization. |

**Important correction to the file-kind premise:** in this checkout, settings, `settings_lock`, and models-integrity are **not three independent JSON files**. Settings reside in scoped `rush.json`; protection is `sync_rev` (`settings_lock.go:21`), and model integrity is `cache_rev` (`settings_models_integrity.go:18`). Do not invent additional files for them.

### 2.2 Rush MCP definitions share the Rush document

Leaving `.mcp.json` as JSON does **not** leave all MCP persistence untouched:

| Location | Operation | Required distinction |
|---|---|---|
| `internal/config/store_mcp_json.go:20`, `:45`, `:79`, `:83` | READ / EDIT-IN-PLACE | Hand-written JSON scanner/editor handles Rush `mcp` as well as external `mcpServers`. |
| `internal/config/store_mcp_transaction.go:325`, `:357` | WRITE | Commit changed physical records. Encode Rush records as Ktav; external records remain JSON. |
| `internal/config/store_mcp_transaction.go:475`, `:501`, `:571`, `:602` | READ/discovery | Evaluate mixed document kinds, origins and scope ordering. |
| `internal/config/load_files.go:393`, `:431` | READ/transform | Disabled-only overlays operate on JSON-shaped internal documents. |
| `internal/config/mcp_revisions.go:21`, `:36`, `:47` | READ | Distinguish Rush `mcp` and external `mcpServers` when building revisions. |
| `internal/config/mcp_json.go:248`, `:299` | READ | Rush-side disabled overlays and entry lookup still need decoded Rush data. |

Preserve the external JSON surgical editor. Add serialization dispatch for Rush-owned documents; do not feed Ktav text into the existing JSON scanner.

### 2.3 Provider-owned files

| Location | Operation | Responsibility |
|---|---|---|
| `internal/config/provider.go:71` | discovery | Generates `providers.json`, `hyper.json`, and other named cache paths. |
| `internal/config/provider.go:106`, `:142` | READ | Local update-provider/update-hyper input is currently JSON. Accept Ktav local inputs explicitly; keep remote Catwalk/Hyper protocols JSON. |
| `internal/config/provider.go:292`, `:306` | READ / WRITE | Generic cache decoder/encoder; currently plain `os.WriteFile` at `:317`. |
| `internal/config/model_catalog_cache.go:28`, `:50`, `:80`, `:95` | READ / WRITE / delete | Named model catalogs, including timestamp and fingerprint; clear must handle legacy/current generations without resurrecting stale data. |
| `internal/config/catwalk.go:28` | READ, indirect | Cache age policy; conversion must not accidentally make stale provider content fresh. |

The observed cache names are **not** evidence of a separate file per configured provider credential. Credentials are in `providers.<id>` inside Rush config.

### 2.4 Consumers, CLI, SDK and documentation

| Location | Operation |
|---|---|
| `internal/cmd/lock.go:29`, `:35` | WRITE indirectly; printed local filename is hardcoded. |
| `internal/cmd/ps_stale.go:34` | READ directly, deliberately without opening the store or DB. Must use a lightweight format-aware reader, not initialize the application. |
| `internal/cmd/migrate.go:575`, `:614`, `:713`, `:823`, `:846` | READ / rename / WRITE. Legacy content rewriting and destination conflict handling must end in Ktav via the shared migration service. |
| `internal/server/handlers_config.go:253`, `:288`, `:314`, `:375`, `:401` | WRITE through store: debug, context paths, skills paths. |
| `internal/server/handlers_providers.go:147`, `:179`, `:215`, `:240`, `:286`, `:290` | WRITE through store: provider add/update/delete/rename and peak hours. |
| `internal/server/handlers_models.go:437`, `:445` | WRITE through store: scoped removal. Preserve session-only behavior separately. |
| `sdk/sdk.go:442` | READ through `config.Init` in application mode. |
| `sdk/library_mode.go:28`, `:39` | Explicit discovery bypass: library mode must neither read nor migrate disk config. |
| `internal/cmd/models_use.go:20`, `:278`; `models_unset.go:29`, `:150`; `models_bump.go:208`; `providers.go:46`, `:879` | User-visible filename/help contract. |
| `README.md:962`, `:1028`, `:1051`, `:1076` onward | Migration instructions, discovery, paths and configuration examples. |
| `sdk/README.md:21`, `:64`, `:115` | Application discovery versus library-mode exclusion. |
| `docs/hooks/README.md:134`, `:159` | Hook configuration examples change to Ktav; protocol examples remain JSON. |
| `internal/skills/builtin/rush-config/SKILL.md:8`, `:16`, `:79` | Embedded configuration instructions/examples. |
| `internal/skills/builtin/rush-hooks/SKILL.md:8`, `:190` | Embedded hook-configuration instructions, not wire serialization. |

**Installers:** `internal/cmd/claude_init.go:24`, `:32`, `:45` embeds command templates; it is not a Rush JSON writer. Review installed prose for filename references rather than converting third-party CLI settings. Searches of `npm/` and `scripts/` found no Rush config filename references. Do not blanket-convert JSON in installer ecosystems.

The checked-in `rush.json` should be converted only after inspecting it for secrets; this review deliberately did not print its contents. `schema.json` remains a JSON Schema artifact, not a runtime Rush config.

### 2.5 Tidwall inventory and pinned behavior

**OBSERVED: 17 production function-call expressions in Rush-owned config code:**

| File | `sjson` calls | `gjson` calls |
|---|---:|---:|
| `store_write.go:58`, `:371`, `:533` | 2 | 1 |
| `store_oauth.go:379`, `:388`, `:394`, `:505` | 2 | 3 |
| `settings_lock.go:187`, `:303`, `:342` | 2 | 1 |
| `settings_models_integrity.go:48`, `:76`, `:81`, `:83`, `:132`, `:140` | 2 | 4 |
| **Total** | **8** | **9** |

This counts function calls, not constant references such as `gjson.Null` or `gjson.String`.

Four additional production calls belong to unchanged protocols: `internal/hooks/input.go:67`, `:70`; `internal/hooks/hooks.go:182`; `internal/agent/hooked_tool.go:145`. Tidwall therefore cannot simply be removed repository-wide.

Existing behavior anchors:

- `store_crossprocess_test.go:30`: `TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive`.
- `store_crossprocess_test.go:167`: deterministic negative control demonstrating lost updates without locking.
- `store_write_test.go:141`, `:176`, `:234`: automatic reload and atomic multi-key publication.
- `store_write_lock_timeout_test.go:18`, `:66`, `:101`: bounded internal timeout, preservation under external lock and uncontended success.
- `settings_models_integrity_test.go:109–563`: signing, non-model writes, direct edits, missing/invalid signatures, lock behavior, redaction, separate scopes, and canonical forms.
- `settings_lock_test.go`, `settings_lock_regression_test.go`, `settings_lock_exemptions_test.go`, `settings_lock_audit_test.go`: authorization and exemptions; `settings_lock_test.go:398` contains a direct JSON-path assertion helper.
- `store_oauth_refresh_pending_test.go:148–150`: direct set/delete fixtures.
- `store_oauth_refresh_cas_test.go:15`, `store_oauth_refresh_race_test.go:26`, `:31`: stale-credential and two-store refresh behavior.
- `store_mcp_format_test.go` pins a separate formatting-preserving editor; split Rush-side expectations from external JSON expectations.

## 3. File layout and discovery

### 3.1 Names

| Legacy | New active file | Retained original |
|---|---|---|
| `rush.json`, at every Rush location | `rush.ktav` | `rush.json.bak` |
| `.rush.json` | `.rush.ktav` | `.rush.json.bak` |
| Named Rush provider/cache `<name>.json` | `<name>.ktav` | `<name>.json.bak` |
| `providers.json`, `hyper.json` | `providers.ktav`, `hyper.ktav` | Original name plus `.bak` |
| `model-catalog-<id>.json` | `model-catalog-<id>.ktav` | Original name plus `.bak` |
| `rush.json.oauth-pending-<hash>.json` | `rush.ktav.oauth-pending-<hash>.ktav` | Original name plus `.bak` |

Settings, `sync_rev` and `cache_rev` stay inside their existing logical documents.

Use a format-independent **logical configuration identity** for locks. For compatibility with current code, retain the legacy physical anchor:

```text
<legacy-json-path>.lock
<legacy-json-path>.oauth-refresh-<provider-hash>.lock
```

Do not switch to `rush.ktav.lock` just because the active file changed. Current locks are derived directly from physical paths (`store_write.go:167`; `store_oauth_journal_worker.go:31`), so this requires an explicit mapping used by generic, MCP and OAuth writers.

### 3.2 Ordering

**OBSERVED:** merge order is lowest priority first:

1. System config.
2. `GlobalConfig()`.
3. `GlobalConfigData()`.
4. Project directories from boundary toward the current directory.
5. Separately resolved workspace/data-directory config last, unless already loaded.

At each project directory, reversing the complete current list makes `.rush.json` precede `rush.json` in merge order: **visible `rush.json` currently wins over hidden `.rush.json`** (`load_files.go:82–95`). Existing skill prose says otherwise. Preserve code behavior; do not combine a precedence change with serialization migration.

Replace each logical slot with a preferred-Ktav/legacy-JSON pair. Select **one**, never merge both.

### 3.3 Selection and failure rules

- **Ktav and JSON both exist:** Ktav wins. Log one warning per logical source: “Both config formats exist; using Ktav and ignoring legacy JSON.” Include paths, never values. Do not automatically archive arbitrary conflicting JSON.
- **Ktav exists but is invalid/unreadable:** report the error; do not fall back to JSON and silently activate different settings.
- **Only JSON exists:** attempt automatic migration under the shared lock.
- **Only `.json.bak` exists:** do not restore automatically. Report backup-only state with manual recovery instructions. For an authoritative settings source, fail closed rather than treating it as an unlocked, empty config.
- **Read-only filesystem or insufficient permission:** retain and read valid JSON with a migration-deferred warning. No JSON writes by the new version; explicit mutations fail with an actionable migration-required error.
- **Existing backup:** never overwrite or delete it automatically. Without an authenticated incomplete transaction, migration stops and JSON remains active.
- **Foreign-owned file:** preserve existing rejection. Never elevate, chown, copy into a user-owned replacement, or select a lower-trust sibling to bypass rejection.

Keep the bounded walk, canonicalized directory boundary, negative candidate tracking and opened-handle ownership checks (`load_files.go:72–95`, `:186–201`; `store_reload.go:70–125`). Unix home files require the invoking UID and system files UID 0 (`config_owner_unix.go:9`, `:12`). Windows deliberately bypasses UID checks (`config_owner_windows.go:5–9`); that is not an ACL security guarantee.

For root-owned system JSON, an ordinary user may read it but cannot migrate it. Continue JSON read compatibility and request administrator migration; do not make startup require privilege.

## 4. Automatic migration protocol

### 4.1 Required transaction model

**PROPOSED:** use a small, owner-protected Ktav migration record beside each source:

```text
<legacy-json-path>.ktav-migration
```

It records transaction version, source identity/digest, staged-output digest, source/destination/backup basenames and phase. It contains no config values. It is not trusted merely because its name exists: validate owner, regular-file type, parent binding and referenced identities.

The record is necessary to distinguish an interrupted migration from an unrelated user-created Ktav/JSON pair. Without it, a later start cannot safely decide whether to archive the JSON.

### 4.2 Steps

1. Resolve the logical source and physical parent using existing alias/owner protections. Reject unsupported hard-link topology, as current mutable config commits do.
2. Acquire the existing in-process disk-write serialization and OS sidecar lock, using the legacy anchor. Re-read all relevant entries **after acquiring** it.
3. Re-evaluate preferred file, source, backup and any migration record. A second process may already have completed the operation.
4. Read stable source bytes through the owned reader. Parse exactly one complete JSON document into a lossless tree:
   - detect duplicate decoded keys at every depth;
   - retain number lexemes for validation;
   - reject invalid Unicode/replacement-producing input;
   - do not decode through `Config`.
5. Generate Ktav through the pure backend. Strictly parse the generated bytes back. Compare source and result as described below. Any failure leaves JSON untouched.
6. Stage output in the same pinned parent, with exclusive creation and secret-safe permissions. Flush file data. Persist and flush the migration record and parent before publishing.
7. Publish Ktav with **no replacement of an existing destination**. Reuse `commitConfigFile` protections and outcome classification; extend only the migration-specific multi-name operations.
8. Flush and verify the published Ktav. Revalidate JSON identity and digest. No new-version mutation may modify this Ktav until archival completes.
9. Rename the original JSON to its exact `.json.bak` name using an identity-checked, no-replace operation in the pinned parent. Flush the parent.
10. Verify both entries and mark completion/remove the migration record durably. Restart discovery and construct the ordinary store from the resulting files.

Migrate initial discovery sources before constructing a candidate snapshot. After resolving `data_directory`, migrate that workspace source before its separate merge. Complete OAuth journal discovery/migration before starting recovery workers or exchanging refresh tokens.

Do not acquire long migration locks while holding `publishMu`. The existing distinction between 30-second explicit writes and two-second internal cleanup matters (`store_write.go:72`, `:93`). Use one bounded startup budget, not an unbounded timeout per retry.

### 4.3 Platform primitives

**OBSERVED:** Rush already has more than “temp plus `os.Rename`”:

- Unix pins the parent descriptor, stages and fsyncs, rechecks identity/digest, and publishes using no-replace support for absent targets (`config_commit_unix.go:39–121`).
- Windows keeps the staged handle through publication, uses parent-relative handle rename and readback, and classifies uncertain outcomes (`config_commit_windows.go:15–18`, `:86–146`; `config_rename_windows.go:24–64`).
- Public `CommitOutcome` distinguishes committed, maybe committed and reconciled state (`atomicwrite.go:42–55`).

Reuse those properties. **Do not** implement backup archival as `Stat(bak)` followed by unrestricted `os.Rename`: that races and can replace a backup on Unix. Do not implement Windows replacement as remove-then-rename.

A migration spans multiple directory entries; it is **recoverable**, not one atomic filesystem operation.

### 4.4 Crash recovery table

Let `J` be JSON, `K` Ktav, `B` backup and `M` a valid migration record.

| Crash point | Possible disk state | Next startup |
|---|---|---|
| Before staging/record | `J` | Start migration normally. |
| During staging | `J` plus incomplete temp | Ignore temp as config; remove only after proving ownership/identity. Retry. |
| After durable record, before K publication | `J + M + temp` | Validate source and transaction; finish staging/publication or safely regenerate. |
| After K rename, before durability confirmation | `J + K + M`, or K absent after power loss | Reconcile actual entries/digests. Never assume an error means “not committed.” |
| After durable K, before J archival | `J + K + M` | If both match recorded generations, finish no-replace archival before writes. Otherwise stop migration and report conflict. |
| During backup rename | `K + M` with either `J` or `B` | Inspect identities; finish only the missing proven step. |
| After backup rename, before directory sync | `K + B + M`, possibly prior rename state after power loss | Reconcile, sync and complete. |
| After archival, before record cleanup | `K + B + M` | Verify completed state and remove record. |
| Complete | `K + B` | Read K; never reimport B. |
| Backup exists unexpectedly | `J + B`, or conflicting generations | Preserve every file; require manual resolution. |
| K unavailable but only B survives | `B`, with/without damaged record | Fail closed with manual recovery guidance; never silently initialize defaults. |

CI must test process death and injected durability failures separately; they are not equivalent to power-loss simulation.

### 4.5 Concurrency and old versions

Two new processes serialize on the **same persistent legacy sidecar**. The waiter reselects the active document after acquiring the lock. Never unlink the sidecar; current code intentionally retains it (`store_write.go:252–266`).

**INFERRED, reproducible mixed-version hazard:** after migration, an old process reopens `rush.json`, finds it absent and may recreate it through its normal RMW path (`store_write.go:345–349`). New Rush reads Ktav and ignores the recreated JSON. With OAuth, independent refresh state can be materially worse than a lost UI preference.

The `.bak` remains readable **as JSON bytes to a program explicitly opening that path**, but the old version does not discover it. An already-open Unix descriptor may continue reading the renamed inode; that is not ongoing pathname compatibility.

**Required operational contract:** stop old Rush processes before first migration and do not run old/new writers concurrently. Reusing locks prevents overlapping operations, not writes occurring after the rename. An unmodified old binary has no permanent generation fence that this migration can impose.

### 4.6 Validation and confidentiality

Compare:

- exact object membership and decoded key strings;
- exact string Unicode content, including embedded newlines;
- ordered array elements;
- booleans, null, empty containers and explicit missingness;
- exact mathematical numeric values, without passing through float64.

Number spelling and object key order are not tree semantics. `0.50 → 0.5` is permitted only if exact value and application-visible decoding are unchanged. Reject rounded big integers, changed floats, overflow/underflow or unsupported negative-zero semantics. Include exponent forms.

Reject duplicates before ordinary `encoding/json` map decoding hides them. Require writer refusal for `ScalarRoot`, `EmptyKeyName`, `NonFiniteFloat`, `CRByte`, `BothFormsRequired`, and leading/trailing whitespace collisions. A `##`-prefixed key must survive quoted-key writing and strict reparse.

CRLF **between JSON tokens** is harmless formatting. A JSON string containing decoded `\r` is a different case: reject conversion when unrepresentable, rather than normalizing the prompt or secret.

New secret-bearing files and migration records use Unix mode `0600` or stricter existing restrictions. Backup renames preserve the source inode’s permissions. Do not silently broaden access.

On Windows, `0600` is insufficient to prove confidentiality. The new staged file must receive a DACL no broader than the source’s effective intended readership, or a restrictive current-user/system/admin policy. Verify it before publishing API keys. The inspected commit code does not establish such a DACL guarantee.

## 5. Semantic traps and test matrix

### 5.1 Preserve JSON-shaped semantics

Keep a lossless document model separate from runtime `Config`. Never write a marshaled merged/defaulted/resolved `Config` back over a scope:

- unknown fields would disappear;
- defaults could become explicit settings;
- global/workspace provenance would collapse;
- resolved credentials could replace `$VAR` templates;
- `omitempty` could remove explicit false/zero/empty values.

`ProviderOptions` and `ExtraBody` are `map[string]any` (`config.go:117`, `:156`, `:158`). MCP/OAuth overlay readers use `json.RawMessage`. Preserve these as JSON-shaped values internally, not base64 byte strings or Ktav fragments.

Selected-model sampling values use float pointers (`config.go:109–114`); explicit zero differs from absence. Provider discovery and peak hours also use pointers (`config.go:171`, `:185`).

Do not impose a new recursive environment-expansion rule. Provider headers expand, while `extra_body` deliberately does not (`config.go:141–156`). Conversion must never execute shell substitutions.

Not all times are strings: `ToolGrep.Timeout` is a `*time.Duration`, currently decoded by JSON numeric semantics (`config.go:279`); hook timeout is integer seconds (`:299`). Preserve units rather than “improving” them during migration. Model catalog `FetchedAt` is `time.Time` (`model_catalog_cache.go:22`).

### 5.2 Model-integrity incompatibility

**OBSERVED:** `canonicalModelsJSON` uses `UseNumber`, and the test explicitly requires `1.50` and `1.5` to hash differently (`settings_models_integrity_test.go:582–586`).

Ktav normalization therefore cannot preserve every existing `cache_rev` by copying it unchanged.

**PROPOSED:**

1. Verify legacy integrity against original JSON before conversion.
2. If a valid old signature exists, translate it atomically into the signature of the normalized JSON-shaped models value.
3. Preserve an existing mismatch as a mismatch; migration must not bless a direct edit.
4. Preserve missing-signature policy: unlocked legacy acceptance versus locked mismatch.
5. Update canonical-form tests to distinguish numerical value semantics from source lexeme identity.

This is a controlled integrity-format transition, not ordinary settings authorization. It must work without knowing the lock password while preserving `sync_rev` exactly.

### 5.3 Minimum matrix

| Area | Cases and assertions |
|---|---|
| Scalar-looking strings | `"true"`, `"false"`, `"null"`, `"0"`, `"01"`, `"1.5"`, exponent-looking strings, empty string: write literal-string forms where required; remain strings. |
| Keys | Dots, backslashes, numeric-looking keys, `##`, Unicode, delimiters, empty key; literal keys must not become dotted paths. |
| Multiline prompts | LF, blank lines, trailing newline, parentheses and both multiline delimiter forms, leading/trailing spaces; exact content or explicit refusal. |
| Environment templates | `$VAR`, `${VAR:-x}`, `$(cmd)`, Windows paths and regex escapes: byte-equivalent strings; no migration-time execution. |
| Containers | Nested provider `models` arrays, empty arrays/objects, array order and existing cross-file array-append behavior. |
| Presence | Missing, null, false, zero, empty string/container, nil and non-nil pointers. |
| Numbers | `0.50`, exponents, >2⁵³ integers, int64 boundaries, long fractions, negative zero, overflow and underflow. No silent rounding. |
| Dynamic data | Unknown nested fields in provider options, extra body, MCP and future top-level fields survive unrelated writes. |
| Typed decoding | `json.RawMessage`, custom `csync.Map`, times, numeric durations and string times match current JSON behavior through the serialization adapter. |
| Unknown fields | Preserve ignored fields on disk; retain unknown-model-slot diagnostics (`load_files.go:483`). |
| Integrity/protection | Valid/mismatched/missing `cache_rev`, `sync_rev`, canonical and redirected sources, OAuth and recent-model exemptions. |
| Unicode | Escaped versus literal Unicode, surrogate pairs, invalid UTF-8, isolated surrogates, control characters. |
| Discovery | Both formats, bad preferred file, backup-only state, owner mismatch, symlink/junction retarget, hard links and aliases. |
| Failure | Every writer refusal, lock timeout, disk full, read-only mount, Windows sharing denial, uncertain commit and restart. |

## 6. Write-path recommendation

| Option | Cost | Consequences |
|---|---|---|
| **A. Parse → tree → mutate → regenerate** | Moderate serialization work; high path-editor compatibility burden | Loses comments, formatting, source order and numeric spelling; must reproduce existing escaped-path/set/delete behavior. |
| **B. Minimal line-oriented editor** | High and open-ended | Dotted keys, quoted keys, inline/nested containers and multiline literals defeat reliable line matching. Correct preservation requires a parser/concrete-syntax tree, not a small line editor. |
| **C. JSON-shaped internal model; change serialization boundaries** | Lowest semantic churn, but every I/O edge must be covered | Retains JSON merge, path operations, RawMessage consumers and typed decoding. Ktav is the disk representation only. |

**Recommend C.** Its Ktav write boundary necessarily parses and regenerates, but the migration does not introduce a second path-language implementation.

Conceptually:

```text
raw disk bytes + fingerprint
    → format-aware strict decode
    → lossless JSON-shaped document / JSON bytes
    → existing guards, merge, CAS and path mutation
    → pure Ktav writer
    → strict reparse and semantic validation
    → raw Ktav bytes committed using original raw fingerprint
```

Keep raw bytes/fingerprints and semantic bytes distinct. A digest of normalized JSON must never substitute for the digest of the file actually read.

### Formatting contract

Rush-owned Ktav rewrites may lose:

- `##` comment lines;
- blank lines and indentation;
- original object-key order;
- inline versus multiline layout;
- dotted versus nested spelling;
- equivalent literal-string forms;
- numeric spelling, including trailing zeros;
- line-ending and escape choices.

JSON has no valid comments, so migration loses no supported legacy JSON comments. **New Ktav comments are at stake.** Document this before users adopt comments. External `.mcp.json` retains its existing formatting-preserving contract.

### CAS and writer ordering

- Read the current document **under the existing cross-process lock**, not from the snapshot.
- Keep last-lock-holder-wins semantics for the same ordinary key and preservation of unrelated updates.
- Retain credential-value CAS (`store_oauth.go:379`) and raw destination identity/digest checks.
- Preserve committed/maybe-committed outcomes; never retry a logical mutation merely because durability reporting failed.
- Decode/parser failure must not be interpreted as missing `sync_rev`, absent OAuth token or proven logout.
- Preserve bounded best-effort cleanup and post-write reload ordering.

Port the cross-process negative control as well as the successful test. Merely proving that one Ktav write is syntactically valid does not prove RMW safety.

## 7. Rollout and lane ownership

### 7.1 Base and H4 sequencing

Use **`origin/ci/job-batches` after the H4 composition**, pinned to an exact reviewed commit—not checkpoint `main` and not individual H4 lane heads.

**OBSERVED:** the checkpoint identifies H4 P3 as storepath/config and P8 as CLI work (`docs/checkpoints/2026-10-22-1331.md:16–18`), with one composed PR and compile repair after composition (`:32`). Local branch metadata also shows active H4 lanes.

The referenced H4 manifest was absent from this checkout and did not yield content through the inspected remote-tracking paths. Its final file allocation is **UNVERIFIED**. Reconcile the final composed tree before issuing write assignments.

Keep DB activation/import independent from configuration migration:

- do not change `rush_store_v1.db` names or importer behavior;
- do not reuse the database-import lock as the config lock;
- do not start application/DB activation if required config migration is in an uncertain state;
- keep SDK library mode diskless.

All implementation lanes are **static-only locally**: reading, editing and static diff review only; no local build, test or vet. Compilation, formatting checks, tests and verification run in GitHub CI.

### 7.2 Exclusive file allocation

Issue a concrete manifest after rebasing; ownership includes matching fixture/test files.

| Lane | Exclusive ownership |
|---|---|
| **K0 — codec/conformance** | New `internal/config/ktav_codec*.go`, codec tests and spec fixtures; `go.mod`, `go.sum`. No existing store files. |
| **K1 — discovery/migration/platform security** | `load.go`, `load_files.go`, `load_paths.go`, `store_reload.go`, `store_staleness.go`, `config_unix.go`, `config_windows.go`, owner-policy files; new `ktav_migration*.go`; migration-specific platform helpers; existing commit/rename/identity helpers only where necessary. |
| **K2 — ordinary settings and protection** | `store_write.go`, `store_models.go`, `store_options.go`, `settings_lock*.go`, `settings_models_integrity.go`; cross-process, timeout, lock and integrity tests. |
| **K3 — OAuth** | All `store_oauth*.go` and their tests, including journals, quarantine and disk sync. |
| **K4 — Rush/external MCP boundary** | `store_mcp*.go`, `mcp_json.go`, `mcp_revisions.go`, associated format/transaction tests. Do not touch MCP runtime ownership. |
| **K5 — provider caches** | `provider.go`, `catwalk.go`, `hyper.go`, `model_catalog_cache.go`, associated cache tests. |
| **K6 — CLI/SDK/web/docs** | `internal/cmd/lock.go`, `ps_stale.go`, `migrate.go`, relevant CLI help/fixtures; `sdk/`; settings handler files under `internal/server/`; README and current examples; builtin skill docs. |
| **Integrator/CI** | `internal/config/config.go`, `store.go`, shared test isolation helpers, CI workflows/selectors and cross-lane wiring. No lane edits these ad hoc. |

K0 publishes the adapter contract first. K1 publishes logical path/lock APIs next. Other lanes implement against those interfaces. Compose into one integration branch; do not ship a filename switch before every direct reader and writer is converted.

Historical checkpoints/reviews remain historical. Do not mass-rewrite archived JSON examples.

## 8. Acceptance criteria and GitHub CI gates

### Backend and dependency gates

1. Pin v0.8.1 and prove the imported package is `github.com/ktav-lang/golang/pure`.
2. For each shipped GOOS/GOARCH/tag combination, inspect `go list -deps` for the actual binary target:
   - pure backend present;
   - exact root wrapper package absent;
   - `github.com/ebitengine/purego` absent;
   - any `ktav_cabi` package absent.
3. Inspect binary/module build metadata and release contents for forbidden native artifacts. `CGO_ENABLED=0` alone is **not** sufficient: a purego C-ABI wrapper can pass that condition.
4. Independently inspect the pure package’s dependency closure: its runtime dependencies must be standard-library-only.

### Spec conformance gate

Run the pinned spec’s **`versions/0.8.1/tests`** against the pure backend, with all categories executed:

- `valid`;
- `invalid`;
- `unrepresentable`;
- `parseable-unrepresentable`;
- `strict-lossy`.

Require fixture enumeration counts and an assertion that every discovered fixture was executed exactly once in its category. Missing directories, unsupported fixture handling, zero-case categories, skipped cases or filtered failures fail the gate.

Use the upstream expected oracle, not only parse/write/parse agreement: two mutually incorrect operations can round-trip successfully.

### Persistence/security gates

Run native Windows, Linux and macOS CI covering:

- the crash table using real child termination at named transaction boundaries;
- injected rename, sync, readback and permission failures;
- two-process first-start migration and migration-versus-write contention;
- pre-existing backup and destination races with no clobber;
- symlink/junction/parent retarget and hard-link rejection;
- ownership and Windows DACL confidentiality;
- raw-byte fingerprint and ABA protection;
- exact conversion validation and every writer refusal;
- no secret values in diagnostics, including parse errors.

Migration tests must assert both active configuration and **all resulting disk entries**, not just successful restart.

### Existing behavior gates

Port and run:

- `store_crossprocess_test.go`;
- `store_write_lock_timeout_test.go`;
- `store_write_durability_test.go`;
- `settings_lock*_test.go`;
- `settings_models_integrity_test.go`;
- OAuth CAS, two-store rotation, journal restart, outbox and disk-sync suites;
- MCP formatting/identity/transaction suites, with external JSON preservation unchanged;
- provider-cache invalidation and TTL tests;
- CLI `lock`, `migrate`, `ps`, models and providers tests;
- SDK application discovery and library-mode no-I/O tests;
- web scope tests, including session-only selections not persisting settings.

The final composed branch must pass full GitHub CI. No lane-local green result substitutes for composed verification.

### Product acceptance

- Every observed direct reader recognizes the selected format.
- New-version mutations never recreate legacy active JSON.
- Successful migration leaves validated Ktav and the original JSON backup.
- Existing backups are never silently replaced.
- Deferred/failed migration never silently removes settings protection or credentials.
- Provider cache migration preserves freshness semantics.
- Rollback instructions require stopping all versions, preserving current Ktav, restoring chosen `.bak` files and acknowledging loss of post-migration changes.
- OAuth rollback explicitly warns that a backed-up refresh token may already be burned; manual rollback may require reauthentication.

## 9. Remaining risks and operator signoff

1. **Mixed-version operation:** automatic rename is unsafe with an old writer still running. The concrete recreate-JSON scenario above cannot be solved by changing only the new binary. Approve an explicit quiescence requirement and mixed-version warning.
2. **Comment preservation:** approve the documented loss of new Ktav comments on Rush writes. Preserving them would require a separate concrete-syntax editing project, not a minimal line editor.
3. **Integrity transition:** approve replacing lexeme-sensitive model signatures with value-normalized signatures only after validating legacy integrity. Otherwise valid JSON such as a signed `1.50` cannot be migrated without producing a false integrity mismatch.

**Implementation blockers, not additional product decisions:** verify the actual v0.8.1 pure API/spec fixtures, and re-anchor the inventory and lane manifest on the completed H4 tree. No backend capability, platform durability result or migration test pass is claimed by this read-only review.