# Ktav 0.8.1 configuration migration — operator and user guide

This document describes Rush's migration of its own configuration files from
JSON to the Ktav 0.8.1 format. **Important:** the automatic migration gate
(`ktavAutoMigrate`) is currently `false` in the code
(`internal/config/ktav_migration.go`). A separate PR flips it. Everything
below describes behavior **after** that gate is enabled, unless explicitly
stated otherwise. Every claim here is verified against the code in
`internal/config/`.

## 1. What changes and what does not

### Rush-owned files that move to Ktav

| Legacy JSON name | Ktav name | Legacy file after migration |
|---|---|---|
| `rush.json` | `rush.ktav` | kept in place |
| `.rush.json` | `.rush.ktav` | kept in place |
| `providers.json` | `providers.ktav` | kept in place |
| `hyper.json` | `hyper.ktav` | kept in place |
| `model-catalog-<id>.json` | `model-catalog-<id>.ktav` | kept in place |
| OAuth pending journal: `rush.json.oauth-pending-<hash>.json` | `rush.ktav.oauth-pending-<hash>.ktav` | renamed to `.bak` |

`<hash>` is the first 16 hex characters of the SHA-256 of the provider ID
(`oauthJournalDigest`, `ktav_oauth.go`).

### What does NOT change

- `.mcp.json` — stays JSON.
- Hook stdin/stdout payloads — JSON.
- Session and export files — JSON.
- API payloads and the remote Catwalk/Hyper protocol bodies — JSON in both
  directions (`ktav_cache.go` header comment).
- `crush.json` — the legacy Crush configuration. It is only an input for the
  `rush migrate` command (`internal/cmd/migrate.go`), which renames it to
  `rush.json`; it is never read by the Ktav path and never becomes a `.ktav`
  file directly.

## 2. Discovery and precedence

Implemented in `ktav_discovery.go` and `ktav_doc.go`:

- **Ktav wins per logical slot.** Each legacy JSON path (`rush.json`,
  `.rush.json`, etc.) names a logical "slot". `selectConfigSlot` resolves the
  slot to `rush.ktav` whenever that file exists, otherwise to the JSON name.
- **Both-formats warning.** When both spellings exist, Rush logs once per
  file: `Both config formats exist; using Ktav and ignoring legacy JSON`
  (`warnBothFormatsOnce`). Paths only are logged, never document values.
- **Invalid Ktav is an error, no JSON fallback.** A selected `.ktav` file
  that fails to decode produces `invalid Ktav in config file <path>: …`
  (`decodeConfigDocument`). The legacy JSON sibling is **not** used as a
  fallback — including for I/O errors while statting the Ktav file.
- **Backup-only state fails closed.** If a well-known config location holds
  only a `*.bak` backup (no JSON, no Ktav), reads fail with
  `errKtavBackupOnly` ("config backup exists without an active config file;
  restore it manually"). Backups are never restored automatically. This check
  runs before loading (`checkAuthoritativeBackupOnly`, called from
  `load.go:63` and `store_reload.go:435`) and covers the system, global, and
  all discovered project slots.
- **Merge order unchanged.** Visible `rush.json` still beats hidden
  `.rush.json` at the same directory level. Note: some historical README text
  described the opposite; the code is authoritative here.
- **Locks anchor on the legacy name.** The inter-process write sidecar for a
  `rush.ktav` document is `rush.json.lock` (`configWriteLockAnchor`), the same
  sidecar the pre-Ktav write path used. The migration transaction also locks
  `rush.json.lock` (`ktav_migration.go:112`).
- **Foreign-owned Ktav files are still selected, then rejected.** If a
  `rush.ktav` exists but is owned by a different user, the slot still selects
  it and the owner policy rejects the read — the slot never silently
  downgrades to the JSON sibling (`selectConfigSlot` doc comment; the owner
  check happens in the stable reader). Files are never chowned or copied.

## 3. Automatic migration

Implemented in `ktav_migration.go`; call sites: startup in `load.go`
(`ktavMigrateStartupSlots` at `load.go:68`, workspace via
`ktavMigrateWorkspaceIfNeeded` at `load.go:101`) and reload
(`store_reload.go:360` and `:370`).

- **Runs at startup and on reload**, gated on `ktavAutoMigrate`.
- **Pending-record recovery runs regardless of the gate.** If a migration
  record file exists (an interrupted transaction), recovery of that slot runs
  even with the gate off (`ktavMigrateStartupSlots` calls
  `ktavMigrationPendingSlots` before checking the gate).
- **Files produced:** only `<name>.ktav`. The JSON original stays in place,
  untouched, so older binaries that only read JSON keep working; newer ones
  use the Ktav file. The two are not kept in sync afterwards (`ktavKeepsLegacy`).
- **Pending OAuth journals are the exception:** their JSON copy is still
  renamed to `<name>.json.bak`, because a stale duplicate journal could replay
  over rotated tokens.
- **Leftover backups are ignored.** A `<name>.json.bak` beside an active JSON
  no longer blocks the migration and is never rewritten.
- **Interrupted transactions finish from the migration record**
  `<name>.json.ktav-migration` (mode 0600, contains only paths and SHA-256
  digests — no configuration values). A crash after the Ktav file is
  published leaves the record in place; the next startup re-authenticates it
  (digests, file identity, parent identity) and retires it.
- **Crash-recovery states from the state machine** (`runKtavMigrationStateLocked`):
  - Nothing exists → no-op.
  - Only a backup exists (no JSON, no Ktav) → `errKtavBackupOnly` (fail closed).
  - Ktav + backup exist, no JSON, record present and valid → transaction is
    already complete; the record is removed.
  - Ktav exists, no JSON, no backup, record present → `errKtavMigrationConflict`
    (source and backup are gone but the record remains).
  - JSON + backup exist → the backup is a leftover; the migration proceeds
    (journals: `errKtavMigrationConflict`).
  - Ktav + JSON exist without a record → the normal post-migration state;
    no-op (journals: `errKtavMigrationConflict`).
- **Conflicts preserve everything.** `errKtavMigrationConflict` never deletes
  or rewrites files; resolve manually and restart.
- **Read-only / permission failures defer.** Non-conflict errors (e.g.
  read-only filesystem) are reported once via `warnMigrationDeferredOnce`
  ("Ktav config migration deferred; continuing with legacy JSON") and the
  valid JSON keeps serving. Conflict-, backup-only-, and record-class errors
  abort the load instead of being deferred.
- **15-second budget.** All migration work in one load shares a single
  context deadline (`ktavMigrationBudget = 15 * time.Second`).
- **Hard links rejected.** A legacy source with more than one link aborts
  with `ErrConfigHardLink`.
- **Owner policy.** System, global-config, and global-data slots enforce the
  file-owner policy; project slots use the project directory's policy
  (`ownerPolicyForMigration`). A foreign-owned source or record is rejected;
  files are preserved, never chowned or copied.

### Symlinked rush.json

Implemented in `ktav_migration_symlink.go` (KTAV-6). Dotfiles managers keep
`rush.json` as a symbolic link into a versioned repository; such a link is
**migrated**, not deferred:

- `rush.ktav` is published as a **regular file beside the LINK path** (never
  next to the physical target).
- The symbolic link and its physical target are both left untouched — no
  `.bak` is created for a symlinked source.
- The migration record carries a `symlink` flag; crash recovery
  (`finishKtavSymlinkMigration`) re-verifies the record's digests and file
  identities (through the link, while it still exists) and retires the
  record idempotently.
- A **dangling link** or a non-regular/unreadable target stays deferred with
  zero side effects (`warnMigrationDeferredOnce`; no record, no `.ktav`, no
  link removal).

### OAuth journals during migration

The pending-journal file name follows the config slot's disk format for
**newly created** journals (`selectOAuthJournalPath`). Existing legacy JSON
journals are **not** converted automatically (KTAV-4, deferred). Migration of
`rush.json` itself does not touch the journal files; discovery keeps working
because the legacy journal name anchors on the legacy `rush.json` spelling.

## 4. Operational contract

- **Older Rush versions keep working after migration, on their own copy.**
  They read and write the kept `rush.json`; the new version reads and writes
  `rush.ktav` (it wins slot selection). Changes made by one version are not
  seen by the other — the files diverge from the first write.
- **OAuth refresh-token consequence, stated plainly.** Two Rush versions
  operating at once maintain **independent OAuth refresh state**. Refresh
  tokens are single-use or limited-use: if one version refreshes the token,
  the other's copy can become invalid ("burned"). Concurrent old/new
  instances can therefore invalidate each other's credentials and force
  re-authentication.
- Treat any `errKtavMigrationConflict` as a stop sign: preserve all files,
  resolve manually (see §6), then restart.

## 5. Formatting contract

- **Rush rewrites a `rush.ktav` document through
  `ConvertJSONToKtav`, which re-parses and refuses anything it cannot
  represent losslessly.** As a consequence, Rush writes lose:
  - `##` comments,
  - blank lines,
  - key order (the in-memory model's order is used),
  - numeric spelling (e.g. `0.50` becomes `0.5`),
  - equivalent string forms.
  Hand-edited presentation is not preserved.
- **`cache_rev` (models integrity):** during migration the signature is
  **recomputed over the value-normalized models** only when the old signature
  was **valid** for the legacy document (`applyKtavMigrationModelsIntegrity`,
  `ktav_migration_cache_rev.go`). Rationale: Ktav normalizes number lexemes
  on disk, so a valid legacy signature would no longer verify. An existing
  mismatch or missing signature is copied through **unchanged** — a
  pre-existing direct edit is never blessed by migration, and a mismatch
  stays a mismatch. `sync_rev` is preserved as-is and is never touched. Its stored value is a salted PBKDF2-SHA-256 hash of the settings password; values written by older Rush versions are not recognized and the protection must be re-applied.

## 6. Manual rollback

1. Stop **all** Rush versions.
2. Keep the current `rush.ktav` somewhere safe (copy, do not move yet).
3. The legacy JSON is still in place (older migrations left a
   `<name>.json.bak` instead: rename it back, e.g. `rush.json.bak` →
   `rush.json`).
4. Remove the migrated `.ktav` files (`rush.ktav` and any `providers.ktav`,
   `hyper.ktav`, `model-catalog-<id>.ktav` you want to revert).
5. Acknowledge: any configuration changes made **after** the migration live
   only in the `.ktav` file and are lost by this rollback. Merge them into
   the JSON by hand if needed.
6. **OAuth warning:** the refresh token in the kept JSON may already have
   been burned (used or rotated) by the newer version.
   Re-authentication may be required.

## 7. Windows notes

- The backup archival runs with a **pinned parent-directory handle** and a
  **no-replace rename** through that handle, after re-proving the source
  identity and bytes (`ktav_migration_windows.go`). A path substitution
  between validation and rename cannot redirect the operation.
- **DACL confidentiality is NOT separately established.** The new 0600 files
  (`rush.ktav`, the migration record) get their mode recorded, but on Windows
  the code makes no claim about the DACL of the new file; it inherits the
  parent directory's ACL. Do not assume extra confidentiality beyond what the
  directory grants.

## 8. Library mode (SDK)

SDK library mode (`sdk/library_mode.go`, `ModeLibrary` / `openLibrary`)
performs **zero config-file discovery and zero disk I/O** for configuration:
it never reads, never creates, and never migrates `rush.json` / `rush.ktav`.
All configuration is supplied explicitly through `LibraryConfig`; persistence
is the in-memory (or explicitly provided) database.

## 9. Known limits

- **The automatic migration gate is currently disabled.** `ktavAutoMigrate`
  is `false` (`internal/config/ktav_migration.go:21`); a separate PR enables
  it. This document describes post-activation behavior. With the gate off,
  only pending-record recovery runs; new files are still written in JSON.
- **Existing legacy JSON OAuth journals are NOT auto-converted** (KTAV-4,
  deferred; see the comment in `ktav_oauth.go`). New journals get the Ktav
  name only when the gate is on.
- **A `rush.ktav` appearing while a live process runs** is picked up only on
  the next load/reload for some writers; there is no file-watching mechanism
  for the format switch.
- **No system-config slot on Windows.** `systemConfigPath` is `""` on Windows
  (`config_windows.go`); the `/etc/rush/rush.json` slot exists on Unix only.
- **Comments are lost on Rush writes.** Ktav itself supports `##` comments,
  but the settings document layer re-encodes via `ConvertJSONToKtav` and the
  in-memory model carries no comments, so hand-written comments do not
  survive a Rush rewrite.
- **`schema.json` cannot validate `rush.ktav`.** The repository-root
  `schema.json` is a JSON Schema artifact describing `rush.json`; Ktav is a
  different syntax and is not supported by it.
