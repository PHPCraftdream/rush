# Sprint 0 config I/O amendment — verified projection and execution plan

Date: 2026-11-16. This document supersedes the earlier draft. It is self-contained as the design/measurement record for the current projected split; it does not claim that source has been moved or compiled. Only this document is changed in this pass. No test/build was run.

## Non-ROOT declarations, by proposed owner



`internal/config` remains the policy/orchestration facade. The proposed I/O subtree has seven immediate children; `write` has two children:

```
internal/config/io/
  cfghooks/                 shared provider contracts
  fileid/                   identity and stable-open identity primitives
  casefold/                 case-sensitivity primitives
  docread/                  stable reads, fingerprints, discovery/decode
  fswap/                    rename/link/sync primitives
  slots/                    slot state and accessors
  write/
    cfgcommit/              atomic commit
    slotmigrate/            migration implementation
```

### Proposed package dependency graph

An arrow lists the package's direct dependencies. `cfgmodel` and `datadir` are pre-existing lower packages; the proposed-owner topological order below lists the eight owners in this amendment.

```text
cfghooks    -> {}
fileid      -> {}
casefold    -> {fileid}
docread     -> {casefold, cfghooks, fileid, cfgmodel, datadir}
fswap       -> {casefold, cfghooks, fileid, docread}
cfgcommit   -> {cfghooks, fileid, docread, fswap}
slotmigrate -> {cfgcommit, cfghooks, cfgmodel, datadir, docread, fileid, fswap}
slots       -> {cfgcommit, cfghooks, cfgmodel, datadir, docread, slotmigrate}
```

Toposorted proposed-owner leaf order: `cfghooks`, `fileid`, `casefold`, `docread`, `fswap`, `cfgcommit`, `slotmigrate`, `slots`. Detailed graph proof and edge-symbol tables are reserved for the appendix placeholder; this is a dependency projection, not a compile result.

## Generated artifacts and appendix status

The supporting files are present under `.rush/tmp/io-design/`: `project.py`, `ledger.json`, `projection.json`, and `astledger.go`. The dedicated appendix contains the detailed generated evidence; this repair does not reproduce the full proof or tables.

## Authoritative placement corrections from the earlier hypothesis

The projection supersedes the earlier draft where they conflict:

- `physicalWindowsConfigPath` belongs to `fileid`, not `cfgcommit`.
- `configFileNlinkOfInfo` belongs to `fileid`; `docread` uses it for stable-entry handling.
- `errConfigCommitVerification` and `ErrConfigHardLink` have identity in `fileid`.
- Shared hook wrappers belong to `cfghooks`; migration hook providers are not simply merged into the root provider.
- `canonicalModelsAndRolesJSON` moves to `cfgmodel` with a closure of existing pure aliases; do not redefine mutable model defaults or globals.
- `Fingerprint` remains in `docread`. It does not become a `fileid` concept.
- `fswap` and `slots` depend on `docread`; `docread` imports neither `fswap` nor `slots`.
- The small root journal closure remains root; do not invent a journal port merely to force a lower-level move.

## Complete baseline mapping: 46 source files to destination base file

Each row is one distinct current source path. Rows with split declarations name the destination(s) receiving declarations; this is deliberately a source-path-to-destination map, not a claim that every source file maps exclusively to one output. Merge destinations preserve build constraints and package comments. The source-path rows below correct the earlier duplicate/incomplete baseline table.

| Current source path | Destination base file(s) | Handling |
|---|---|---|
| `config_file_identity.go` | `fileid/identity.go` | move |
| `config_file_identity_unix.go` | `fileid/identity_unix.go` | move/merge |
| `config_file_identity_windows.go` | `fileid/identity_windows.go`; `docread/stabledocs_windows.go` | split `readStableConfigHandle` to docread; remaining identity declarations to fileid |
| `config_owner_unix.go` | `fileid/identity_unix.go` | merge |
| `config_owner_windows.go` | `fileid/identity_windows.go` | merge |
| `config_parent_identity_unix.go` | `fileid/identity_unix.go` | merge |
| `config_parent_identity_windows.go` | `fileid/identity_windows.go` | merge |
| `config_case_sensitivity.go` | `casefold/` matching neutral declarations | move |
| `config_case_sensitivity_behavioral_unix.go` | `casefold/` matching platform declarations | move |
| `config_case_sensitivity_darwin.go` | `casefold/` matching platform declarations | move |
| `config_case_sensitivity_freebsd.go` | `casefold/` matching platform declarations | move |
| `config_case_sensitivity_linux.go` | `casefold/` matching platform declarations | move |
| `config_case_sensitivity_other_unix.go` | `casefold/` matching platform declarations | move |
| `config_case_sensitivity_windows.go` | `casefold/` matching platform declarations | move |
| `config_rename_darwin.go` | `fswap/rename_darwin.go` | move |
| `config_rename_unix.go` | `fswap/rename_unix.go` | merge durability declarations |
| `config_rename_windows.go` | `fswap/rename_windows.go` | merge durability declarations |
| `config_renameat2_linux.go` | `fswap/renameat2_linux.go` | move |
| `config_renameat2_other_unix.go` | `fswap/renameat2_other_unix.go` | move |
| `config_linkat_unix.go` | `fswap/` and `docread/stabledocs_unix.go` | split: `readConfigEntryAt`/`configFileNlinkOfInfo` according to corrected owner assignment |
| `config_durability_unix.go` | `fswap/rename_unix.go` | merge |
| `config_durability_windows.go` | `fswap/rename_windows.go` | merge |
| `config_open_unix.go` | `fileid/identity_unix.go` | move stable-open identity operations |
| `config_open_windows.go` | `fileid/identity_windows.go` | move stable-open identity operations |
| `candidates.go` | `docread/candidates.go` | move/split root policy as listed below |
| `fingerprints.go` | `docread/fingerprints.go` | move/split root reload helpers |
| `mcpdocs.go` | `docread/mcpdocs.go` | move raw primitives; retain root policy composites |
| `normalize.go` | `docread/stabledocs.go` | split pure document operations |
| `stabledocs.go` | `docread/stabledocs.go` | move |
| `ktav_discovery.go` | `docread/slotdocs.go`; `slots/gate.go` | split migration gate state/accessors |
| `ktav_doc.go` | `docread/slotdocs.go`; `slots/gate.go` | split `newConfigFileIsKtav`, `writeTargetSlot` to slots |
| `oauthjournal.go` | `docread/slotdocs.go` or `docread/mcpdocs.go` | split primitives; root journal orchestration stays root |
| `ktav_cache.go` | `slots/ktav_cache.go` | move |
| `ktav_light.go` | `slots/ktav_light.go` | move |
| `ktav_oauth.go` | `slots/ktav_oauth.go` | move low-level path primitives only; orchestration root |
| `ktav_migration.go` | `slots/gate.go`, `slots/startup.go`, `write/slotmigrate/` | split gate, startup, implementation |
| `ktav_migration_artifacts.go` | `slots/startup.go`, `write/slotmigrate/` | split startup path list from migration implementation |
| `ktav_migration_cache_rev.go` | `write/slotmigrate/` | move |
| `ktav_migration_symlink.go` | `write/slotmigrate/` | move |
| `ktav_migration_unix.go` | `write/slotmigrate/` | move |
| `ktav_migration_windows.go` | `write/slotmigrate/` | move |
| `atomicwrite.go` | `write/cfgcommit/atomicwrite.go`; `fswap/` | split sync and same-bytes fingerprint primitives |
| `config_commit_test_seam.go` | `write/cfgcommit/atomicwrite.go` | merge |
| `config_commit_unix.go` | `write/cfgcommit/commit_unix.go` | move |
| `config_commit_windows.go` | `write/cfgcommit/commit_windows.go` | move |
| `config_write_identity.go` | `fswap/sync.go`; root `config` | split pure helpers from root-target functions |

The earlier baseline incorrectly repeated `config_linkat_unix.go` and conflated source-file accounting with output-file accounting. Destination spans must be generated from the declaration ledger, with comments included and the 60-line maximum import allowance applied; files are merged only while preserving build tags, package documentation, comments, and declaration ownership.

## Declaration ownership and boundaries

### `cfghooks` and hook state

Keep root `configTestHooks` and all its original fields unchanged. `cfghooks` provides `valueHooks`, `Snapshot`, and `SetProvider`; root initialization copies the provider values while holding the same root mutex. The migration provider carries the exact migration hook variables `ktavMigrationStepHook`, `ktavArtifactMigrationStepHook`, `ktavArtifactStatFile`, `ktavArtifactChtimes`, and `ktavMigrationBudget`, sampled at their original read moment. Do not add a migration lock. Preserve provider source timing and field selection; use one coherent snapshot where the original read did so. These contracts do not justify consolidating unrelated root variables or mutexes; avoid opaque callbacks.

### `fileid`, `casefold`, `fswap`

`fileid` owns identity value/operation declarations, Unix/Windows owner and parent identity implementations, stable-open identity operations, `physicalWindowsConfigPath`, `configFileNlinkOfInfo`, and the identity sentinels `errConfigCommitVerification` / `ErrConfigHardLink`. `readStableConfigHandle` stays in docread. `casefold` owns the seven case-sensitivity files. `fswap` owns rename/open-swap/linkat/sync primitives and the pure helpers `configWriteIdentityKey`, `configDirectoryIsCaseInsensitive`, `configEquivalentLeafNames`; root-target-taking `configWriteTargetDedupKey*` functions stay root. `readConfigEntryAt` belongs to docread. Fold Unix/Windows durability declarations into their rename files and use neutral `sync.go` for shared declarations as fits the measured source spans.

### `docread`, `slots`, and `slotmigrate`

`docread` owns stable reads, candidate helpers, fingerprints, raw documents, and discovery/decode closure. Move `lookupConfigCandidateGroups`, `configCandidateGroups`, `configOwnersMatch`, `configDocumentBytes`, `addDocumentAliasFingerprints`, `stableReadMaxAttempts`, `sameBytesFingerprint`, `readStableConfigHandle`, and `readConfigEntryAt`. Move the actual root reload helpers from `store_reload.go` into `docread`: `captureReloadFingerprints` (line 549), `addReloadFingerprint` (line 559), `readReloadFingerprint` (line 575), `reloadFingerprintsChanged`, `reloadFingerprintsChangedWithOwner`, `sameReloadPathSet`, `trackedSlotFingerprint`, `reloadFingerprintForPath`, and `rebindReloadFingerprintPath`. Keep root policy functions `loadConfigCandidateStableForWorkingDir`, `candidateInputsChanged`, `initialMCPRevisions`, `mcpInputFingerprints`, `mergeExternalMCPServers`, `readMCPDisabledOverride`, `mcpRevisionDiff`, and `mcpNames`. Other MCP composites stay root unless proven to depend only on `cfgmodel` and raw-document primitives. `errReloadDiskChanged` remains root.

`slots/gate.go` owns `ktavAutoMigrate` state/accessors plus `newConfigFileIsKtav` and `writeTargetSlot`. `slots/startup.go` owns `ktavMigrateStartupSlots`, `ktavMigrateWorkspaceIfNeeded`, `ktavMigrateCacheArtifactsStartup`, and `ktavCacheArtifactPaths`. Remaining light/cache/OAuth path functions remain in corresponding slots files. `write/slotmigrate` owns migration implementation; high-level OAuth journal orchestration and the small root journal closure remain root. Do not invent a lower journal sentinel. Keep a single sentinel identity.

`canonicalModelsAndRolesJSON` moves to `cfgmodel` as a pure closure over existing aliases; compatibility facades may re-export/reference existing lower names but must not define new mutable defaults. Preserve exact existing semantics and avoid duplication of model global state.

### Root-owned moved-back declarations

Root remains policy owner for the root composites listed above, `errReloadDiskChanged`, and the small journal orchestration closure. When packaging the moved declarations, each root-returned declaration is retained in root rather than being assigned by filename. The projection includes all production declarations moved back to root and the original declarations moved down; ROOT is excluded from the 14-owner package count when computing lower package owners. The complete expanded root declaration enumeration and all edge-symbol rows are reserved for the appendix and will be appended from the available generated artifacts. This pass does not reproduce that inventory.

## Measured root usage and export discipline

The parent’s projected production root-to-lower call-location counts, derived from production edges only (tests excluded), are:

| Destination | Root production call locations |
|---|---:|
| `casefold` | 1 |
| `cfgcommit` | 26 |
| `cfghooks` | 30 |
| `docread` | 278 |
| `fileid` | 35 |
| `fswap` | 7 |
| `slotmigrate` | 2 |
| `slots` | 30 |
| **Total** | **409** |

These are deduplicated call-location counts in the ledger, not exact AST call multiplicity: multiple calls at the same source location are counted once. The 409 is the sum of those rows. It does not include tests. The per-file root test-reference counts, excluding exports and filtering `configTestHooks` and root-kept components, are reserved for the appendix. List each export only when an actual production edge requires it and cite the edge evidence as `file:line`; no test-only exports. `Error`/`Unwrap` remain methods. Model/datadir facade aliases use the existing lower names.

Appendix evidence will include the uppercase proposed-renaming list derived from production external-reference evidence in the AST ledger. Keep proposed APIs minimal: expose only what an actual production edge requires.

## Destination packing and size bounds

Measure each destination by declaration spans from the ledger, including comments, package/build-tag comments, and imports. Reserve as much as **60 lines for imports** in each destination estimate; this is a bounded planning measurement, not a formatter-derived exact line count. Use tag-preserving merging and retain platform constraints. The measured/estimated packing targets and rationale supplied by the parent are:

| Owner | Destination packing / bound rationale |
|---|---|
| `fileid` (3) | `identity.go`; `identity_unix.go` (77 + 12 + 16 + 40 + new Nlink helper = under 200); `identity_windows.go` (452 − stable-read 38 + 9 + 16 + 34 + physical path 13 = under 550). |
| `casefold` (7) | Seven platform/neutral source files retained. |
| `docread` (7) | `slotdocs.go` 329 + 199 − write/new + OAuth record (~75), under 650; `fingerprints.go` 277 minus root composites + normalize 29 + root reload helpers (~200), under 600; candidates 122 + lookup (~70), under 250; MCP docs 375 minus root composites (~110), under 375; stable docs 209 − candidate 25 + document 34 + same-bytes 3, under 250; Windows 38; Unix `readConfigEntryAt` 43. |
| `fswap` (7) | Five rename files plus `linkat` 256 − `readConfigEntryAt` 43 − Nlink 3, sync wrappers, and three identity helpers (~60). Fold the two durability files into Unix/Windows rename outputs. |
| `cfgcommit` (3) | `atomicwrite` 392 minus hooks (~185), sync 29, errors 13, same-bytes 3, plus seam 14 (~200); plus Unix commit 263 and Windows commit 313 minus physical-path 13. |
| `slotmigrate` (6) | Migration source 558 minus startup ~70/hook-flag portions (under 500); artifacts 552 minus high-OAuth ~100/cache ~50/warning 9 (under 450); remaining files 80, 240, 94, 93. |
| `slots` (5) | gate flag + new/write + accessors (~50); startup high functions (~110); light (~55) minus accessors 14; cache 66; OAuth 27. |
| `cfghooks` | One provider contract file, sized to a maximum of ~250 lines. |

These parent-provided span measurements are the packing basis, not exact final formatted LOC. Every proposed output is intended to remain below the 900-line policy gate including the 60-line import allowance; actual implementation must remeasure exact spans and fail closed if any output exceeds the cap. The seven-file `docread` layout is an explicit constraint; if exact measured spans invalidate a bound, stop and revise the partition without changing the graph silently.

## Hook and model migration contracts

- Root hook provider remains unchanged; no root hook tests.
- Introduce snapshot contracts only where the projection requires new lower dependencies. Keep configuration-hook fields and migration-hook fields in their respective original sources/providers.
- Preserve the precise read-time snapshot semantics and any existing lock behavior; do not add synchronization that changes migration timing.
- `cfgmodel` exports/reuses the existing pure alias closure for canonical model/role JSON. No mutable defaults are redefined.
- Keep the root journal closure; do not create a dependency port for a tiny unresolved closure.

## Test/reference accounting and verification status

The measured production root call-site total is 409 as detailed above. The per-file root test-reference table, complete projected declaration-owner lists, full edge names/examples, full export evidence table, and uppercase rename candidates are included in the dedicated appendix from the available generated artifacts. No additional graph proof or evidence tables are produced in this repair. `project.py` on disk contains the projection logic referenced by those artifacts.

No source edits are permitted in this document integration pass. No tests, builds, or vet were run. Implementation gates 18+19 are compile-gate policy: compile-only gate `go test -c -o NUL -gcflags=-e <package>`; do not substitute running tests. Vet must cover Linux, Darwin, and Windows; use the prescribed goimports and gofumpt; require clean diff against HEAD. No commit is authorized in this pass.

## Source fingerprint

A fresh recheck of the source fingerprint over sorted `internal/config` relative paths and file bytes covered 353 files and produced SHA-256 `52d808c35fad3f9f28dfee643e97d0563d996889fc27f0e0a9b294d7e5b964bd`. No source files were edited.

## Appendix A — concrete generated projection evidence

This generated appendix is the concrete evidence addendum for the projection above. It takes precedence over earlier statements that tables were reserved or would be appended later. It reports a proposed ownership/design, not executed source moves or a compile result. Generation read the existing projection, ledger, and script artifacts; it appends only this document.

### Artifact inventory and scope

| Input | Observed value |
|---|---|
| Projection | `.rush/tmp/io-design/projection.json` |
| AST ledger | `.rush/tmp/io-design/ledger.json` |
| Ledger totals | 278 files (46 under `io/`), 2409 declarations, 15407 identifier references |
| Unresolved ledger targets | 0 |
| Source fingerprint recorded in the existing body | 353 files; `internal/config` (parent-measured before/after value; not recomputed by this generator) |

The flat-name AST ledger omits selector-field dependencies (`x.Field`) by construction; the artifact carries a separate struct-field table. Ledger call sites are unique by file/line/name/enclosing declaration. If multiple same-identifier calls share one source line, they appear as one ledger location.

### A. Complete proposed IO package declarations and ROOT returns

Each identifier is shown under its original spelling with source file, line, and declaration kind. Synthetic provider endpoints are deliberately omitted from these source-declaration inventories.

#### `casefold` — 44 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `configCaseSensitivity` | type | `io/casefold/config_case_sensitivity.go:10` |
| `configCaseSensitivityUnknown` | const | `io/casefold/config_case_sensitivity.go:13` |
| `configCaseSensitivitySensitive` | const | `io/casefold/config_case_sensitivity.go:14` |
| `configCaseSensitivityInsensitive` | const | `io/casefold/config_case_sensitivity.go:15` |
| `configFoldCaseLeaf` | func | `io/casefold/config_case_sensitivity.go:18` |
| `configCaseSensitivityFromMetadata` | func | `io/casefold/config_case_sensitivity.go:25` |
| `configLinuxExt4SuperMagic` | const | `io/casefold/config_case_sensitivity.go:37` |
| `configLinuxF2FSSuperMagic` | const | `io/casefold/config_case_sensitivity.go:38` |
| `configLinuxMSDOSSuperMagic` | const | `io/casefold/config_case_sensitivity.go:39` |
| `configLinuxExFATSuperMagic` | const | `io/casefold/config_case_sensitivity.go:40` |
| `configLinuxFSCasefoldFlag` | const | `io/casefold/config_case_sensitivity.go:41` |
| `configCaseDirectoryMaxEntries` | const | `io/casefold/config_case_sensitivity.go:42` |
| `configCaseDirectoryMaxNameBytes` | const | `io/casefold/config_case_sensitivity.go:43` |
| `errConfigCaseDirectoryBudgetExceeded` | var | `io/casefold/config_case_sensitivity.go:46` |
| `configLinuxCaseSensitivityFromMetadata` | func | `io/casefold/config_case_sensitivity.go:48` |
| `configFreeBSDCaseSensitivityFromFilesystemName` | func | `io/casefold/config_case_sensitivity.go:57` |
| `configCaseDirectoryBudgetExceeded` | func | `io/casefold/config_case_sensitivity.go:61` |
| `configCaseDirectoryEntry` | type | `io/casefold/config_case_sensitivity.go:75` |
| `configCaseDirectorySnapshot` | type | `io/casefold/config_case_sensitivity.go:80` |
| `configCaseSensitivityFromDirectorySnapshots` | func | `io/casefold/config_case_sensitivity.go:87` |
| `configCaseDirectorySnapshotsEqual` | func | `io/casefold/config_case_sensitivity.go:135` |
| `configCaseDirectoryEntriesEqual` | func | `io/casefold/config_case_sensitivity.go:141` |
| `configCaseDirectoryHasVariantNames` | func | `io/casefold/config_case_sensitivity.go:153` |
| `configASCIICaseVariant` | func | `io/casefold/config_case_sensitivity.go:165` |
| `configASCIINameFold` | func | `io/casefold/config_case_sensitivity.go:180` |
| `configDarwinCaseSensitivityFromPathconf` | func | `io/casefold/config_case_sensitivity.go:190` |
| `configDirectoryCaseSensitivityByEntries` | func | `io/casefold/config_case_sensitivity_behavioral_unix.go:11` |
| `configReadCaseDirectorySnapshot` | func | `io/casefold/config_case_sensitivity_behavioral_unix.go:29` |
| `darwinPathconfCaseSensitive` | const | `io/casefold/config_case_sensitivity_darwin.go:12` |
| `configPlatformCaseFoldLeaf` | func | `io/casefold/config_case_sensitivity_darwin.go:16` |
| `configPlatformCaseSensitivity` | func | `io/casefold/config_case_sensitivity_darwin.go:20` |
| `configPlatformCaseFoldLeaf` | func | `io/casefold/config_case_sensitivity_freebsd.go:11` |
| `configPlatformCaseSensitivity` | func | `io/casefold/config_case_sensitivity_freebsd.go:15` |
| `configFreeBSDDirectoryCaseSensitivity` | func | `io/casefold/config_case_sensitivity_freebsd.go:19` |
| `configPlatformCaseFoldLeaf` | func | `io/casefold/config_case_sensitivity_linux.go:11` |
| `configPlatformCaseSensitivity` | func | `io/casefold/config_case_sensitivity_linux.go:15` |
| `configLinuxDirectoryCaseSensitivity` | func | `io/casefold/config_case_sensitivity_linux.go:19` |
| `configPlatformCaseFoldLeaf` | func | `io/casefold/config_case_sensitivity_other_unix.go:5` |
| `configPlatformCaseSensitivity` | func | `io/casefold/config_case_sensitivity_other_unix.go:9` |
| `configPlatformCaseFoldLeaf` | func | `io/casefold/config_case_sensitivity_windows.go:17` |
| `configPlatformCaseSensitivity` | func | `io/casefold/config_case_sensitivity_windows.go:21` |
| `windowsConfigDirectoryCaseInsensitive` | func | `io/casefold/config_case_sensitivity_windows.go:32` |
| `configWindowsCaseInsensitiveFromFlags` | func | `io/casefold/config_case_sensitivity_windows.go:61` |
| `windowsCaseSensitivityInfoUnsupported` | func | `io/casefold/config_case_sensitivity_windows.go:65` |

#### `cfgcommit` — 25 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `errConfigCommitCommitted` | var | `io/cfgcommit/atomicwrite.go:20` |
| `errConfigCommitUncertain` | var | `io/cfgcommit/atomicwrite.go:25` |
| `errConfigCommitDurabilityUncertain` | var | `io/cfgcommit/atomicwrite.go:30` |
| `errAtomicWriteCommitted` | var | `io/cfgcommit/atomicwrite.go:40` |
| `CommitOutcome` | type | `io/cfgcommit/atomicwrite.go:50` |
| `*CommitOutcome.Error` | method | `io/cfgcommit/atomicwrite.go:58` |
| `*CommitOutcome.Unwrap` | method | `io/cfgcommit/atomicwrite.go:68` |
| `CommitOutcomeFromError` | func | `io/cfgcommit/atomicwrite.go:78` |
| `newCommitOutcome` | func | `io/cfgcommit/atomicwrite.go:275` |
| `newMaybeCommitOutcome` | func | `io/cfgcommit/atomicwrite.go:284` |
| `cloneCommitOutcome` | func | `io/cfgcommit/atomicwrite.go:292` |
| `commitPostCommitCauses` | func | `io/cfgcommit/atomicwrite.go:301` |
| `verifySelectedCommitPath` | func | `io/cfgcommit/atomicwrite.go:315` |
| `caseEquivalentConfigPaths` | func | `io/cfgcommit/atomicwrite.go:328` |
| `atomicWriteFile` | func | `io/cfgcommit/atomicwrite.go:340` |
| `stageConfigFile` | func | `io/cfgcommit/atomicwrite.go:362` |
| `commitConfigFile` | func | `io/cfgcommit/config_commit_unix.go:21` |
| `configTempNameAttempts` | const | `io/cfgcommit/config_commit_unix.go:216` |
| `openUniqueConfigTemp` | func | `io/cfgcommit/config_commit_unix.go:218` |
| `verifyCommitEntry` | func | `io/cfgcommit/config_commit_unix.go:236` |
| `commitConfigFile` | func | `io/cfgcommit/config_commit_windows.go:18` |
| `verifyWindowsCommitInput` | func | `io/cfgcommit/config_commit_windows.go:164` |
| `verifyWindowsCommitParent` | func | `io/cfgcommit/config_commit_windows.go:180` |
| `verifyWindowsCommitDestinationHandle` | func | `io/cfgcommit/config_commit_windows.go:211` |
| `classifyWindowsRenameFailure` | func | `io/cfgcommit/config_commit_windows.go:253` |

#### `cfghooks` — 11 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `runConfigBeforeOpenHook` | func | `io/cfgcommit/atomicwrite.go:115` |
| `runConfigAfterStableReadHook` | func | `io/cfgcommit/atomicwrite.go:124` |
| `runConfigBeforeCommitCheckHook` | func | `io/cfgcommit/atomicwrite.go:133` |
| `runConfigAfterCommitRenameHook` | func | `io/cfgcommit/atomicwrite.go:142` |
| `runConfigAfterCommitRenameHookForPath` | func | `io/cfgcommit/atomicwrite.go:152` |
| `runConfigAfterMCPCommitHook` | func | `io/cfgcommit/atomicwrite.go:166` |
| `runConfigBeforeCommitRenameHook` | func | `io/cfgcommit/atomicwrite.go:193` |
| `runConfigBeforeMCPReconcileHook` | func | `io/cfgcommit/atomicwrite.go:202` |
| `acquireConfigFileLock` | func | `io/cfgcommit/atomicwrite.go:211` |
| `releaseConfigFileLock` | func | `io/cfgcommit/atomicwrite.go:221` |
| `configContextWithTimeout` | func | `io/cfgcommit/atomicwrite.go:231` |

#### `docread` — 106 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `sameBytesFingerprint` | func | `io/cfgcommit/atomicwrite.go:271` |
| `readConfigEntryAt` | func | `io/cfgcommit/config_linkat_unix.go:150` |
| `lookupConfigCandidates` | func | `io/docread/candidates.go:16` |
| `eligibleConfigCandidate` | func | `io/docread/candidates.go:23` |
| `eligibleWorkspaceConfig` | func | `io/docread/candidates.go:56` |
| `pathAlreadyLoaded` | func | `io/docread/candidates.go:89` |
| `configOwnerForWorkingDir` | func | `io/docread/candidates.go:99` |
| `configOwnerPolicyForPath` | func | `io/docread/candidates.go:110` |
| `fileSnapshot` | type | `io/docread/fingerprints.go:19` |
| `reloadFileFingerprint` | type | `io/docread/fingerprints.go:28` |
| `configAliasChainFingerprint` | func | `io/docread/fingerprints.go:47` |
| `configDiscoveryFingerprint` | func | `io/docread/fingerprints.go:92` |
| `reloadStalenessState` | func | `io/docread/fingerprints.go:129` |
| `mcpJSONFile` | type | `io/docread/mcpdocs.go:20` |
| `mcpJSONEntry` | type | `io/docread/mcpdocs.go:25` |
| `loadMCPJSON` | func | `io/docread/mcpdocs.go:35` |
| `loadMCPJSONBytes` | func | `io/docread/mcpdocs.go:44` |
| `mcpJSONCandidatePaths` | func | `io/docread/mcpdocs.go:77` |
| `discoverMCPJSONFiles` | func | `io/docread/mcpdocs.go:99` |
| `configAndMCPStalenessPaths` | func | `io/docread/mcpdocs.go:109` |
| `loadExternalMCPServers` | func | `io/docread/mcpdocs.go:115` |
| `loadExternalMCPServersFromPaths` | func | `io/docread/mcpdocs.go:122` |
| `loadExternalMCPServersFromStablePaths` | func | `io/docread/mcpdocs.go:138` |
| `loadExternalMCPServersFromStableDocuments` | func | `io/docread/mcpdocs.go:146` |
| `loadExternalMCPDocumentsStable` | func | `io/docread/mcpdocs.go:151` |
| `loadExternalMCPDocumentsStableForWorkingDir` | func | `io/docread/mcpdocs.go:155` |
| `mergeExternalMCPServersFromDocuments` | func | `io/docread/mcpdocs.go:217` |
| `mcpDisabledOverlays` | func | `io/docread/mcpdocs.go:246` |
| `isMCPDisabledOnlyEntry` | func | `io/docread/mcpdocs.go:299` |
| `mcpEntryFromJSON` | func | `io/docread/mcpdocs.go:307` |
| `normalizeReloadPath` | func | `io/docread/normalize.go:9` |
| `normalizeDiscoveryPath` | func | `io/docread/normalize.go:16` |
| `ErrConfigReloadUnstable` | var | `io/docread/normalize.go:29` |
| `stableConfigDocument` | type | `io/docread/stabledocs.go:17` |
| `stableConfigAlias` | type | `io/docread/stabledocs.go:25` |
| `readStableConfigDocumentsWithOwner` | func | `io/docread/stabledocs.go:57` |
| `readStableConfigFileOwned` | func | `io/docread/stabledocs.go:117` |
| `readStableConfigFile` | func | `io/docread/stabledocs.go:207` |
| `readStableConfigHandle` | func | `io/fileid/config_file_identity_windows.go:368` |
| `warnKtavArtifactConflictOnce` | func | `io/slotmigrate/ktav_migration_artifacts.go:493` |
| `ktavConfigExt` | const | `io/slots/ktav_discovery.go:24` |
| `ktavBackupSuffix` | const | `io/slots/ktav_discovery.go:26` |
| `ktavMigrationRecordSuffix` | const | `io/slots/ktav_discovery.go:29` |
| `errKtavBackupOnly` | var | `io/slots/ktav_discovery.go:35` |
| `errKtavMigrationConflict` | var | `io/slots/ktav_discovery.go:38` |
| `errKtavMigrationRecord` | var | `io/slots/ktav_discovery.go:41` |
| `ktavSiblingOf` | func | `io/slots/ktav_discovery.go:45` |
| `configSlotBaseNames` | func | `io/slots/ktav_discovery.go:53` |
| `expandConfigSlotSiblings` | func | `io/slots/ktav_discovery.go:65` |
| `backupPathFor` | func | `io/slots/ktav_discovery.go:86` |
| `migrationRecordPathFor` | func | `io/slots/ktav_discovery.go:93` |
| `isKtavConfigPath` | func | `io/slots/ktav_discovery.go:98` |
| `isKtavBackupPath` | func | `io/slots/ktav_discovery.go:104` |
| `isKtavMigrationRecordPath` | func | `io/slots/ktav_discovery.go:110` |
| `ktavBothFormatsWarned` | var | `io/slots/ktav_discovery.go:114` |
| `warnBothFormatsOnce` | func | `io/slots/ktav_discovery.go:118` |
| `warnMigrationDeferredOnce` | func | `io/slots/ktav_discovery.go:136` |
| `selectConfigSlot` | func | `io/slots/ktav_discovery.go:151` |
| `decodeConfigDocument` | func | `io/slots/ktav_discovery.go:177` |
| `workspaceConfigPath` | func | `io/slots/ktav_discovery.go:194` |
| `matchesLogicalSlot` | func | `io/slots/ktav_discovery.go:205` |
| `checkBackupOnlySlot` | func | `io/slots/ktav_discovery.go:215` |
| `checkAuthoritativeBackupOnly` | func | `io/slots/ktav_discovery.go:233` |
| `ktavProjectLegacySlots` | func | `io/slots/ktav_discovery.go:252` |
| `ktavMigrationPendingSlots` | func | `io/slots/ktav_discovery.go:282` |
| `ktavEntryExists` | func | `io/slots/ktav_discovery.go:309` |
| `ktavEntryAbsentErr` | func | `io/slots/ktav_discovery.go:319` |
| `fileExists` | func | `io/slots/ktav_discovery.go:323` |
| `settingsDocumentFormat` | type | `io/slots/ktav_doc.go:17` |
| `settingsDocumentJSON` | const | `io/slots/ktav_doc.go:21` |
| `settingsDocumentKtav` | const | `io/slots/ktav_doc.go:23` |
| `settingsDocument` | type | `io/slots/ktav_doc.go:30` |
| `settingsDocumentFormatOf` | func | `io/slots/ktav_doc.go:39` |
| `decodeSettingsDocument` | func | `io/slots/ktav_doc.go:50` |
| `readSettingsDocument` | func | `io/slots/ktav_doc.go:81` |
| `encodeSettingsDocument` | func | `io/slots/ktav_doc.go:99` |
| `rejectUnconvertedKtavWrite` | func | `io/slots/ktav_doc.go:114` |
| `legacySlotOf` | func | `io/slots/ktav_doc.go:140` |
| `configWriteLockAnchor` | func | `io/slots/ktav_doc.go:152` |
| `readSlotOf` | func | `io/slots/ktav_doc.go:170` |
| `oauthJournalRecord` | type | `io/slots/oauthjournal.go:19` |
| `errOAuthJournalInvalid` | var | `io/slots/oauthjournal.go:25` |
| `validOAuthJournal` | func | `io/slots/oauthjournal.go:27` |
| `decodeOAuthJournalDiskToken` | func | `io/slots/oauthjournal.go:62` |
| `oauthJournalDigest` | func | `io/slots/oauthjournal.go:99` |
| `oauthJournalLegacyPath` | func | `io/slots/oauthjournal.go:108` |
| `oauthJournalKtavPath` | func | `io/slots/oauthjournal.go:116` |
| `configCandidateGroups` | type | `load_files.go:34` |
| `lookupConfigCandidateGroups` | func | `load_files.go:39` |
| `configOwnersMatch` | func | `load_files.go:90` |
| `configDocumentBytes` | func | `load_files.go:116` |
| `addDocumentAliasFingerprints` | func | `load_files.go:133` |
| `errOAuthJournalDisk` | var | `store_oauth_journal.go:22` |
| `stableReadMaxAttempts` | const | `store_reload.go:26` |
| `configFingerprintPath` | func | `store_reload.go:31` |
| `writeConfigDiscoveryInfo` | func | `store_reload.go:40` |
| `captureReloadFingerprints` | func | `store_reload.go:549` |
| `addReloadFingerprint` | func | `store_reload.go:559` |
| `readReloadFingerprint` | func | `store_reload.go:575` |
| `reloadFingerprintsChanged` | func | `store_reload.go:583` |
| `reloadFingerprintsChangedWithOwner` | func | `store_reload.go:587` |
| `sameReloadPathSet` | func | `store_reload.go:612` |
| `trackedSlotFingerprint` | func | `store_reload.go:639` |
| `reloadFingerprintForPath` | func | `store_reload.go:646` |
| `rebindReloadFingerprintPath` | func | `store_reload.go:662` |
| `errKtavWriteNotSupported` | var | `store_write.go:50` |

#### `fileid` — 49 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `errConfigCommitVerification` | var | `io/cfgcommit/atomicwrite.go:16` |
| `ErrConfigHardLink` | var | `io/cfgcommit/atomicwrite.go:35` |
| `physicalWindowsConfigPath` | func | `io/cfgcommit/config_commit_windows.go:197` |
| `configFileNlinkOfInfo` | func | `io/cfgcommit/config_linkat_unix.go:208` |
| `errConfigOwnerMismatch` | var | `io/fileid/config_file_identity.go:11` |
| `errStableReadUnstable` | var | `io/fileid/config_file_identity.go:13` |
| `ErrConfigNonRegular` | var | `io/fileid/config_file_identity.go:17` |
| `readOpenedConfigBytes` | func | `io/fileid/config_file_identity.go:19` |
| `configFileIdentity` | type | `io/fileid/config_file_identity_unix.go:13` |
| `configFileIdentityOf` | func | `io/fileid/config_file_identity_unix.go:19` |
| `configFileIdentityOfOpened` | func | `io/fileid/config_file_identity_unix.go:27` |
| `configFileIdentityAtPath` | func | `io/fileid/config_file_identity_unix.go:31` |
| `configFileOwner` | func | `io/fileid/config_file_identity_unix.go:46` |
| `configFileNlinkOfOpened` | func | `io/fileid/config_file_identity_unix.go:54` |
| `sameConfigFileIdentity` | func | `io/fileid/config_file_identity_unix.go:62` |
| `configFilePathIdentityMatches` | func | `io/fileid/config_file_identity_unix.go:68` |
| `configFileIdentity` | type | `io/fileid/config_file_identity_windows.go:23` |
| `configFileIdentityOf` | func | `io/fileid/config_file_identity_windows.go:29` |
| `configFileIdentityOfOpened` | func | `io/fileid/config_file_identity_windows.go:31` |
| `configFileIdentityAtPath` | func | `io/fileid/config_file_identity_windows.go:43` |
| `configFileNlinkAtPath` | func | `io/fileid/config_file_identity_windows.go:67` |
| `removeConfigTempIfIdentity` | func | `io/fileid/config_file_identity_windows.go:87` |
| `openWindowsConfigHandle` | func | `io/fileid/config_file_identity_windows.go:114` |
| `windowsConfigAPIPath` | func | `io/fileid/config_file_identity_windows.go:139` |
| `windowsFileInformation` | func | `io/fileid/config_file_identity_windows.go:157` |
| `openWindowsConfigParent` | func | `io/fileid/config_file_identity_windows.go:163` |
| `windowsStagedConfigFile` | type | `io/fileid/config_file_identity_windows.go:181` |
| `configTempNameAttempts` | const | `io/fileid/config_file_identity_windows.go:188` |
| `stageConfigFileHandle` | func | `io/fileid/config_file_identity_windows.go:190` |
| `stageConfigFileHandleAt` | func | `io/fileid/config_file_identity_windows.go:194` |
| `createWindowsConfigEntryAt` | func | `io/fileid/config_file_identity_windows.go:281` |
| `openWindowsConfigEntryAt` | func | `io/fileid/config_file_identity_windows.go:318` |
| `isWindowsEntryNotFound` | func | `io/fileid/config_file_identity_windows.go:332` |
| `deleteWindowsConfigHandle` | func | `io/fileid/config_file_identity_windows.go:336` |
| `flushWindowsConfigHandle` | func | `io/fileid/config_file_identity_windows.go:358` |
| `configFileOwner` | func | `io/fileid/config_file_identity_windows.go:407` |
| `configFileNlinkOfOpened` | func | `io/fileid/config_file_identity_windows.go:409` |
| `sameConfigFileIdentity` | func | `io/fileid/config_file_identity_windows.go:417` |
| `configFilePathIdentityMatches` | func | `io/fileid/config_file_identity_windows.go:424` |
| `homeConfigOwner` | func | `io/fileid/config_owner_unix.go:9` |
| `systemConfigOwner` | func | `io/fileid/config_owner_unix.go:12` |
| `homeConfigOwner` | func | `io/fileid/config_owner_windows.go:7` |
| `systemConfigOwner` | func | `io/fileid/config_owner_windows.go:9` |
| `configParentIdentity` | func | `io/fileid/config_parent_identity_unix.go:10` |
| `configParentIdentity` | func | `io/fileid/config_parent_identity_windows.go:5` |
| `openStableConfigFile` | func | `io/fswap/config_open_unix.go:11` |
| `writeStableConfigFile` | func | `io/fswap/config_open_unix.go:38` |
| `openStableConfigFile` | func | `io/fswap/config_open_windows.go:12` |
| `writeStableConfigFile` | func | `io/fswap/config_open_windows.go:18` |

#### `fswap` — 28 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `syncConfigParent` | func | `io/cfgcommit/atomicwrite.go:241` |
| `syncConfigParentFile` | func | `io/cfgcommit/atomicwrite.go:251` |
| `syncConfigParentOnDisk` | func | `io/cfgcommit/config_durability_unix.go:12` |
| `syncConfigParentOnDisk` | func | `io/cfgcommit/config_durability_windows.go:14` |
| `publishConfigTempNoReplaceAt` | func | `io/cfgcommit/config_linkat_unix.go:19` |
| `configTempIdentityAt` | func | `io/cfgcommit/config_linkat_unix.go:52` |
| `cleanupConfigTempAlias` | func | `io/cfgcommit/config_linkat_unix.go:73` |
| `recoverConfigTempAlias` | func | `io/cfgcommit/config_linkat_unix.go:87` |
| `configEntryInfoAt` | func | `io/cfgcommit/config_linkat_unix.go:194` |
| `cleanupConfigTempAliasOwned` | func | `io/cfgcommit/config_linkat_unix.go:212` |
| `configTempAliasPolicyAcceptable` | func | `io/cfgcommit/config_linkat_unix.go:226` |
| `isConfigTempAliasName` | func | `io/cfgcommit/config_linkat_unix.go:234` |
| `unlinkConfigTempAt` | func | `io/cfgcommit/config_linkat_unix.go:248` |
| `configWriteIdentityKey` | func | `io/cfgcommit/config_write_identity.go:48` |
| `configDirectoryIsCaseInsensitive` | func | `io/cfgcommit/config_write_identity.go:52` |
| `configEquivalentLeafNames` | func | `io/cfgcommit/config_write_identity.go:57` |
| `renameConfigTempAt` | func | `io/fswap/config_rename_darwin.go:13` |
| `renameConfigTemp` | func | `io/fswap/config_rename_unix.go:7` |
| `windowsFileRenameInfo` | type | `io/fswap/config_rename_windows.go:17` |
| `renameConfigTempHandle` | func | `io/fswap/config_rename_windows.go:24` |
| `windowsRenameRetryAttempts` | const | `io/fswap/config_rename_windows.go:142` |
| `waitAndVerifyWindowsRenameRetry` | func | `io/fswap/config_rename_windows.go:144` |
| `verifyWindowsRenameRetryState` | func | `io/fswap/config_rename_windows.go:153` |
| `isUnsupportedRenameError` | func | `io/fswap/config_rename_windows.go:214` |
| `hasSetFileInformationHook` | func | `io/fswap/config_rename_windows.go:218` |
| `renameConfigTemp` | func | `io/fswap/config_rename_windows.go:228` |
| `renameConfigTempAt` | func | `io/fswap/config_renameat2_linux.go:11` |
| `renameConfigTempAt` | func | `io/fswap/config_renameat2_other_unix.go:7` |

#### `slotmigrate` — 38 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `ktavMigrationRecordVersion` | const | `io/slotmigrate/ktav_migration.go:29` |
| `errKtavMigrationCrashInjected` | var | `io/slotmigrate/ktav_migration.go:32` |
| `runKtavMigrationStep` | func | `io/slotmigrate/ktav_migration.go:36` |
| `ktavMigrationCheckContext` | func | `io/slotmigrate/ktav_migration.go:43` |
| `ktavMigrationCommitHook` | func | `io/slotmigrate/ktav_migration.go:50` |
| `ktavMigrationRecord` | type | `io/slotmigrate/ktav_migration.go:65` |
| `ktavMigrationEntries` | type | `io/slotmigrate/ktav_migration.go:77` |
| `inspectKtavMigrationEntries` | func | `io/slotmigrate/ktav_migration.go:84` |
| `migrateLegacyConfigFile` | func | `io/slotmigrate/ktav_migration.go:93` |
| `runKtavMigrationState` | func | `io/slotmigrate/ktav_migration.go:108` |
| `runKtavMigrationStateLocked` | func | `io/slotmigrate/ktav_migration.go:129` |
| `runKtavMigrationTransaction` | func | `io/slotmigrate/ktav_migration.go:209` |
| `finishKtavArchival` | func | `io/slotmigrate/ktav_migration.go:340` |
| `loadKtavMigrationRecord` | func | `io/slotmigrate/ktav_migration.go:387` |
| `validKtavMigrationDigest` | func | `io/slotmigrate/ktav_migration.go:416` |
| `verifyKtavGenerations` | func | `io/slotmigrate/ktav_migration.go:424` |
| `ktavKeepsLegacy` | func | `io/slotmigrate/ktav_migration.go:466` |
| `removeKtavMigrationRecord` | func | `io/slotmigrate/ktav_migration.go:470` |
| `ownerPolicyForMigration` | func | `io/slotmigrate/ktav_migration.go:477` |
| `runKtavArtifactMigrationStep` | func | `io/slotmigrate/ktav_migration_artifacts.go:40` |
| `ktavArtifactCheckContext` | func | `io/slotmigrate/ktav_migration_artifacts.go:47` |
| `ktavArtifactMigrationEntries` | func | `io/slotmigrate/ktav_migration_artifacts.go:59` |
| `migrateKtavArtifact` | func | `io/slotmigrate/ktav_migration_artifacts.go:72` |
| `migrateKtavArtifactWithLock` | func | `io/slotmigrate/ktav_migration_artifacts.go:78` |
| `migrateKtavArtifactToWithLock` | func | `io/slotmigrate/ktav_migration_artifacts.go:90` |
| `runKtavArtifactMigrationStateLocked` | func | `io/slotmigrate/ktav_migration_artifacts.go:113` |
| `runKtavArtifactMigrationTransaction` | func | `io/slotmigrate/ktav_migration_artifacts.go:204` |
| `ktavArtifactFinishArchival` | func | `io/slotmigrate/ktav_migration_artifacts.go:346` |
| `ktavArtifactPreserveMTime` | func | `io/slotmigrate/ktav_migration_artifacts.go:374` |
| `applyKtavMigrationModelsIntegrity` | func | `io/slotmigrate/ktav_migration_cache_rev.go:34` |
| `ktavSymlinkDeferredReason` | const | `io/slotmigrate/ktav_migration_symlink.go:20` |
| `isKtavSymlinkPath` | func | `io/slotmigrate/ktav_migration_symlink.go:22` |
| `runKtavSymlinkMigrationState` | func | `io/slotmigrate/ktav_migration_symlink.go:31` |
| `recoverKtavSymlinkBeforePublication` | func | `io/slotmigrate/ktav_migration_symlink.go:57` |
| `finishKtavSymlinkMigration` | func | `io/slotmigrate/ktav_migration_symlink.go:101` |
| `migrateKtavSymlinkedConfig` | func | `io/slotmigrate/ktav_migration_symlink.go:138` |
| `archiveKtavBackup` | func | `io/slotmigrate/ktav_migration_unix.go:17` |
| `archiveKtavBackup` | func | `io/slotmigrate/ktav_migration_windows.go:19` |

#### `slots` — 13 declarations

| Original identifier | Kind | Original source |
|---|---|---|
| `ktavMigrateStartupSlots` | func | `io/slotmigrate/ktav_migration.go:489` |
| `ktavMigrateWorkspaceIfNeeded` | func | `io/slotmigrate/ktav_migration.go:529` |
| `ktavMigrateCacheArtifactsStartup` | func | `io/slotmigrate/ktav_migration_artifacts.go:507` |
| `ktavCacheArtifactPaths` | func | `io/slotmigrate/ktav_migration_artifacts.go:530` |
| `readCacheDocument` | func | `io/slots/ktav_cache.go:25` |
| `writeCacheDocument` | func | `io/slots/ktav_cache.go:57` |
| `newConfigFileIsKtav` | func | `io/slots/ktav_doc.go:124` |
| `writeTargetSlot` | func | `io/slots/ktav_doc.go:190` |
| `ReadConfigDocumentLight` | func | `io/slots/ktav_light.go:19` |
| `ActiveConfigFileName` | func | `io/slots/ktav_light.go:39` |
| `SetKtavAutoMigrateForTest` | func | `io/slots/ktav_light.go:46` |
| `KtavAutoMigrateEnabled` | func | `io/slots/ktav_light.go:55` |
| `selectOAuthJournalPath` | func | `io/slots/ktav_oauth.go:14` |

#### Production and state declarations moved back to ROOT — 27 rows

These retain original spellings and source locations; `ktavAutoMigrate` is intentionally root-owned physical state. Its lower-package/API contract is described below without exporting a mutable global.

| Original identifier | Kind | Original IO source |
|---|---|---|
| `configTestHooks` | var | `io/cfgcommit/atomicwrite.go:86` |
| `runConfigAfterMCPResolveTargetHook` | func | `io/cfgcommit/atomicwrite.go:175` |
| `runConfigBeforeMCPTargetBindingVerificationHook` | func | `io/cfgcommit/atomicwrite.go:184` |
| `SetBeforeConfigCommitForTest` | func | `io/cfgcommit/config_commit_test_seam.go:4` |
| `configWriteTargetDedupKey` | func | `io/cfgcommit/config_write_identity.go:12` |
| `configWriteTargetDedupKeyWithCaseFold` | func | `io/cfgcommit/config_write_identity.go:16` |
| `configWriteTargetDedupKeyWithCaseSensitivity` | func | `io/cfgcommit/config_write_identity.go:42` |
| `candidateInputsChanged` | func | `io/docread/fingerprints.go:166` |
| `errReloadDiskChanged` | var | `io/docread/fingerprints.go:178` |
| `initialMCPRevisions` | func | `io/docread/fingerprints.go:180` |
| `mcpInputFingerprints` | func | `io/docread/fingerprints.go:193` |
| `mergeExternalMCPServers` | func | `io/docread/mcpdocs.go:197` |
| `readMCPDisabledOverride` | func | `io/docread/mcpdocs.go:272` |
| `mcpRevisionDiff` | func | `io/docread/mcpdocs.go:328` |
| `mcpNames` | func | `io/docread/mcpdocs.go:370` |
| `loadConfigCandidateStableForWorkingDir` | func | `io/docread/stabledocs.go:30` |
| `ktavAutoMigrate` | var | `io/slotmigrate/ktav_migration.go:25` |
| `ktavMigrationBudget` | var | `io/slotmigrate/ktav_migration.go:26` |
| `ktavMigrationStepHook` | var | `io/slotmigrate/ktav_migration.go:33` |
| `ktavArtifactMigrationStepHook` | var | `io/slotmigrate/ktav_migration_artifacts.go:32` |
| `ktavArtifactStatFile` | var | `io/slotmigrate/ktav_migration_artifacts.go:36` |
| `ktavArtifactChtimes` | var | `io/slotmigrate/ktav_migration_artifacts.go:37` |
| `ktavMigrateOAuthJournalsAtStartup` | func | `io/slotmigrate/ktav_migration_artifacts.go:392` |
| `ktavValidateOAuthJournalBytes` | func | `io/slotmigrate/ktav_migration_artifacts.go:420` |
| `ktavMigrateOAuthJournal` | func | `io/slotmigrate/ktav_migration_artifacts.go:435` |
| `oauthJournalConfigSyncParent` | var | `io/slots/oauthjournal.go:16` |
| `readOAuthJournalWithRetry` | func | `io/slots/oauthjournal.go:47` |

### B. Cross-owner edge symbols and evidence

Rows are emitted from every projection edge incident to one of the eight IO owners. The symbol list under each edge is exhaustive for that edge (`names` in the projection); each symbol has its ledger reference count and one concrete `file:line` witness (the lexicographically first artifact evidence location). Counts are identifier references, not calls.

#### `ROOT` → `casefold` — 8 references, 1 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configCaseSensitivity` | 4 | 0 | `io/cfgcommit/config_write_identity.go:42` (`configWriteTargetDedupKeyWithCaseSensitivity`) |
| `configCaseSensitivityUnknown` | 1 | 0 | `store_mcp_locks.go:207` (`configMCPAdmissionLockGroups`) |
| `configFoldCaseLeaf` | 1 | 1 | `io/cfgcommit/config_write_identity.go:44` (`configWriteTargetDedupKeyWithCaseSensitivity`) |
| `configPlatformCaseFoldLeaf` | 1 | 0 | `io/cfgcommit/config_write_identity.go:13` (`configWriteTargetDedupKey`) |
| `configPlatformCaseSensitivity` | 1 | 0 | `store_mcp_locks.go:189` (`*ConfigStore.withMCPLocks`) |

#### `ROOT` → `cfgcommit` — 39 references, 26 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `CommitOutcomeFromError` | 13 | 13 | `settings_lock.go:525` (`*ConfigStore.writeSettingsLockRaw`) |
| `atomicWriteFile` | 7 | 3 | `api_remotes.go:321` (`WriteAPIRemoteTokenFile`) |
| `cloneCommitOutcome` | 4 | 4 | `store_mcp_transaction.go:399` (`*ConfigStore.writeMCPFileChanges`) |
| `commitConfigFile` | 5 | 5 | `settings_lock.go:523` (`*ConfigStore.writeSettingsLockRaw`) |
| `errConfigCommitCommitted` | 2 | 0 | `store_mcp_transaction.go:409` (`*ConfigStore.writeMCPFileChanges`) |
| `errConfigCommitDurabilityUncertain` | 2 | 0 | `store_mcp_transaction.go:386` (`*ConfigStore.writeMCPFileChanges`) |
| `errConfigCommitUncertain` | 5 | 0 | `store_mcp_transaction.go:409` (`*ConfigStore.writeMCPFileChanges`) |
| `newCommitOutcome` | 1 | 1 | `store_mcp_transaction.go:447` (`*ConfigStore.writeMCPFileChanges`) |

#### `ROOT` → `cfghooks` — 30 references, 30 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `acquireConfigFileLock` | 9 | 9 | `store_mcp_locks.go:302` (`*ConfigStore.withMCPLocksUsingCaseSensitivity`) |
| `configContextWithTimeout` | 4 | 4 | `store_mcp_admission.go:229` (`*ConfigStore.WithCurrentMCPAdmission`) |
| `releaseConfigFileLock` | 15 | 15 | `store_oauth_disk_sync.go:185` (`*ConfigStore.drainOAuthOutboxOnStop`) |
| `runConfigAfterMCPCommitHook` | 1 | 1 | `store_mcp_transaction.go:444` (`*ConfigStore.writeMCPFileChanges`) |
| `runConfigBeforeMCPReconcileHook` | 1 | 1 | `store_mcp_transaction.go:461` (`*ConfigStore.reconcileMCPCommit`) |

#### `ROOT` → `docread` — 437 references, 278 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigReloadUnstable` | 3 | 0 | `load.go:107` (`Load`) |
| `addDocumentAliasFingerprints` | 1 | 1 | `io/docread/stabledocs.go:48` (`loadConfigCandidateStableForWorkingDir`) |
| `backupPathFor` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:457` (`ktavMigrateOAuthJournal`) |
| `checkAuthoritativeBackupOnly` | 2 | 2 | `load.go:111` (`loadOnce`) |
| `checkBackupOnlySlot` | 3 | 3 | `load.go:146` (`loadOnce`) |
| `configAliasChainFingerprint` | 3 | 3 | `store_mcp_admission.go:377` (`mcpAdmissionAbsenceFingerprint`) |
| `configAndMCPStalenessPaths` | 2 | 2 | `store.go:395` (`*ConfigStore.publishInitialSnapshot`) |
| `configDiscoveryFingerprint` | 8 | 8 | `store_mcp_admission.go:374` (`mcpAdmissionAbsenceFingerprint`) |
| `configDocumentBytes` | 1 | 1 | `io/docread/stabledocs.go:47` (`loadConfigCandidateStableForWorkingDir`) |
| `configOwnerForWorkingDir` | 5 | 5 | `load.go:174` (`loadOnce`) |
| `configOwnerPolicyForPath` | 2 | 2 | `io/docread/fingerprints.go:174` (`candidateInputsChanged`) |
| `configWriteLockAnchor` | 4 | 4 | `apitoken_store.go:488` (`*APITokenStore.mutate`) |
| `decodeConfigDocument` | 10 | 10 | `load.go:181` (`loadOnce`) |
| `decodeOAuthJournalDiskToken` | 3 | 3 | `store_oauth_disk_sync.go:105` (`*ConfigStore.syncOAuthFromDisk`) |
| `decodeSettingsDocument` | 7 | 7 | `apitoken_store.go:405` (`*APITokenStore.read`) |
| `eligibleConfigCandidate` | 2 | 2 | `store_mcp_transaction.go:595` (`*ConfigStore.orderedMCPPaths`) |
| `encodeSettingsDocument` | 7 | 7 | `apitoken_store.go:524` (`*APITokenStore.mutate`) |
| `errKtavMigrationConflict` | 1 | 0 | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `errOAuthJournalDisk` | 33 | 0 | `store_oauth_journal.go:158` (`writeOAuthJournalOnce`) |
| `errOAuthJournalInvalid` | 12 | 0 | `io/slotmigrate/ktav_migration_artifacts.go:423` (`ktavValidateOAuthJournalBytes`) |
| `expandConfigSlotSiblings` | 2 | 2 | `store_config_disk_sync.go:296` (`configDiskSyncCandidatePaths`) |
| `fileExists` | 1 | 1 | `load.go:152` (`loadOnce`) |
| `fileSnapshot` | 6 | 0 | `store.go:46` (`storeSnapshot`) |
| `isKtavConfigPath` | 1 | 1 | `io/docread/fingerprints.go:224` (`mcpInputFingerprints`) |
| `isMCPDisabledOnlyEntry` | 3 | 3 | `io/docread/mcpdocs.go:285` (`readMCPDisabledOverride`) |
| `ktavEntryExists` | 6 | 6 | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `ktavSiblingOf` | 5 | 5 | `load.go:152` (`loadOnce`) |
| `legacySlotOf` | 3 | 3 | `io/slotmigrate/ktav_migration_artifacts.go:397` (`ktavMigrateOAuthJournalsAtStartup`) |
| `loadExternalMCPDocumentsStableForWorkingDir` | 2 | 2 | `load.go:236` (`loadOnce`) |
| `loadMCPJSONBytes` | 1 | 1 | `store_mcp_transaction.go:570` (`*ConfigStore.evaluateMCPFiles`) |
| `lookupConfigCandidateGroups` | 1 | 1 | `load_files.go:21` (`lookupConfigs`) |
| `lookupConfigCandidates` | 8 | 8 | `io/docread/fingerprints.go:167` (`candidateInputsChanged`) |
| `matchesLogicalSlot` | 5 | 5 | `store_mcp_transaction.go:688` (`*ConfigStore.mcpOwnerPolicy`) |
| `mcpEntryFromJSON` | 2 | 2 | `io/docread/mcpdocs.go:284` (`readMCPDisabledOverride`) |
| `mcpJSONCandidatePaths` | 5 | 5 | `io/docread/fingerprints.go:168` (`candidateInputsChanged`) |
| `mergeExternalMCPServersFromDocuments` | 2 | 2 | `load.go:243` (`loadOnce`) |
| `migrationRecordPathFor` | 2 | 2 | `io/slotmigrate/ktav_migration_artifacts.go:449` (`ktavMigrateOAuthJournal`) |
| `normalizeDiscoveryPath` | 31 | 31 | `io/cfgcommit/config_write_identity.go:29` (`configWriteTargetDedupKeyWithCaseFold`) |
| `normalizeReloadPath` | 61 | 61 | `io/docread/fingerprints.go:171` (`candidateInputsChanged`) |
| `oauthJournalKtavPath` | 3 | 3 | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `oauthJournalLegacyPath` | 2 | 2 | `io/slotmigrate/ktav_migration_artifacts.go:405` (`ktavMigrateOAuthJournalsAtStartup`) |
| `oauthJournalRecord` | 23 | 0 | `io/slotmigrate/ktav_migration_artifacts.go:421` (`ktavValidateOAuthJournalBytes`) |
| `pathAlreadyLoaded` | 2 | 2 | `load.go:173` (`loadOnce`) |
| `readReloadFingerprint` | 5 | 5 | `store_config_disk_sync.go:304` (`configDiskSyncDigestsMatch`) |
| `readSettingsDocument` | 9 | 9 | `io/docread/mcpdocs.go:280` (`readMCPDisabledOverride`) |
| `readSlotOf` | 9 | 9 | `settings_lock.go:34` (`newUnrecognizedSettingsLock`) |
| `readStableConfigDocumentsWithOwner` | 2 | 2 | `io/docread/stabledocs.go:31` (`loadConfigCandidateStableForWorkingDir`) |
| `readStableConfigFile` | 1 | 1 | `settings_models_integrity.go:44` (`*ConfigStore.verifyModelsIntegrityAtLoad`) |
| `readStableConfigFileOwned` | 18 | 18 | `apitoken_store.go:396` (`*APITokenStore.read`) |
| `rebindReloadFingerprintPath` | 1 | 1 | `store_mcp_transaction.go:633` (`*ConfigStore.mcpPathData`) |
| `reloadFileFingerprint` | 57 | 0 | `io/docread/fingerprints.go:166` (`candidateInputsChanged`) |
| `reloadFingerprintsChangedWithOwner` | 1 | 1 | `io/docread/fingerprints.go:169` (`candidateInputsChanged`) |
| `reloadStalenessState` | 2 | 2 | `store.go:397` (`*ConfigStore.publishInitialSnapshot`) |
| `sameBytesFingerprint` | 1 | 1 | `store_mcp_transaction.go:463` (`*ConfigStore.reconcileMCPCommit`) |
| `sameReloadPathSet` | 3 | 3 | `io/docread/fingerprints.go:167` (`candidateInputsChanged`) |
| `selectConfigSlot` | 2 | 2 | `load.go:143` (`loadOnce`) |
| `settingsDocument` | 4 | 0 | `settings_lock.go:492` (`*ConfigStore.writeSettingsLockRaw`) |
| `settingsDocumentFormatOf` | 5 | 5 | `apitoken_store.go:523` (`*APITokenStore.mutate`) |
| `stableConfigAlias` | 4 | 0 | `load.go:191` (`loadOnce`) |
| `stableConfigDocument` | 16 | 0 | `io/docread/fingerprints.go:193` (`mcpInputFingerprints`) |
| `trackedSlotFingerprint` | 2 | 2 | `store_mcp_admission.go:684` (`*ConfigStore.mcpMutationResultCurrentLocked`) |
| `validOAuthJournal` | 3 | 3 | `io/slotmigrate/ktav_migration_artifacts.go:422` (`ktavValidateOAuthJournalBytes`) |
| `warnKtavArtifactConflictOnce` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:408` (`ktavMigrateOAuthJournalsAtStartup`) |
| `warnMigrationDeferredOnce` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:410` (`ktavMigrateOAuthJournalsAtStartup`) |
| `workspaceConfigPath` | 3 | 3 | `load.go:144` (`loadOnce`) |

#### `ROOT` → `fileid` — 53 references, 35 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigNonRegular` | 2 | 0 | `store_mcp_admission.go:393` (`validatePreparedAdmissionFile`) |
| `configFileIdentity` | 2 | 0 | `store_mcp_locks.go:35` (`mcpFileRecord`) |
| `configFileIdentityAtPath` | 2 | 2 | `apitoken_store.go:377` (`*APITokenStore.read`) |
| `configFileIdentityOf` | 1 | 1 | `apitoken_store.go:375` (`*APITokenStore.read`) |
| `configFileIdentityOfOpened` | 2 | 2 | `store_mcp_admission.go:540` (`validateFinalMCPAdmissionFile`) |
| `configFileNlinkOfOpened` | 3 | 3 | `store_mcp_admission.go:421` (`validatePreparedAdmissionFile`) |
| `configFileOwner` | 5 | 5 | `apitoken_store.go:368` (`*APITokenStore.read`) |
| `configFilePathIdentityMatches` | 2 | 2 | `store_mcp_admission.go:432` (`validatePreparedAdmissionFile`) |
| `configParentIdentity` | 4 | 4 | `io/cfgcommit/config_write_identity.go:26` (`configWriteTargetDedupKeyWithCaseFold`) |
| `errConfigCommitVerification` | 8 | 0 | `store_mcp_transaction.go:436` (`*ConfigStore.writeMCPFileChanges`) |
| `errConfigOwnerMismatch` | 3 | 0 | `apitoken_store.go:370` (`*APITokenStore.read`) |
| `errStableReadUnstable` | 3 | 0 | `load.go:99` (`Load`) |
| `homeConfigOwner` | 10 | 10 | `apitoken_store.go:369` (`*APITokenStore.read`) |
| `openStableConfigFile` | 1 | 1 | `store_mcp_admission.go:321` (`*ConfigStore.prepareMCPAdmissionToken`) |
| `readOpenedConfigBytes` | 2 | 2 | `store_mcp_admission.go:399` (`validatePreparedAdmissionFile`) |
| `systemConfigOwner` | 3 | 3 | `server_listeners.go:143` (`GlobalServerListenersWithSources`) |

#### `ROOT` → `fswap` — 8 references, 7 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configEquivalentLeafNames` | 1 | 1 | `store_mcp_transaction.go:812` (`configEquivalentMissingMCPInput`) |
| `configWriteIdentityKey` | 2 | 2 | `io/cfgcommit/config_write_identity.go:18` (`configWriteTargetDedupKeyWithCaseFold`) |
| `syncConfigParent` | 5 | 4 | `io/slots/oauthjournal.go:16` (`<package>`) |

#### `ROOT` → `slotmigrate` — 2 references, 2 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `migrateKtavArtifactToWithLock` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:454` (`ktavMigrateOAuthJournal`) |
| `runKtavMigrationStateLocked` | 1 | 1 | `store_write.go:284` (`*ConfigStore.withConfigWriteLockCtx`) |

#### `ROOT` → `slots` — 30 references, 30 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ActiveConfigFileName` | 1 | 1 | `store_mcp_transaction.go:607` (`*ConfigStore.orderedMCPPaths`) |
| `ReadConfigDocumentLight` | 1 | 1 | `store_models.go:51` (`*ConfigStore.ReadAllModelsAtScope`) |
| `ktavMigrateStartupSlots` | 2 | 2 | `load.go:116` (`loadOnce`) |
| `ktavMigrateWorkspaceIfNeeded` | 2 | 2 | `load.go:149` (`loadOnce`) |
| `newConfigFileIsKtav` | 1 | 1 | `apitoken_store.go:462` (`*APITokenStore.selectedPath`) |
| `readCacheDocument` | 5 | 5 | `model_catalog_cache.go:52` (`readModelCatalog`) |
| `selectOAuthJournalPath` | 7 | 7 | `store_oauth_journal_outbox.go:167` (`*ConfigStore.flushOAuthOutbox`) |
| `writeCacheDocument` | 3 | 3 | `model_catalog_cache.go:86` (`cachedProviderModels`) |
| `writeTargetSlot` | 8 | 8 | `provider_peak_hours_read.go:27` (`*ConfigStore.PeakHoursEntryForScope`) |

#### `casefold` → `fileid` — 10 references, 4 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configFileIdentity` | 6 | 0 | `io/casefold/config_case_sensitivity.go:77` (`configCaseDirectoryEntry`) |
| `configFileIdentityOf` | 2 | 2 | `io/casefold/config_case_sensitivity_behavioral_unix.go:20` (`configDirectoryCaseSensitivityByEntries`) |
| `openWindowsConfigHandle` | 1 | 1 | `io/casefold/config_case_sensitivity_windows.go:33` (`windowsConfigDirectoryCaseInsensitive`) |
| `windowsFileInformation` | 1 | 1 | `io/casefold/config_case_sensitivity_windows.go:39` (`windowsConfigDirectoryCaseInsensitive`) |

#### `cfgcommit` → `cfghooks` — 6 references, 6 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `runConfigAfterCommitRenameHookForPath` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:180` (`commitConfigFile`) |
| `runConfigBeforeCommitCheckHook` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:125` (`commitConfigFile`) |
| `runConfigBeforeCommitRenameHook` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:142` (`commitConfigFile`) |

#### `cfgcommit` → `docread` — 83 references, 25 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configDiscoveryFingerprint` | 4 | 4 | `io/cfgcommit/config_commit_unix.go:55` (`commitConfigFile`) |
| `normalizeReloadPath` | 1 | 1 | `io/cfgcommit/atomicwrite.go:321` (`verifySelectedCommitPath`) |
| `readStableConfigFileOwned` | 7 | 7 | `io/cfgcommit/config_commit_unix.go:25` (`commitConfigFile`) |
| `readStableConfigHandle` | 3 | 3 | `io/cfgcommit/config_commit_windows.go:107` (`commitConfigFile`) |
| `reloadFileFingerprint` | 58 | 0 | `io/cfgcommit/config_commit_unix.go:21` (`commitConfigFile`) |
| `sameBytesFingerprint` | 10 | 10 | `io/cfgcommit/config_commit_unix.go:35` (`commitConfigFile`) |

#### `cfgcommit` → `fileid` — 68 references, 23 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigHardLink` | 4 | 0 | `io/cfgcommit/config_commit_unix.go:70` (`commitConfigFile`) |
| `configFileIdentity` | 1 | 0 | `io/cfgcommit/config_commit_windows.go:265` (`classifyWindowsRenameFailure`) |
| `configFileIdentityOf` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:89` (`commitConfigFile`) |
| `configFileIdentityOfOpened` | 4 | 4 | `io/cfgcommit/config_commit_windows.go:181` (`verifyWindowsCommitParent`) |
| `configFileNlinkOfOpened` | 1 | 1 | `io/cfgcommit/config_commit_unix.go:259` (`verifyCommitEntry`) |
| `configFileOwner` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:50` (`commitConfigFile`) |
| `configParentIdentity` | 1 | 1 | `io/cfgcommit/atomicwrite.go:332` (`caseEquivalentConfigPaths`) |
| `deleteWindowsConfigHandle` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:71` (`commitConfigFile`) |
| `errConfigCommitVerification` | 39 | 0 | `io/cfgcommit/atomicwrite.go:323` (`verifySelectedCommitPath`) |
| `flushWindowsConfigHandle` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:106` (`commitConfigFile`) |
| `isWindowsEntryNotFound` | 3 | 3 | `io/cfgcommit/config_commit_windows.go:214` (`verifyWindowsCommitDestinationHandle`) |
| `openWindowsConfigEntryAt` | 3 | 3 | `io/cfgcommit/config_commit_windows.go:212` (`verifyWindowsCommitDestinationHandle`) |
| `openWindowsConfigParent` | 2 | 2 | `io/cfgcommit/config_commit_windows.go:36` (`commitConfigFile`) |
| `physicalWindowsConfigPath` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:31` (`commitConfigFile`) |
| `stageConfigFileHandleAt` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:61` (`commitConfigFile`) |
| `windowsFileInformation` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:230` (`verifyWindowsCommitDestinationHandle`) |
| `windowsStagedConfigFile` | 1 | 0 | `io/cfgcommit/config_commit_windows.go:253` (`classifyWindowsRenameFailure`) |

#### `cfgcommit` → `fswap` — 12 references, 12 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `cleanupConfigTempAlias` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:93` (`commitConfigFile`) |
| `configDirectoryIsCaseInsensitive` | 1 | 1 | `io/cfgcommit/atomicwrite.go:334` (`caseEquivalentConfigPaths`) |
| `configEquivalentLeafNames` | 1 | 1 | `io/cfgcommit/atomicwrite.go:329` (`caseEquivalentConfigPaths`) |
| `recoverConfigTempAlias` | 1 | 1 | `io/cfgcommit/config_commit_unix.go:61` (`commitConfigFile`) |
| `renameConfigTemp` | 1 | 1 | `io/cfgcommit/atomicwrite.go:352` (`atomicWriteFile`) |
| `renameConfigTempAt` | 1 | 1 | `io/cfgcommit/config_commit_unix.go:148` (`commitConfigFile`) |
| `renameConfigTempHandle` | 1 | 1 | `io/cfgcommit/config_commit_windows.go:96` (`commitConfigFile`) |
| `syncConfigParent` | 1 | 1 | `io/cfgcommit/atomicwrite.go:356` (`atomicWriteFile`) |
| `syncConfigParentFile` | 2 | 2 | `io/cfgcommit/config_commit_unix.go:183` (`commitConfigFile`) |
| `unlinkConfigTempAt` | 1 | 1 | `io/cfgcommit/config_commit_unix.go:86` (`commitConfigFile`) |

#### `docread` → `casefold` — 1 references, 1 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configPlatformCaseFoldLeaf` | 1 | 1 | `store_reload.go:34` (`configFingerprintPath`) |

#### `docread` → `cfghooks` — 2 references, 2 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `runConfigAfterStableReadHook` | 1 | 1 | `io/docread/stabledocs.go:158` (`readStableConfigFileOwned`) |
| `runConfigBeforeOpenHook` | 1 | 1 | `io/docread/stabledocs.go:121` (`readStableConfigFileOwned`) |

#### `docread` → `cfgmodel` — 23 references, 1 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `Config` | 1 | 0 | `io/docread/mcpdocs.go:217` (`mergeExternalMCPServersFromDocuments`) |
| `MCPConfig` | 12 | 0 | `io/docread/mcpdocs.go:35` (`loadMCPJSON`) |
| `MCPHttp` | 2 | 0 | `io/docread/mcpdocs.go:56` (`loadMCPJSONBytes`) |
| `MCPSSE` | 2 | 0 | `io/docread/mcpdocs.go:54` (`loadMCPJSONBytes`) |
| `MCPSourceExternal` | 1 | 0 | `io/docread/mcpdocs.go:67` (`loadMCPJSONBytes`) |
| `MCPStdio` | 1 | 0 | `io/docread/mcpdocs.go:52` (`loadMCPJSONBytes`) |
| `MCPType` | 1 | 1 | `io/docread/mcpdocs.go:53` (`loadMCPJSONBytes`) |
| `MCPs` | 1 | 0 | `io/docread/mcpdocs.go:219` (`mergeExternalMCPServersFromDocuments`) |
| `appName` | 2 | 0 | `io/slots/ktav_discovery.go:54` (`configSlotBaseNames`) |

#### `docread` → `datadir` — 26 references, 26 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `GlobalConfig` | 4 | 4 | `io/docread/candidates.go:118` (`configOwnerPolicyForPath`) |
| `GlobalConfigData` | 4 | 4 | `io/docread/candidates.go:118` (`configOwnerPolicyForPath`) |
| `canonicalConfigPath` | 8 | 8 | `io/docread/candidates.go:60` (`eligibleWorkspaceConfig`) |
| `projectBoundary` | 2 | 2 | `io/slots/ktav_discovery.go:257` (`ktavProjectLegacySlots`) |
| `sameDir` | 2 | 2 | `io/slots/ktav_discovery.go:271` (`ktavProjectLegacySlots`) |
| `systemConfigSlot` | 6 | 6 | `io/docread/candidates.go:115` (`configOwnerPolicyForPath`) |

#### `docread` → `fileid` — 42 references, 30 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigNonRegular` | 1 | 0 | `io/docread/stabledocs.go:141` (`readStableConfigFileOwned`) |
| `configFileIdentity` | 3 | 0 | `io/docread/fingerprints.go:35` (`reloadFileFingerprint`) |
| `configFileIdentityOf` | 2 | 2 | `io/cfgcommit/config_linkat_unix.go:186` (`readConfigEntryAt`) |
| `configFileIdentityOfOpened` | 2 | 2 | `io/docread/stabledocs.go:181` (`readStableConfigFileOwned`) |
| `configFileNlinkOfInfo` | 1 | 1 | `io/cfgcommit/config_linkat_unix.go:189` (`readConfigEntryAt`) |
| `configFileNlinkOfOpened` | 2 | 2 | `io/docread/stabledocs.go:182` (`readStableConfigFileOwned`) |
| `configFileOwner` | 3 | 3 | `io/cfgcommit/config_linkat_unix.go:164` (`readConfigEntryAt`) |
| `configFilePathIdentityMatches` | 1 | 1 | `io/docread/stabledocs.go:171` (`readStableConfigFileOwned`) |
| `configParentIdentity` | 3 | 3 | `io/docread/stabledocs.go:120` (`readStableConfigFileOwned`) |
| `errConfigCommitVerification` | 1 | 0 | `io/cfgcommit/config_linkat_unix.go:162` (`readConfigEntryAt`) |
| `errConfigOwnerMismatch` | 3 | 0 | `io/cfgcommit/config_linkat_unix.go:166` (`readConfigEntryAt`) |
| `errStableReadUnstable` | 4 | 0 | `io/cfgcommit/config_linkat_unix.go:177` (`readConfigEntryAt`) |
| `homeConfigOwner` | 5 | 5 | `io/docread/candidates.go:119` (`configOwnerPolicyForPath`) |
| `openStableConfigFile` | 1 | 1 | `io/docread/stabledocs.go:122` (`readStableConfigFileOwned`) |
| `readOpenedConfigBytes` | 6 | 6 | `io/cfgcommit/config_linkat_unix.go:168` (`readConfigEntryAt`) |
| `sameConfigFileIdentity` | 1 | 1 | `io/cfgcommit/config_linkat_unix.go:183` (`readConfigEntryAt`) |
| `systemConfigOwner` | 2 | 2 | `io/docread/candidates.go:116` (`configOwnerPolicyForPath`) |
| `windowsFileInformation` | 1 | 1 | `io/fileid/config_file_identity_windows.go:373` (`readStableConfigHandle`) |

#### `fileid` → `cfghooks` — 6 references, 0 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configTestHooks` | 6 | 0 | `io/fileid/config_file_identity_windows.go:338` (`deleteWindowsConfigHandle`) |

#### `fswap` → `casefold` — 3 references, 3 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configPlatformCaseFoldLeaf` | 3 | 3 | `io/cfgcommit/config_write_identity.go:53` (`configDirectoryIsCaseInsensitive`) |

#### `fswap` → `cfghooks` — 35 references, 0 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configTestHooks` | 35 | 0 | `io/cfgcommit/atomicwrite.go:242` (`syncConfigParent`) |

#### `fswap` → `docread` — 22 references, 7 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `configDiscoveryFingerprint` | 2 | 2 | `io/fswap/config_rename_windows.go:162` (`verifyWindowsRenameRetryState`) |
| `readConfigEntryAt` | 2 | 2 | `io/cfgcommit/config_linkat_unix.go:89` (`recoverConfigTempAlias`) |
| `readStableConfigHandle` | 1 | 1 | `io/fswap/config_rename_windows.go:200` (`verifyWindowsRenameRetryState`) |
| `reloadFileFingerprint` | 15 | 0 | `io/cfgcommit/config_linkat_unix.go:87` (`recoverConfigTempAlias`) |
| `sameBytesFingerprint` | 2 | 2 | `io/cfgcommit/config_linkat_unix.go:95` (`recoverConfigTempAlias`) |

#### `fswap` → `fileid` — 53 references, 17 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigHardLink` | 9 | 0 | `io/cfgcommit/config_linkat_unix.go:25` (`publishConfigTempNoReplaceAt`) |
| `configFileIdentity` | 8 | 0 | `io/cfgcommit/config_linkat_unix.go:52` (`configTempIdentityAt`) |
| `configFileIdentityOf` | 3 | 3 | `io/cfgcommit/config_linkat_unix.go:66` (`configTempIdentityAt`) |
| `configFileIdentityOfOpened` | 3 | 3 | `io/fswap/config_rename_windows.go:155` (`verifyWindowsRenameRetryState`) |
| `configFileNlinkOfInfo` | 1 | 1 | `io/cfgcommit/config_linkat_unix.go:121` (`recoverConfigTempAlias`) |
| `configFileNlinkOfOpened` | 1 | 1 | `io/cfgcommit/config_linkat_unix.go:70` (`configTempIdentityAt`) |
| `configFileOwner` | 1 | 1 | `io/cfgcommit/config_linkat_unix.go:230` (`configTempAliasPolicyAcceptable`) |
| `errConfigCommitVerification` | 19 | 0 | `io/cfgcommit/config_linkat_unix.go:38` (`publishConfigTempNoReplaceAt`) |
| `isWindowsEntryNotFound` | 1 | 1 | `io/fswap/config_rename_windows.go:184` (`verifyWindowsRenameRetryState`) |
| `openWindowsConfigEntryAt` | 2 | 2 | `io/fswap/config_rename_windows.go:165` (`verifyWindowsRenameRetryState`) |
| `openWindowsConfigHandle` | 1 | 1 | `io/cfgcommit/config_durability_windows.go:15` (`syncConfigParentOnDisk`) |
| `windowsConfigAPIPath` | 2 | 2 | `io/fswap/config_rename_windows.go:229` (`renameConfigTemp`) |
| `windowsFileInformation` | 2 | 2 | `io/fswap/config_rename_windows.go:174` (`verifyWindowsRenameRetryState`) |

#### `slotmigrate` → `cfgcommit` — 20 references, 12 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `commitConfigFile` | 7 | 7 | `io/slotmigrate/ktav_migration.go:318` (`runKtavMigrationTransaction`) |
| `errAtomicWriteCommitted` | 5 | 0 | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |
| `errConfigCommitDurabilityUncertain` | 3 | 0 | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |
| `newCommitOutcome` | 5 | 5 | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |

#### `slotmigrate` → `cfghooks` — 13 references, 8 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `acquireConfigFileLock` | 2 | 2 | `io/slotmigrate/ktav_migration.go:118` (`runKtavMigrationState`) |
| `ktavArtifactChtimes` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:380` (`ktavArtifactPreserveMTime`) |
| `ktavArtifactMigrationStepHook` | 2 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:41` (`runKtavArtifactMigrationStep`) |
| `ktavArtifactStatFile` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:375` (`ktavArtifactPreserveMTime`) |
| `ktavMigrationBudget` | 3 | 0 | `io/slotmigrate/ktav_migration.go:102` (`migrateLegacyConfigFile`) |
| `ktavMigrationStepHook` | 2 | 1 | `io/slotmigrate/ktav_migration.go:37` (`runKtavMigrationStep`) |
| `releaseConfigFileLock` | 2 | 2 | `io/slotmigrate/ktav_migration.go:122` (`runKtavMigrationState`) |

#### `slotmigrate` → `cfgmodel` — 6 references, 4 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `cacheRevKey` | 2 | 0 | `io/slotmigrate/ktav_migration_cache_rev.go:41` (`applyKtavMigrationModelsIntegrity`) |
| `canonicalModelsAndRolesJSON` | 2 | 2 | `io/slotmigrate/ktav_migration_cache_rev.go:35` (`applyKtavMigrationModelsIntegrity`) |
| `modelsIntegritySignature` | 2 | 2 | `io/slotmigrate/ktav_migration_cache_rev.go:46` (`applyKtavMigrationModelsIntegrity`) |

#### `slotmigrate` → `datadir` — 3 references, 3 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `GlobalConfig` | 1 | 1 | `io/slotmigrate/ktav_migration.go:479` (`ownerPolicyForMigration`) |
| `GlobalConfigData` | 1 | 1 | `io/slotmigrate/ktav_migration.go:479` (`ownerPolicyForMigration`) |
| `systemConfigSlot` | 1 | 1 | `io/slotmigrate/ktav_migration.go:479` (`ownerPolicyForMigration`) |

#### `slotmigrate` → `docread` — 130 references, 78 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `backupPathFor` | 9 | 9 | `io/slotmigrate/ktav_migration.go:88` (`inspectKtavMigrationEntries`) |
| `configAliasChainFingerprint` | 3 | 3 | `io/slotmigrate/ktav_migration.go:310` (`runKtavMigrationTransaction`) |
| `configDiscoveryFingerprint` | 6 | 6 | `io/slotmigrate/ktav_migration.go:307` (`runKtavMigrationTransaction`) |
| `configOwnerPolicyForPath` | 2 | 2 | `io/slotmigrate/ktav_migration.go:480` (`ownerPolicyForMigration`) |
| `errKtavBackupOnly` | 2 | 0 | `io/slotmigrate/ktav_migration.go:148` (`runKtavMigrationStateLocked`) |
| `errKtavMigrationConflict` | 30 | 0 | `io/slotmigrate/ktav_migration.go:181` (`runKtavMigrationStateLocked`) |
| `errKtavMigrationRecord` | 15 | 0 | `io/slotmigrate/ktav_migration.go:349` (`finishKtavArchival`) |
| `ktavEntryExists` | 8 | 8 | `io/slotmigrate/ktav_migration.go:86` (`inspectKtavMigrationEntries`) |
| `ktavSiblingOf` | 10 | 10 | `io/slotmigrate/ktav_migration.go:87` (`inspectKtavMigrationEntries`) |
| `matchesLogicalSlot` | 1 | 1 | `io/slotmigrate/ktav_migration.go:479` (`ownerPolicyForMigration`) |
| `migrationRecordPathFor` | 4 | 4 | `io/slotmigrate/ktav_migration.go:89` (`inspectKtavMigrationEntries`) |
| `normalizeReloadPath` | 1 | 1 | `io/slotmigrate/ktav_migration.go:478` (`ownerPolicyForMigration`) |
| `readConfigEntryAt` | 2 | 2 | `io/slotmigrate/ktav_migration_unix.go:53` (`archiveKtavBackup`) |
| `readStableConfigFileOwned` | 15 | 15 | `io/slotmigrate/ktav_migration.go:229` (`runKtavMigrationTransaction`) |
| `readStableConfigHandle` | 2 | 2 | `io/slotmigrate/ktav_migration_windows.go:57` (`archiveKtavBackup`) |
| `reloadFileFingerprint` | 5 | 0 | `io/slotmigrate/ktav_migration.go:306` (`runKtavMigrationTransaction`) |
| `sameBytesFingerprint` | 11 | 11 | `io/slotmigrate/ktav_migration.go:365` (`finishKtavArchival`) |
| `warnMigrationDeferredOnce` | 4 | 4 | `io/slotmigrate/ktav_migration_artifacts.go:154` (`runKtavArtifactMigrationStateLocked`) |

#### `slotmigrate` → `fileid` — 23 references, 13 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ErrConfigHardLink` | 3 | 0 | `io/slotmigrate/ktav_migration.go:234` (`runKtavMigrationTransaction`) |
| `configFileIdentityOf` | 1 | 1 | `io/slotmigrate/ktav_migration_unix.go:37` (`archiveKtavBackup`) |
| `configFileIdentityOfOpened` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:37` (`archiveKtavBackup`) |
| `configFileOwner` | 2 | 2 | `io/slotmigrate/ktav_migration_unix.go:33` (`archiveKtavBackup`) |
| `configParentIdentity` | 5 | 5 | `io/slotmigrate/ktav_migration.go:309` (`runKtavMigrationTransaction`) |
| `createWindowsConfigEntryAt` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:51` (`archiveKtavBackup`) |
| `errConfigCommitVerification` | 7 | 0 | `io/slotmigrate/ktav_migration_unix.go:35` (`archiveKtavBackup`) |
| `isWindowsEntryNotFound` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:46` (`archiveKtavBackup`) |
| `openWindowsConfigEntryAt` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:41` (`archiveKtavBackup`) |
| `openWindowsConfigParent` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:23` (`archiveKtavBackup`) |

#### `slotmigrate` → `fswap` — 7 references, 7 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `renameConfigTempAt` | 1 | 1 | `io/slotmigrate/ktav_migration_unix.go:66` (`archiveKtavBackup`) |
| `renameConfigTempHandle` | 1 | 1 | `io/slotmigrate/ktav_migration_windows.go:71` (`archiveKtavBackup`) |
| `syncConfigParent` | 3 | 3 | `io/slotmigrate/ktav_migration.go:164` (`runKtavMigrationStateLocked`) |
| `syncConfigParentFile` | 2 | 2 | `io/slotmigrate/ktav_migration_unix.go:87` (`archiveKtavBackup`) |

#### `slots` → `cfgcommit` — 1 references, 1 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `atomicWriteFile` | 1 | 1 | `io/slots/ktav_cache.go:65` (`writeCacheDocument`) |

#### `slots` → `cfghooks` — 9 references, 0 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `ktavAutoMigrate` | 8 | 0 | `io/slotmigrate/ktav_migration.go:503` (`ktavMigrateStartupSlots`) |
| `ktavMigrationBudget` | 1 | 0 | `io/slotmigrate/ktav_migration.go:495` (`ktavMigrateStartupSlots`) |

#### `slots` → `cfgmodel` — 1 references, 0 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `appName` | 1 | 0 | `io/slotmigrate/ktav_migration.go:539` (`ktavMigrateWorkspaceIfNeeded`) |

#### `slots` → `datadir` — 2 references, 2 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `cachePathFor` | 2 | 2 | `io/slotmigrate/ktav_migration_artifacts.go:531` (`ktavCacheArtifactPaths`) |

#### `slots` → `docread` — 53 references, 44 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `checkBackupOnlySlot` | 1 | 1 | `io/slots/ktav_light.go:21` (`ReadConfigDocumentLight`) |
| `decodeConfigDocument` | 1 | 1 | `io/slots/ktav_light.go:30` (`ReadConfigDocumentLight`) |
| `errKtavBackupOnly` | 2 | 0 | `io/slotmigrate/ktav_migration.go:517` (`ktavMigrateStartupSlots`) |
| `errKtavMigrationConflict` | 3 | 0 | `io/slotmigrate/ktav_migration.go:517` (`ktavMigrateStartupSlots`) |
| `errKtavMigrationRecord` | 4 | 0 | `io/slotmigrate/ktav_migration.go:500` (`ktavMigrateStartupSlots`) |
| `fileExists` | 3 | 3 | `io/slotmigrate/ktav_migration.go:510` (`ktavMigrateStartupSlots`) |
| `isKtavBackupPath` | 1 | 1 | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `isKtavConfigPath` | 3 | 3 | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `isKtavMigrationRecordPath` | 1 | 1 | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `ktavEntryAbsentErr` | 1 | 1 | `io/slots/ktav_cache.go:27` (`readCacheDocument`) |
| `ktavEntryExists` | 7 | 7 | `io/slotmigrate/ktav_migration.go:520` (`ktavMigrateStartupSlots`) |
| `ktavMigrationPendingSlots` | 1 | 1 | `io/slotmigrate/ktav_migration.go:498` (`ktavMigrateStartupSlots`) |
| `ktavSiblingOf` | 9 | 9 | `io/slotmigrate/ktav_migration.go:510` (`ktavMigrateStartupSlots`) |
| `legacySlotOf` | 2 | 2 | `io/slots/ktav_light.go:20` (`ReadConfigDocumentLight`) |
| `lookupConfigCandidates` | 1 | 1 | `io/slotmigrate/ktav_migration.go:506` (`ktavMigrateStartupSlots`) |
| `migrationRecordPathFor` | 3 | 3 | `io/slotmigrate/ktav_migration.go:520` (`ktavMigrateStartupSlots`) |
| `oauthJournalKtavPath` | 1 | 1 | `io/slots/ktav_oauth.go:16` (`selectOAuthJournalPath`) |
| `oauthJournalLegacyPath` | 1 | 1 | `io/slots/ktav_oauth.go:15` (`selectOAuthJournalPath`) |
| `selectConfigSlot` | 3 | 3 | `io/slots/ktav_doc.go:194` (`writeTargetSlot`) |
| `warnBothFormatsOnce` | 1 | 1 | `io/slots/ktav_cache.go:41` (`readCacheDocument`) |
| `warnKtavArtifactConflictOnce` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:517` (`ktavMigrateCacheArtifactsStartup`) |
| `warnMigrationDeferredOnce` | 3 | 3 | `io/slotmigrate/ktav_migration.go:523` (`ktavMigrateStartupSlots`) |

#### `slots` → `slotmigrate` — 6 references, 6 call locations

| Referenced identifier | Ref count | Call count | First evidence |
|---|---:|---:|---|
| `inspectKtavMigrationEntries` | 1 | 1 | `io/slotmigrate/ktav_migration.go:550` (`ktavMigrateWorkspaceIfNeeded`) |
| `migrateKtavArtifact` | 1 | 1 | `io/slotmigrate/ktav_migration_artifacts.go:515` (`ktavMigrateCacheArtifactsStartup`) |
| `runKtavMigrationState` | 4 | 4 | `io/slotmigrate/ktav_migration.go:499` (`ktavMigrateStartupSlots`) |

### C. Required lower-owner API/export table

This is the capitalization/accessor plan for every referenced identifier crossing into an IO owner in the projection. It is a proposed API, not an assertion that the symbols are exported today. Ordinary names capitalize the first rune; explicit domain names and error accessors are mapped below. `configTestHooks` and migration hook variables are provider-snapshot transformations, not exports. Root-kept composites do not become lower exports. Error identity is preserved through one getter/provider alias rather than a second package-global error value.

| Target owner | Referenced identifier | Proposed API / treatment | First external evidence |
|---|---|---|---|
| `casefold` | `configCaseSensitivity` | `ConfigCaseSensitivity` | `io/cfgcommit/config_write_identity.go:42` (`configWriteTargetDedupKeyWithCaseSensitivity`) |
| `casefold` | `configCaseSensitivityUnknown` | `ConfigCaseSensitivityUnknown` | `store_mcp_locks.go:207` (`configMCPAdmissionLockGroups`) |
| `casefold` | `configFoldCaseLeaf` | `ConfigFoldCaseLeaf` | `io/cfgcommit/config_write_identity.go:44` (`configWriteTargetDedupKeyWithCaseSensitivity`) |
| `casefold` | `configPlatformCaseFoldLeaf` | `ConfigPlatformCaseFoldLeaf` | `io/cfgcommit/config_write_identity.go:13` (`configWriteTargetDedupKey`) |
| `casefold` | `configPlatformCaseFoldLeaf` | `ConfigPlatformCaseFoldLeaf` | `io/cfgcommit/config_write_identity.go:53` (`configDirectoryIsCaseInsensitive`) |
| `casefold` | `configPlatformCaseFoldLeaf` | `ConfigPlatformCaseFoldLeaf` | `store_reload.go:34` (`configFingerprintPath`) |
| `casefold` | `configPlatformCaseSensitivity` | `ConfigPlatformCaseSensitivity` | `store_mcp_locks.go:189` (`*ConfigStore.withMCPLocks`) |
| `cfgcommit` | `CommitOutcomeFromError` | `CommitOutcomeFromError` | `settings_lock.go:525` (`*ConfigStore.writeSettingsLockRaw`) |
| `cfgcommit` | `atomicWriteFile` | `AtomicWriteFile` | `api_remotes.go:321` (`WriteAPIRemoteTokenFile`) |
| `cfgcommit` | `atomicWriteFile` | `AtomicWriteFile` | `io/slots/ktav_cache.go:65` (`writeCacheDocument`) |
| `cfgcommit` | `cloneCommitOutcome` | `CloneCommitOutcome` | `store_mcp_transaction.go:399` (`*ConfigStore.writeMCPFileChanges`) |
| `cfgcommit` | `commitConfigFile` | `CommitConfigFile` | `io/slotmigrate/ktav_migration.go:318` (`runKtavMigrationTransaction`) |
| `cfgcommit` | `commitConfigFile` | `CommitConfigFile` | `settings_lock.go:523` (`*ConfigStore.writeSettingsLockRaw`) |
| `cfgcommit` | `errAtomicWriteCommitted` | `ErrAtomicWriteCommitted()` | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |
| `cfgcommit` | `errConfigCommitCommitted` | `ErrConfigCommitCommitted()` | `store_mcp_transaction.go:409` (`*ConfigStore.writeMCPFileChanges`) |
| `cfgcommit` | `errConfigCommitDurabilityUncertain` | `ErrConfigCommitDurabilityUncertain()` | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |
| `cfgcommit` | `errConfigCommitDurabilityUncertain` | `ErrConfigCommitDurabilityUncertain()` | `store_mcp_transaction.go:386` (`*ConfigStore.writeMCPFileChanges`) |
| `cfgcommit` | `errConfigCommitUncertain` | `ErrConfigCommitUncertain()` | `store_mcp_transaction.go:409` (`*ConfigStore.writeMCPFileChanges`) |
| `cfgcommit` | `newCommitOutcome` | `NewCommitOutcome` | `io/slotmigrate/ktav_migration_unix.go:71` (`archiveKtavBackup`) |
| `cfgcommit` | `newCommitOutcome` | `NewCommitOutcome` | `store_mcp_transaction.go:447` (`*ConfigStore.writeMCPFileChanges`) |
| `cfghooks` | `acquireConfigFileLock` | `AcquireConfigFileLock` | `io/slotmigrate/ktav_migration.go:118` (`runKtavMigrationState`) |
| `cfghooks` | `acquireConfigFileLock` | `AcquireConfigFileLock` | `store_mcp_locks.go:302` (`*ConfigStore.withMCPLocksUsingCaseSensitivity`) |
| `cfghooks` | `configContextWithTimeout` | `ConfigContextWithTimeout` | `store_mcp_admission.go:229` (`*ConfigStore.WithCurrentMCPAdmission`) |
| `cfghooks` | `configTestHooks` | `cfghooks.Snapshot` provider value; do not export `configTestHooks` | `io/cfgcommit/atomicwrite.go:242` (`syncConfigParent`) |
| `cfghooks` | `configTestHooks` | `cfghooks.Snapshot` provider value; do not export `configTestHooks` | `io/fileid/config_file_identity_windows.go:338` (`deleteWindowsConfigHandle`) |
| `cfghooks` | `ktavArtifactChtimes` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration_artifacts.go:380` (`ktavArtifactPreserveMTime`) |
| `cfghooks` | `ktavArtifactMigrationStepHook` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration_artifacts.go:41` (`runKtavArtifactMigrationStep`) |
| `cfghooks` | `ktavArtifactStatFile` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration_artifacts.go:375` (`ktavArtifactPreserveMTime`) |
| `cfghooks` | `ktavAutoMigrate` | ROOT-owned; keep the composite/state in ROOT and use the narrow existing/approved accessor where needed | `io/slotmigrate/ktav_migration.go:503` (`ktavMigrateStartupSlots`) |
| `cfghooks` | `ktavMigrationBudget` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration.go:102` (`migrateLegacyConfigFile`) |
| `cfghooks` | `ktavMigrationBudget` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration.go:495` (`ktavMigrateStartupSlots`) |
| `cfghooks` | `ktavMigrationStepHook` | migration provider snapshot getter; do not export mutable variable | `io/slotmigrate/ktav_migration.go:37` (`runKtavMigrationStep`) |
| `cfghooks` | `releaseConfigFileLock` | `ReleaseConfigFileLock` | `io/slotmigrate/ktav_migration.go:122` (`runKtavMigrationState`) |
| `cfghooks` | `releaseConfigFileLock` | `ReleaseConfigFileLock` | `store_oauth_disk_sync.go:185` (`*ConfigStore.drainOAuthOutboxOnStop`) |
| `cfghooks` | `runConfigAfterCommitRenameHookForPath` | `RunConfigAfterCommitRenameHookForPath` | `io/cfgcommit/config_commit_unix.go:180` (`commitConfigFile`) |
| `cfghooks` | `runConfigAfterMCPCommitHook` | `RunConfigAfterMCPCommitHook` | `store_mcp_transaction.go:444` (`*ConfigStore.writeMCPFileChanges`) |
| `cfghooks` | `runConfigAfterStableReadHook` | `RunConfigAfterStableReadHook` | `io/docread/stabledocs.go:158` (`readStableConfigFileOwned`) |
| `cfghooks` | `runConfigBeforeCommitCheckHook` | `RunConfigBeforeCommitCheckHook` | `io/cfgcommit/config_commit_unix.go:125` (`commitConfigFile`) |
| `cfghooks` | `runConfigBeforeCommitRenameHook` | `RunConfigBeforeCommitRenameHook` | `io/cfgcommit/config_commit_unix.go:142` (`commitConfigFile`) |
| `cfghooks` | `runConfigBeforeMCPReconcileHook` | `RunConfigBeforeMCPReconcileHook` | `store_mcp_transaction.go:461` (`*ConfigStore.reconcileMCPCommit`) |
| `cfghooks` | `runConfigBeforeOpenHook` | `RunConfigBeforeOpenHook` | `io/docread/stabledocs.go:121` (`readStableConfigFileOwned`) |
| `docread` | `ErrConfigReloadUnstable` | `ErrConfigReloadUnstable` | `load.go:107` (`Load`) |
| `docread` | `addDocumentAliasFingerprints` | `AddDocumentAliasFingerprints` | `io/docread/stabledocs.go:48` (`loadConfigCandidateStableForWorkingDir`) |
| `docread` | `backupPathFor` | `BackupPathFor` | `io/slotmigrate/ktav_migration.go:88` (`inspectKtavMigrationEntries`) |
| `docread` | `backupPathFor` | `BackupPathFor` | `io/slotmigrate/ktav_migration_artifacts.go:457` (`ktavMigrateOAuthJournal`) |
| `docread` | `checkAuthoritativeBackupOnly` | `CheckAuthoritativeBackupOnly` | `load.go:111` (`loadOnce`) |
| `docread` | `checkBackupOnlySlot` | `CheckBackupOnlySlot` | `io/slots/ktav_light.go:21` (`ReadConfigDocumentLight`) |
| `docread` | `checkBackupOnlySlot` | `CheckBackupOnlySlot` | `load.go:146` (`loadOnce`) |
| `docread` | `configAliasChainFingerprint` | `ConfigAliasChainFingerprint` | `io/slotmigrate/ktav_migration.go:310` (`runKtavMigrationTransaction`) |
| `docread` | `configAliasChainFingerprint` | `ConfigAliasChainFingerprint` | `store_mcp_admission.go:377` (`mcpAdmissionAbsenceFingerprint`) |
| `docread` | `configAndMCPStalenessPaths` | `ConfigAndMCPStalenessPaths` | `store.go:395` (`*ConfigStore.publishInitialSnapshot`) |
| `docread` | `configDiscoveryFingerprint` | `ConfigDiscoveryFingerprint` | `io/cfgcommit/config_commit_unix.go:55` (`commitConfigFile`) |
| `docread` | `configDiscoveryFingerprint` | `ConfigDiscoveryFingerprint` | `io/fswap/config_rename_windows.go:162` (`verifyWindowsRenameRetryState`) |
| `docread` | `configDiscoveryFingerprint` | `ConfigDiscoveryFingerprint` | `io/slotmigrate/ktav_migration.go:307` (`runKtavMigrationTransaction`) |
| `docread` | `configDiscoveryFingerprint` | `ConfigDiscoveryFingerprint` | `store_mcp_admission.go:374` (`mcpAdmissionAbsenceFingerprint`) |
| `docread` | `configDocumentBytes` | `ConfigDocumentBytes` | `io/docread/stabledocs.go:47` (`loadConfigCandidateStableForWorkingDir`) |
| `docread` | `configOwnerForWorkingDir` | `ConfigOwnerForWorkingDir` | `load.go:174` (`loadOnce`) |
| `docread` | `configOwnerPolicyForPath` | `ConfigOwnerPolicyForPath` | `io/docread/fingerprints.go:174` (`candidateInputsChanged`) |
| `docread` | `configOwnerPolicyForPath` | `ConfigOwnerPolicyForPath` | `io/slotmigrate/ktav_migration.go:480` (`ownerPolicyForMigration`) |
| `docread` | `configWriteLockAnchor` | `ConfigWriteLockAnchor` | `apitoken_store.go:488` (`*APITokenStore.mutate`) |
| `docread` | `decodeConfigDocument` | `DecodeConfigDocument` | `io/slots/ktav_light.go:30` (`ReadConfigDocumentLight`) |
| `docread` | `decodeConfigDocument` | `DecodeConfigDocument` | `load.go:181` (`loadOnce`) |
| `docread` | `decodeOAuthJournalDiskToken` | `DecodeOAuthJournalDiskToken` | `store_oauth_disk_sync.go:105` (`*ConfigStore.syncOAuthFromDisk`) |
| `docread` | `decodeSettingsDocument` | `DecodeSettingsDocument` | `apitoken_store.go:405` (`*APITokenStore.read`) |
| `docread` | `eligibleConfigCandidate` | `EligibleConfigCandidate` | `store_mcp_transaction.go:595` (`*ConfigStore.orderedMCPPaths`) |
| `docread` | `encodeSettingsDocument` | `EncodeSettingsDocument` | `apitoken_store.go:524` (`*APITokenStore.mutate`) |
| `docread` | `errKtavBackupOnly` | `ErrKtavBackupOnly()` | `io/slotmigrate/ktav_migration.go:148` (`runKtavMigrationStateLocked`) |
| `docread` | `errKtavBackupOnly` | `ErrKtavBackupOnly()` | `io/slotmigrate/ktav_migration.go:517` (`ktavMigrateStartupSlots`) |
| `docread` | `errKtavMigrationConflict` | `ErrKtavMigrationConflict()` | `io/slotmigrate/ktav_migration.go:181` (`runKtavMigrationStateLocked`) |
| `docread` | `errKtavMigrationConflict` | `ErrKtavMigrationConflict()` | `io/slotmigrate/ktav_migration.go:517` (`ktavMigrateStartupSlots`) |
| `docread` | `errKtavMigrationConflict` | `ErrKtavMigrationConflict()` | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `errKtavMigrationRecord` | `ErrKtavMigrationRecord()` | `io/slotmigrate/ktav_migration.go:349` (`finishKtavArchival`) |
| `docread` | `errKtavMigrationRecord` | `ErrKtavMigrationRecord()` | `io/slotmigrate/ktav_migration.go:500` (`ktavMigrateStartupSlots`) |
| `docread` | `errOAuthJournalDisk` | `ErrOAuthJournalDisk()` | `store_oauth_journal.go:158` (`writeOAuthJournalOnce`) |
| `docread` | `errOAuthJournalInvalid` | `ErrOAuthJournalInvalid()` | `io/slotmigrate/ktav_migration_artifacts.go:423` (`ktavValidateOAuthJournalBytes`) |
| `docread` | `expandConfigSlotSiblings` | `ExpandConfigSlotSiblings` | `store_config_disk_sync.go:296` (`configDiskSyncCandidatePaths`) |
| `docread` | `fileExists` | `FileExists` | `io/slotmigrate/ktav_migration.go:510` (`ktavMigrateStartupSlots`) |
| `docread` | `fileExists` | `FileExists` | `load.go:152` (`loadOnce`) |
| `docread` | `fileSnapshot` | `FileSnapshot` | `store.go:46` (`storeSnapshot`) |
| `docread` | `isKtavBackupPath` | `IsKtavBackupPath` | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `docread` | `isKtavConfigPath` | `IsKtavConfigPath` | `io/docread/fingerprints.go:224` (`mcpInputFingerprints`) |
| `docread` | `isKtavConfigPath` | `IsKtavConfigPath` | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `docread` | `isKtavMigrationRecordPath` | `IsKtavMigrationRecordPath` | `io/slotmigrate/ktav_migration.go:507` (`ktavMigrateStartupSlots`) |
| `docread` | `isMCPDisabledOnlyEntry` | `IsMCPDisabledOnlyEntry` | `io/docread/mcpdocs.go:285` (`readMCPDisabledOverride`) |
| `docread` | `ktavEntryAbsentErr` | `KtavEntryAbsentErr` | `io/slots/ktav_cache.go:27` (`readCacheDocument`) |
| `docread` | `ktavEntryExists` | `KtavEntryExists` | `io/slotmigrate/ktav_migration.go:86` (`inspectKtavMigrationEntries`) |
| `docread` | `ktavEntryExists` | `KtavEntryExists` | `io/slotmigrate/ktav_migration.go:520` (`ktavMigrateStartupSlots`) |
| `docread` | `ktavEntryExists` | `KtavEntryExists` | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `ktavMigrationPendingSlots` | `KtavMigrationPendingSlots` | `io/slotmigrate/ktav_migration.go:498` (`ktavMigrateStartupSlots`) |
| `docread` | `ktavSiblingOf` | `KtavSiblingOf` | `io/slotmigrate/ktav_migration.go:87` (`inspectKtavMigrationEntries`) |
| `docread` | `ktavSiblingOf` | `KtavSiblingOf` | `io/slotmigrate/ktav_migration.go:510` (`ktavMigrateStartupSlots`) |
| `docread` | `ktavSiblingOf` | `KtavSiblingOf` | `load.go:152` (`loadOnce`) |
| `docread` | `legacySlotOf` | `LegacySlotOf` | `io/slotmigrate/ktav_migration_artifacts.go:397` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `legacySlotOf` | `LegacySlotOf` | `io/slots/ktav_light.go:20` (`ReadConfigDocumentLight`) |
| `docread` | `loadExternalMCPDocumentsStableForWorkingDir` | `LoadExternalMCPDocumentsStableForWorkingDir` | `load.go:236` (`loadOnce`) |
| `docread` | `loadMCPJSONBytes` | `LoadMCPJSONBytes` | `store_mcp_transaction.go:570` (`*ConfigStore.evaluateMCPFiles`) |
| `docread` | `lookupConfigCandidateGroups` | `LookupConfigCandidateGroups` | `load_files.go:21` (`lookupConfigs`) |
| `docread` | `lookupConfigCandidates` | `LookupConfigCandidates` | `io/docread/fingerprints.go:167` (`candidateInputsChanged`) |
| `docread` | `lookupConfigCandidates` | `LookupConfigCandidates` | `io/slotmigrate/ktav_migration.go:506` (`ktavMigrateStartupSlots`) |
| `docread` | `matchesLogicalSlot` | `MatchesLogicalSlot` | `io/slotmigrate/ktav_migration.go:479` (`ownerPolicyForMigration`) |
| `docread` | `matchesLogicalSlot` | `MatchesLogicalSlot` | `store_mcp_transaction.go:688` (`*ConfigStore.mcpOwnerPolicy`) |
| `docread` | `mcpEntryFromJSON` | `McpEntryFromJSON` | `io/docread/mcpdocs.go:284` (`readMCPDisabledOverride`) |
| `docread` | `mcpJSONCandidatePaths` | `McpJSONCandidatePaths` | `io/docread/fingerprints.go:168` (`candidateInputsChanged`) |
| `docread` | `mergeExternalMCPServersFromDocuments` | `MergeExternalMCPServersFromDocuments` | `load.go:243` (`loadOnce`) |
| `docread` | `migrationRecordPathFor` | `MigrationRecordPathFor` | `io/slotmigrate/ktav_migration.go:89` (`inspectKtavMigrationEntries`) |
| `docread` | `migrationRecordPathFor` | `MigrationRecordPathFor` | `io/slotmigrate/ktav_migration.go:520` (`ktavMigrateStartupSlots`) |
| `docread` | `migrationRecordPathFor` | `MigrationRecordPathFor` | `io/slotmigrate/ktav_migration_artifacts.go:449` (`ktavMigrateOAuthJournal`) |
| `docread` | `normalizeDiscoveryPath` | `NormalizeDiscoveryPath` | `io/cfgcommit/config_write_identity.go:29` (`configWriteTargetDedupKeyWithCaseFold`) |
| `docread` | `normalizeReloadPath` | `NormalizeReloadPath` | `io/cfgcommit/atomicwrite.go:321` (`verifySelectedCommitPath`) |
| `docread` | `normalizeReloadPath` | `NormalizeReloadPath` | `io/docread/fingerprints.go:171` (`candidateInputsChanged`) |
| `docread` | `normalizeReloadPath` | `NormalizeReloadPath` | `io/slotmigrate/ktav_migration.go:478` (`ownerPolicyForMigration`) |
| `docread` | `oauthJournalKtavPath` | `OauthJournalKtavPath` | `io/slotmigrate/ktav_migration_artifacts.go:407` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `oauthJournalKtavPath` | `OauthJournalKtavPath` | `io/slots/ktav_oauth.go:16` (`selectOAuthJournalPath`) |
| `docread` | `oauthJournalLegacyPath` | `OauthJournalLegacyPath` | `io/slotmigrate/ktav_migration_artifacts.go:405` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `oauthJournalLegacyPath` | `OauthJournalLegacyPath` | `io/slots/ktav_oauth.go:15` (`selectOAuthJournalPath`) |
| `docread` | `oauthJournalRecord` | `OauthJournalRecord` | `io/slotmigrate/ktav_migration_artifacts.go:421` (`ktavValidateOAuthJournalBytes`) |
| `docread` | `pathAlreadyLoaded` | `PathAlreadyLoaded` | `load.go:173` (`loadOnce`) |
| `docread` | `readConfigEntryAt` | `ReadConfigEntryAt` | `io/cfgcommit/config_linkat_unix.go:89` (`recoverConfigTempAlias`) |
| `docread` | `readConfigEntryAt` | `ReadConfigEntryAt` | `io/slotmigrate/ktav_migration_unix.go:53` (`archiveKtavBackup`) |
| `docread` | `readReloadFingerprint` | `ReadReloadFingerprint` | `store_config_disk_sync.go:304` (`configDiskSyncDigestsMatch`) |
| `docread` | `readSettingsDocument` | `ReadSettingsDocument` | `io/docread/mcpdocs.go:280` (`readMCPDisabledOverride`) |
| `docread` | `readSlotOf` | `ReadSlotOf` | `settings_lock.go:34` (`newUnrecognizedSettingsLock`) |
| `docread` | `readStableConfigDocumentsWithOwner` | `ReadStableConfigDocumentsWithOwner` | `io/docread/stabledocs.go:31` (`loadConfigCandidateStableForWorkingDir`) |
| `docread` | `readStableConfigFile` | `ReadStableConfigFile` | `settings_models_integrity.go:44` (`*ConfigStore.verifyModelsIntegrityAtLoad`) |
| `docread` | `readStableConfigFileOwned` | `ReadStableConfigFileOwned` | `apitoken_store.go:396` (`*APITokenStore.read`) |
| `docread` | `readStableConfigFileOwned` | `ReadStableConfigFileOwned` | `io/cfgcommit/config_commit_unix.go:25` (`commitConfigFile`) |
| `docread` | `readStableConfigFileOwned` | `ReadStableConfigFileOwned` | `io/slotmigrate/ktav_migration.go:229` (`runKtavMigrationTransaction`) |
| `docread` | `readStableConfigHandle` | `ReadStableConfigHandle` | `io/cfgcommit/config_commit_windows.go:107` (`commitConfigFile`) |
| `docread` | `readStableConfigHandle` | `ReadStableConfigHandle` | `io/fswap/config_rename_windows.go:200` (`verifyWindowsRenameRetryState`) |
| `docread` | `readStableConfigHandle` | `ReadStableConfigHandle` | `io/slotmigrate/ktav_migration_windows.go:57` (`archiveKtavBackup`) |
| `docread` | `rebindReloadFingerprintPath` | `RebindReloadFingerprintPath` | `store_mcp_transaction.go:633` (`*ConfigStore.mcpPathData`) |
| `docread` | `reloadFileFingerprint` | `Fingerprint` | `io/cfgcommit/config_commit_unix.go:21` (`commitConfigFile`) |
| `docread` | `reloadFileFingerprint` | `Fingerprint` | `io/cfgcommit/config_linkat_unix.go:87` (`recoverConfigTempAlias`) |
| `docread` | `reloadFileFingerprint` | `Fingerprint` | `io/docread/fingerprints.go:166` (`candidateInputsChanged`) |
| `docread` | `reloadFileFingerprint` | `Fingerprint` | `io/slotmigrate/ktav_migration.go:306` (`runKtavMigrationTransaction`) |
| `docread` | `reloadFingerprintsChangedWithOwner` | `ReloadFingerprintsChangedWithOwner` | `io/docread/fingerprints.go:169` (`candidateInputsChanged`) |
| `docread` | `reloadStalenessState` | `ReloadStalenessState` | `store.go:397` (`*ConfigStore.publishInitialSnapshot`) |
| `docread` | `sameBytesFingerprint` | `SameBytesFingerprint` | `io/cfgcommit/config_commit_unix.go:35` (`commitConfigFile`) |
| `docread` | `sameBytesFingerprint` | `SameBytesFingerprint` | `io/cfgcommit/config_linkat_unix.go:95` (`recoverConfigTempAlias`) |
| `docread` | `sameBytesFingerprint` | `SameBytesFingerprint` | `io/slotmigrate/ktav_migration.go:365` (`finishKtavArchival`) |
| `docread` | `sameBytesFingerprint` | `SameBytesFingerprint` | `store_mcp_transaction.go:463` (`*ConfigStore.reconcileMCPCommit`) |
| `docread` | `sameReloadPathSet` | `SameReloadPathSet` | `io/docread/fingerprints.go:167` (`candidateInputsChanged`) |
| `docread` | `selectConfigSlot` | `SelectConfigSlot` | `io/slots/ktav_doc.go:194` (`writeTargetSlot`) |
| `docread` | `selectConfigSlot` | `SelectConfigSlot` | `load.go:143` (`loadOnce`) |
| `docread` | `settingsDocument` | `SettingsDocument` | `settings_lock.go:492` (`*ConfigStore.writeSettingsLockRaw`) |
| `docread` | `settingsDocumentFormatOf` | `SettingsDocumentFormatOf` | `apitoken_store.go:523` (`*APITokenStore.mutate`) |
| `docread` | `stableConfigAlias` | `StableConfigAlias` | `load.go:191` (`loadOnce`) |
| `docread` | `stableConfigDocument` | `StableConfigDocument` | `io/docread/fingerprints.go:193` (`mcpInputFingerprints`) |
| `docread` | `trackedSlotFingerprint` | `TrackedSlotFingerprint` | `store_mcp_admission.go:684` (`*ConfigStore.mcpMutationResultCurrentLocked`) |
| `docread` | `validOAuthJournal` | `ValidOAuthJournal` | `io/slotmigrate/ktav_migration_artifacts.go:422` (`ktavValidateOAuthJournalBytes`) |
| `docread` | `warnBothFormatsOnce` | `WarnBothFormatsOnce` | `io/slots/ktav_cache.go:41` (`readCacheDocument`) |
| `docread` | `warnKtavArtifactConflictOnce` | `WarnKtavArtifactConflictOnce` | `io/slotmigrate/ktav_migration_artifacts.go:408` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `warnKtavArtifactConflictOnce` | `WarnKtavArtifactConflictOnce` | `io/slotmigrate/ktav_migration_artifacts.go:517` (`ktavMigrateCacheArtifactsStartup`) |
| `docread` | `warnMigrationDeferredOnce` | `WarnMigrationDeferredOnce` | `io/slotmigrate/ktav_migration.go:523` (`ktavMigrateStartupSlots`) |
| `docread` | `warnMigrationDeferredOnce` | `WarnMigrationDeferredOnce` | `io/slotmigrate/ktav_migration_artifacts.go:154` (`runKtavArtifactMigrationStateLocked`) |
| `docread` | `warnMigrationDeferredOnce` | `WarnMigrationDeferredOnce` | `io/slotmigrate/ktav_migration_artifacts.go:410` (`ktavMigrateOAuthJournalsAtStartup`) |
| `docread` | `workspaceConfigPath` | `WorkspaceConfigPath` | `load.go:144` (`loadOnce`) |
| `fileid` | `ErrConfigHardLink` | `ErrConfigHardLink` | `io/cfgcommit/config_commit_unix.go:70` (`commitConfigFile`) |
| `fileid` | `ErrConfigHardLink` | `ErrConfigHardLink` | `io/cfgcommit/config_linkat_unix.go:25` (`publishConfigTempNoReplaceAt`) |
| `fileid` | `ErrConfigHardLink` | `ErrConfigHardLink` | `io/slotmigrate/ktav_migration.go:234` (`runKtavMigrationTransaction`) |
| `fileid` | `ErrConfigNonRegular` | `ErrConfigNonRegular` | `io/docread/stabledocs.go:141` (`readStableConfigFileOwned`) |
| `fileid` | `ErrConfigNonRegular` | `ErrConfigNonRegular` | `store_mcp_admission.go:393` (`validatePreparedAdmissionFile`) |
| `fileid` | `configFileIdentity` | `FileIdentity` | `io/casefold/config_case_sensitivity.go:77` (`configCaseDirectoryEntry`) |
| `fileid` | `configFileIdentity` | `FileIdentity` | `io/cfgcommit/config_commit_windows.go:265` (`classifyWindowsRenameFailure`) |
| `fileid` | `configFileIdentity` | `FileIdentity` | `io/cfgcommit/config_linkat_unix.go:52` (`configTempIdentityAt`) |
| `fileid` | `configFileIdentity` | `FileIdentity` | `io/docread/fingerprints.go:35` (`reloadFileFingerprint`) |
| `fileid` | `configFileIdentity` | `FileIdentity` | `store_mcp_locks.go:35` (`mcpFileRecord`) |
| `fileid` | `configFileIdentityAtPath` | `ConfigFileIdentityAtPath` | `apitoken_store.go:377` (`*APITokenStore.read`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `apitoken_store.go:375` (`*APITokenStore.read`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `io/casefold/config_case_sensitivity_behavioral_unix.go:20` (`configDirectoryCaseSensitivityByEntries`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `io/cfgcommit/config_commit_unix.go:89` (`commitConfigFile`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `io/cfgcommit/config_linkat_unix.go:66` (`configTempIdentityAt`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `io/cfgcommit/config_linkat_unix.go:186` (`readConfigEntryAt`) |
| `fileid` | `configFileIdentityOf` | `ConfigFileIdentityOf` | `io/slotmigrate/ktav_migration_unix.go:37` (`archiveKtavBackup`) |
| `fileid` | `configFileIdentityOfOpened` | `ConfigFileIdentityOfOpened` | `io/cfgcommit/config_commit_windows.go:181` (`verifyWindowsCommitParent`) |
| `fileid` | `configFileIdentityOfOpened` | `ConfigFileIdentityOfOpened` | `io/docread/stabledocs.go:181` (`readStableConfigFileOwned`) |
| `fileid` | `configFileIdentityOfOpened` | `ConfigFileIdentityOfOpened` | `io/fswap/config_rename_windows.go:155` (`verifyWindowsRenameRetryState`) |
| `fileid` | `configFileIdentityOfOpened` | `ConfigFileIdentityOfOpened` | `io/slotmigrate/ktav_migration_windows.go:37` (`archiveKtavBackup`) |
| `fileid` | `configFileIdentityOfOpened` | `ConfigFileIdentityOfOpened` | `store_mcp_admission.go:540` (`validateFinalMCPAdmissionFile`) |
| `fileid` | `configFileNlinkOfInfo` | `ConfigFileNlinkOfInfo` | `io/cfgcommit/config_linkat_unix.go:121` (`recoverConfigTempAlias`) |
| `fileid` | `configFileNlinkOfInfo` | `ConfigFileNlinkOfInfo` | `io/cfgcommit/config_linkat_unix.go:189` (`readConfigEntryAt`) |
| `fileid` | `configFileNlinkOfOpened` | `ConfigFileNlinkOfOpened` | `io/cfgcommit/config_commit_unix.go:259` (`verifyCommitEntry`) |
| `fileid` | `configFileNlinkOfOpened` | `ConfigFileNlinkOfOpened` | `io/cfgcommit/config_linkat_unix.go:70` (`configTempIdentityAt`) |
| `fileid` | `configFileNlinkOfOpened` | `ConfigFileNlinkOfOpened` | `io/docread/stabledocs.go:182` (`readStableConfigFileOwned`) |
| `fileid` | `configFileNlinkOfOpened` | `ConfigFileNlinkOfOpened` | `store_mcp_admission.go:421` (`validatePreparedAdmissionFile`) |
| `fileid` | `configFileOwner` | `ConfigFileOwner` | `apitoken_store.go:368` (`*APITokenStore.read`) |
| `fileid` | `configFileOwner` | `ConfigFileOwner` | `io/cfgcommit/config_commit_unix.go:50` (`commitConfigFile`) |
| `fileid` | `configFileOwner` | `ConfigFileOwner` | `io/cfgcommit/config_linkat_unix.go:164` (`readConfigEntryAt`) |
| `fileid` | `configFileOwner` | `ConfigFileOwner` | `io/cfgcommit/config_linkat_unix.go:230` (`configTempAliasPolicyAcceptable`) |
| `fileid` | `configFileOwner` | `ConfigFileOwner` | `io/slotmigrate/ktav_migration_unix.go:33` (`archiveKtavBackup`) |
| `fileid` | `configFilePathIdentityMatches` | `ConfigFilePathIdentityMatches` | `io/docread/stabledocs.go:171` (`readStableConfigFileOwned`) |
| `fileid` | `configFilePathIdentityMatches` | `ConfigFilePathIdentityMatches` | `store_mcp_admission.go:432` (`validatePreparedAdmissionFile`) |
| `fileid` | `configParentIdentity` | `ConfigParentIdentity` | `io/cfgcommit/atomicwrite.go:332` (`caseEquivalentConfigPaths`) |
| `fileid` | `configParentIdentity` | `ConfigParentIdentity` | `io/cfgcommit/config_write_identity.go:26` (`configWriteTargetDedupKeyWithCaseFold`) |
| `fileid` | `configParentIdentity` | `ConfigParentIdentity` | `io/docread/stabledocs.go:120` (`readStableConfigFileOwned`) |
| `fileid` | `configParentIdentity` | `ConfigParentIdentity` | `io/slotmigrate/ktav_migration.go:309` (`runKtavMigrationTransaction`) |
| `fileid` | `createWindowsConfigEntryAt` | `CreateWindowsConfigEntryAt` | `io/slotmigrate/ktav_migration_windows.go:51` (`archiveKtavBackup`) |
| `fileid` | `deleteWindowsConfigHandle` | `DeleteWindowsConfigHandle` | `io/cfgcommit/config_commit_windows.go:71` (`commitConfigFile`) |
| `fileid` | `errConfigCommitVerification` | `ErrConfigCommitVerification()` | `io/cfgcommit/atomicwrite.go:323` (`verifySelectedCommitPath`) |
| `fileid` | `errConfigCommitVerification` | `ErrConfigCommitVerification()` | `io/cfgcommit/config_linkat_unix.go:38` (`publishConfigTempNoReplaceAt`) |
| `fileid` | `errConfigCommitVerification` | `ErrConfigCommitVerification()` | `io/cfgcommit/config_linkat_unix.go:162` (`readConfigEntryAt`) |
| `fileid` | `errConfigCommitVerification` | `ErrConfigCommitVerification()` | `io/slotmigrate/ktav_migration_unix.go:35` (`archiveKtavBackup`) |
| `fileid` | `errConfigCommitVerification` | `ErrConfigCommitVerification()` | `store_mcp_transaction.go:436` (`*ConfigStore.writeMCPFileChanges`) |
| `fileid` | `errConfigOwnerMismatch` | `ErrConfigOwnerMismatch()` | `apitoken_store.go:370` (`*APITokenStore.read`) |
| `fileid` | `errConfigOwnerMismatch` | `ErrConfigOwnerMismatch()` | `io/cfgcommit/config_linkat_unix.go:166` (`readConfigEntryAt`) |
| `fileid` | `errStableReadUnstable` | `ErrStableReadUnstable()` | `io/cfgcommit/config_linkat_unix.go:177` (`readConfigEntryAt`) |
| `fileid` | `errStableReadUnstable` | `ErrStableReadUnstable()` | `load.go:99` (`Load`) |
| `fileid` | `flushWindowsConfigHandle` | `FlushWindowsConfigHandle` | `io/cfgcommit/config_commit_windows.go:106` (`commitConfigFile`) |
| `fileid` | `homeConfigOwner` | `HomeConfigOwner` | `apitoken_store.go:369` (`*APITokenStore.read`) |
| `fileid` | `homeConfigOwner` | `HomeConfigOwner` | `io/docread/candidates.go:119` (`configOwnerPolicyForPath`) |
| `fileid` | `isWindowsEntryNotFound` | `IsWindowsEntryNotFound` | `io/cfgcommit/config_commit_windows.go:214` (`verifyWindowsCommitDestinationHandle`) |
| `fileid` | `isWindowsEntryNotFound` | `IsWindowsEntryNotFound` | `io/fswap/config_rename_windows.go:184` (`verifyWindowsRenameRetryState`) |
| `fileid` | `isWindowsEntryNotFound` | `IsWindowsEntryNotFound` | `io/slotmigrate/ktav_migration_windows.go:46` (`archiveKtavBackup`) |
| `fileid` | `openStableConfigFile` | `OpenStableConfigFile` | `io/docread/stabledocs.go:122` (`readStableConfigFileOwned`) |
| `fileid` | `openStableConfigFile` | `OpenStableConfigFile` | `store_mcp_admission.go:321` (`*ConfigStore.prepareMCPAdmissionToken`) |
| `fileid` | `openWindowsConfigEntryAt` | `OpenWindowsConfigEntryAt` | `io/cfgcommit/config_commit_windows.go:212` (`verifyWindowsCommitDestinationHandle`) |
| `fileid` | `openWindowsConfigEntryAt` | `OpenWindowsConfigEntryAt` | `io/fswap/config_rename_windows.go:165` (`verifyWindowsRenameRetryState`) |
| `fileid` | `openWindowsConfigEntryAt` | `OpenWindowsConfigEntryAt` | `io/slotmigrate/ktav_migration_windows.go:41` (`archiveKtavBackup`) |
| `fileid` | `openWindowsConfigHandle` | `OpenWindowsConfigHandle` | `io/casefold/config_case_sensitivity_windows.go:33` (`windowsConfigDirectoryCaseInsensitive`) |
| `fileid` | `openWindowsConfigHandle` | `OpenWindowsConfigHandle` | `io/cfgcommit/config_durability_windows.go:15` (`syncConfigParentOnDisk`) |
| `fileid` | `openWindowsConfigParent` | `OpenWindowsConfigParent` | `io/cfgcommit/config_commit_windows.go:36` (`commitConfigFile`) |
| `fileid` | `openWindowsConfigParent` | `OpenWindowsConfigParent` | `io/slotmigrate/ktav_migration_windows.go:23` (`archiveKtavBackup`) |
| `fileid` | `physicalWindowsConfigPath` | `PhysicalWindowsConfigPath` | `io/cfgcommit/config_commit_windows.go:31` (`commitConfigFile`) |
| `fileid` | `readOpenedConfigBytes` | `ReadOpenedConfigBytes` | `io/cfgcommit/config_linkat_unix.go:168` (`readConfigEntryAt`) |
| `fileid` | `readOpenedConfigBytes` | `ReadOpenedConfigBytes` | `store_mcp_admission.go:399` (`validatePreparedAdmissionFile`) |
| `fileid` | `sameConfigFileIdentity` | `SameConfigFileIdentity` | `io/cfgcommit/config_linkat_unix.go:183` (`readConfigEntryAt`) |
| `fileid` | `stageConfigFileHandleAt` | `StageConfigFileHandleAt` | `io/cfgcommit/config_commit_windows.go:61` (`commitConfigFile`) |
| `fileid` | `systemConfigOwner` | `SystemConfigOwner` | `io/docread/candidates.go:116` (`configOwnerPolicyForPath`) |
| `fileid` | `systemConfigOwner` | `SystemConfigOwner` | `server_listeners.go:143` (`GlobalServerListenersWithSources`) |
| `fileid` | `windowsConfigAPIPath` | `WindowsConfigAPIPath` | `io/fswap/config_rename_windows.go:229` (`renameConfigTemp`) |
| `fileid` | `windowsFileInformation` | `WindowsFileInformation` | `io/casefold/config_case_sensitivity_windows.go:39` (`windowsConfigDirectoryCaseInsensitive`) |
| `fileid` | `windowsFileInformation` | `WindowsFileInformation` | `io/cfgcommit/config_commit_windows.go:230` (`verifyWindowsCommitDestinationHandle`) |
| `fileid` | `windowsFileInformation` | `WindowsFileInformation` | `io/fileid/config_file_identity_windows.go:373` (`readStableConfigHandle`) |
| `fileid` | `windowsFileInformation` | `WindowsFileInformation` | `io/fswap/config_rename_windows.go:174` (`verifyWindowsRenameRetryState`) |
| `fileid` | `windowsStagedConfigFile` | `WindowsStagedConfigFile` | `io/cfgcommit/config_commit_windows.go:253` (`classifyWindowsRenameFailure`) |
| `fswap` | `cleanupConfigTempAlias` | `CleanupConfigTempAlias` | `io/cfgcommit/config_commit_unix.go:93` (`commitConfigFile`) |
| `fswap` | `configDirectoryIsCaseInsensitive` | `ConfigDirectoryIsCaseInsensitive` | `io/cfgcommit/atomicwrite.go:334` (`caseEquivalentConfigPaths`) |
| `fswap` | `configEquivalentLeafNames` | `ConfigEquivalentLeafNames` | `io/cfgcommit/atomicwrite.go:329` (`caseEquivalentConfigPaths`) |
| `fswap` | `configEquivalentLeafNames` | `ConfigEquivalentLeafNames` | `store_mcp_transaction.go:812` (`configEquivalentMissingMCPInput`) |
| `fswap` | `configWriteIdentityKey` | `ConfigWriteIdentityKey` | `io/cfgcommit/config_write_identity.go:18` (`configWriteTargetDedupKeyWithCaseFold`) |
| `fswap` | `recoverConfigTempAlias` | `RecoverConfigTempAlias` | `io/cfgcommit/config_commit_unix.go:61` (`commitConfigFile`) |
| `fswap` | `renameConfigTemp` | `RenameConfigTemp` | `io/cfgcommit/atomicwrite.go:352` (`atomicWriteFile`) |
| `fswap` | `renameConfigTempAt` | `RenameConfigTempAt` | `io/cfgcommit/config_commit_unix.go:148` (`commitConfigFile`) |
| `fswap` | `renameConfigTempAt` | `RenameConfigTempAt` | `io/slotmigrate/ktav_migration_unix.go:66` (`archiveKtavBackup`) |
| `fswap` | `renameConfigTempHandle` | `RenameConfigTempHandle` | `io/cfgcommit/config_commit_windows.go:96` (`commitConfigFile`) |
| `fswap` | `renameConfigTempHandle` | `RenameConfigTempHandle` | `io/slotmigrate/ktav_migration_windows.go:71` (`archiveKtavBackup`) |
| `fswap` | `syncConfigParent` | `SyncConfigParent` | `io/cfgcommit/atomicwrite.go:356` (`atomicWriteFile`) |
| `fswap` | `syncConfigParent` | `SyncConfigParent` | `io/slotmigrate/ktav_migration.go:164` (`runKtavMigrationStateLocked`) |
| `fswap` | `syncConfigParent` | `SyncConfigParent` | `io/slots/oauthjournal.go:16` (`<package>`) |
| `fswap` | `syncConfigParentFile` | `SyncConfigParentFile` | `io/cfgcommit/config_commit_unix.go:183` (`commitConfigFile`) |
| `fswap` | `syncConfigParentFile` | `SyncConfigParentFile` | `io/slotmigrate/ktav_migration_unix.go:87` (`archiveKtavBackup`) |
| `fswap` | `unlinkConfigTempAt` | `UnlinkConfigTempAt` | `io/cfgcommit/config_commit_unix.go:86` (`commitConfigFile`) |
| `slotmigrate` | `inspectKtavMigrationEntries` | `InspectKtavMigrationEntries` | `io/slotmigrate/ktav_migration.go:550` (`ktavMigrateWorkspaceIfNeeded`) |
| `slotmigrate` | `migrateKtavArtifact` | `MigrateKtavArtifact` | `io/slotmigrate/ktav_migration_artifacts.go:515` (`ktavMigrateCacheArtifactsStartup`) |
| `slotmigrate` | `migrateKtavArtifactToWithLock` | `MigrateKtavArtifactToWithLock` | `io/slotmigrate/ktav_migration_artifacts.go:454` (`ktavMigrateOAuthJournal`) |
| `slotmigrate` | `runKtavMigrationState` | `RunKtavMigrationState` | `io/slotmigrate/ktav_migration.go:499` (`ktavMigrateStartupSlots`) |
| `slotmigrate` | `runKtavMigrationStateLocked` | `RunKtavMigrationStateLocked` | `store_write.go:284` (`*ConfigStore.withConfigWriteLockCtx`) |
| `slots` | `ActiveConfigFileName` | `ActiveConfigFileName` | `store_mcp_transaction.go:607` (`*ConfigStore.orderedMCPPaths`) |
| `slots` | `ReadConfigDocumentLight` | `ReadConfigDocumentLight` | `store_models.go:51` (`*ConfigStore.ReadAllModelsAtScope`) |
| `slots` | `ktavMigrateStartupSlots` | `KtavMigrateStartupSlots` | `load.go:116` (`loadOnce`) |
| `slots` | `ktavMigrateWorkspaceIfNeeded` | `KtavMigrateWorkspaceIfNeeded` | `load.go:149` (`loadOnce`) |
| `slots` | `newConfigFileIsKtav` | `NewConfigFileIsKtav` | `apitoken_store.go:462` (`*APITokenStore.selectedPath`) |
| `slots` | `readCacheDocument` | `ReadCacheDocument` | `model_catalog_cache.go:52` (`readModelCatalog`) |
| `slots` | `selectOAuthJournalPath` | `SelectOAuthJournalPath` | `store_oauth_journal_outbox.go:167` (`*ConfigStore.flushOAuthOutbox`) |
| `slots` | `writeCacheDocument` | `WriteCacheDocument` | `model_catalog_cache.go:86` (`cachedProviderModels`) |
| `slots` | `writeTargetSlot` | `WriteTargetSlot` | `provider_peak_hours_read.go:27` (`*ConfigStore.PeakHoursEntryForScope`) |

Explicit API spelling constraints: `reloadFileFingerprint`'s type/API is `Fingerprint` (not `ReloadFileFingerprint`); identity/document types are `FileIdentity`, `FileSnapshot`, `StableConfigDocument`, `StableConfigAlias`, and `SettingsDocument`; the Windows staging type is `WindowsStagedConfigFile`. For error declarations, use `Err…()` getters when a lower package must provide identity; `ErrOAuthJournalDisk()` in `docread` is the single lower identity and the root-facing name aliases that identity, not a duplicate error. `errReloadDiskChanged` remains root-owned. `ktavAutoMigrate` remains root-owned mutable storage; slots owns its API semantics and consumers use an accessor/provider, never an exported mutable global. `Error`/`Unwrap` stay methods.

### D. ROOT production call locations into the eight IO owners

Evidence is grouped by destination and source file. Each location is a distinct `(file, line, identifier, enclosing declaration)` tuple from projection evidence with `call=true`; `_test.go` sources are excluded. Same-line repeated calls to the same identifier are deduplicated by the ledger.

| IO destination | ROOT source file | Unique call locations | Evidence (`line:identifier` — enclosing) |
|---|---|---:|---|
| `casefold` | `io/cfgcommit/config_write_identity.go` | 1 | 44:configFoldCaseLeaf — `configWriteTargetDedupKeyWithCaseSensitivity` |
| **casefold total** | — | **1** | — |
| `cfgcommit` | `api_remotes.go` | 1 | 321:atomicWriteFile — `WriteAPIRemoteTokenFile` |
| `cfgcommit` | `apitoken_store.go` | 1 | 693:atomicWriteFile — `*APITokenStore.flushLastUsedSnapshot` |
| `cfgcommit` | `settings_lock.go` | 2 | 523:commitConfigFile — `*ConfigStore.writeSettingsLockRaw`; 525:CommitOutcomeFromError — `*ConfigStore.writeSettingsLockRaw` |
| `cfgcommit` | `store_mcp_admission.go` | 1 | 178:CommitOutcomeFromError — `mcpCommitWasReconciled` |
| `cfgcommit` | `store_mcp_transaction.go` | 12 | 378:commitConfigFile — `*ConfigStore.writeMCPFileChanges`; 382:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 398:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 399:cloneCommitOutcome — `*ConfigStore.writeMCPFileChanges`; 404:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 405:cloneCommitOutcome — `*ConfigStore.writeMCPFileChanges`; 414:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 425:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 426:cloneCommitOutcome — `*ConfigStore.writeMCPFileChanges`; 431:CommitOutcomeFromError — `*ConfigStore.writeMCPFileChanges`; 432:cloneCommitOutcome — `*ConfigStore.writeMCPFileChanges`; 447:newCommitOutcome — `*ConfigStore.writeMCPFileChanges` |
| `cfgcommit` | `store_oauth.go` | 3 | 334:CommitOutcomeFromError — `*ConfigStore.refreshLegacyOAuthLeader`; 432:commitConfigFile — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 434:CommitOutcomeFromError — `*ConfigStore.setOAuthTokenIfCurrentWithLock` |
| `cfgcommit` | `store_oauth_journal_quarantine.go` | 1 | 20:atomicWriteFile — `<package>` |
| `cfgcommit` | `store_write.go` | 5 | 485:commitConfigFile — `*ConfigStore.setConfigFieldsAuthority`; 487:CommitOutcomeFromError — `*ConfigStore.setConfigFieldsAuthority`; 570:CommitOutcomeFromError — `*ConfigStore.RemoveConfigFieldWithAuthority`; 671:commitConfigFile — `*ConfigStore.removeConfigFieldAt`; 673:CommitOutcomeFromError — `*ConfigStore.removeConfigFieldAt` |
| **cfgcommit total** | — | **26** | — |
| `cfghooks` | `store_mcp_admission.go` | 1 | 229:configContextWithTimeout — `*ConfigStore.WithCurrentMCPAdmission` |
| `cfghooks` | `store_mcp_locks.go` | 3 | 173:configContextWithTimeout — `*ConfigStore.withMCPWriteLocks`; 179:configContextWithTimeout — `*ConfigStore.withMCPAdmissionLocks`; 302:acquireConfigFileLock — `*ConfigStore.withMCPLocksUsingCaseSensitivity` |
| `cfghooks` | `store_mcp_transaction.go` | 2 | 444:runConfigAfterMCPCommitHook — `*ConfigStore.writeMCPFileChanges`; 461:runConfigBeforeMCPReconcileHook — `*ConfigStore.reconcileMCPCommit` |
| `cfghooks` | `store_oauth_disk_sync.go` | 2 | 177:acquireConfigFileLock — `*ConfigStore.drainOAuthOutboxOnStop`; 185:releaseConfigFileLock — `*ConfigStore.drainOAuthOutboxOnStop` |
| `cfghooks` | `store_oauth_journal_worker.go` | 7 | 237:acquireConfigFileLock — `*ConfigStore.withOAuthJournalRecoveryWriteLock`; 241:releaseConfigFileLock — `*ConfigStore.withOAuthJournalRecoveryWriteLock`; 265:acquireConfigFileLock — `*ConfigStore.recoverOAuthJournalsAtLoad`; 268:releaseConfigFileLock — `*ConfigStore.recoverOAuthJournalsAtLoad`; 353:acquireConfigFileLock — `*ConfigStore.oauthJournalWorkerAttempt`; 360:releaseConfigFileLock — `*ConfigStore.oauthJournalWorkerAttempt`; 364:releaseConfigFileLock — `*ConfigStore.oauthJournalWorkerAttempt` |
| `cfghooks` | `store_oauth_provider.go` | 2 | 116:acquireConfigFileLock — `*ConfigStore.refreshLegacyOAuthToken`; 120:releaseConfigFileLock — `*ConfigStore.refreshLegacyOAuthToken` |
| `cfghooks` | `store_oauth_refresh.go` | 4 | 215:acquireConfigFileLock — `*ConfigStore.refreshCodexOAuthLeader`; 220:releaseConfigFileLock — `*ConfigStore.refreshCodexOAuthLeader`; 409:acquireConfigFileLock — `*ConfigStore.withCodexConfigWriteLock`; 413:releaseConfigFileLock — `*ConfigStore.withCodexConfigWriteLock` |
| `cfghooks` | `store_write.go` | 9 | 250:acquireConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 269:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 273:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 285:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 291:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 295:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 301:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 319:releaseConfigFileLock — `*ConfigStore.withConfigWriteLockCtx`; 735:configContextWithTimeout — `*ConfigStore.removeConfigFieldBestEffort` |
| **cfghooks total** | — | **30** | — |
| `docread` | `apitoken_store.go` | 5 | 396:readStableConfigFileOwned — `*APITokenStore.read`; 405:decodeSettingsDocument — `*APITokenStore.read`; 488:configWriteLockAnchor — `*APITokenStore.mutate`; 523:settingsDocumentFormatOf — `*APITokenStore.mutate`; 524:encodeSettingsDocument — `*APITokenStore.mutate` |
| `docread` | `io/cfgcommit/config_write_identity.go` | 1 | 29:normalizeDiscoveryPath — `configWriteTargetDedupKeyWithCaseFold` |
| `docread` | `io/docread/fingerprints.go` | 12 | 167:lookupConfigCandidates — `candidateInputsChanged`; 167:sameReloadPathSet — `candidateInputsChanged`; 168:mcpJSONCandidatePaths — `candidateInputsChanged`; 168:sameReloadPathSet — `candidateInputsChanged`; 169:reloadFingerprintsChangedWithOwner — `candidateInputsChanged`; 171:normalizeReloadPath — `candidateInputsChanged`; 174:configOwnerPolicyForPath — `candidateInputsChanged`; 210:normalizeReloadPath — `mcpInputFingerprints`; 212:normalizeDiscoveryPath — `mcpInputFingerprints`; 224:isKtavConfigPath — `mcpInputFingerprints`; 257:normalizeReloadPath — `mcpInputFingerprints`; 259:normalizeDiscoveryPath — `mcpInputFingerprints` |
| `docread` | `io/docread/mcpdocs.go` | 3 | 280:readSettingsDocument — `readMCPDisabledOverride`; 284:mcpEntryFromJSON — `readMCPDisabledOverride`; 285:isMCPDisabledOnlyEntry — `readMCPDisabledOverride` |
| `docread` | `io/docread/stabledocs.go` | 4 | 31:readStableConfigDocumentsWithOwner — `loadConfigCandidateStableForWorkingDir`; 32:configOwnerPolicyForPath — `loadConfigCandidateStableForWorkingDir`; 47:configDocumentBytes — `loadConfigCandidateStableForWorkingDir`; 48:addDocumentAliasFingerprints — `loadConfigCandidateStableForWorkingDir` |
| `docread` | `io/slotmigrate/ktav_migration_artifacts.go` | 16 | 397:legacySlotOf — `ktavMigrateOAuthJournalsAtStartup`; 397:normalizeReloadPath — `ktavMigrateOAuthJournalsAtStartup`; 405:oauthJournalLegacyPath — `ktavMigrateOAuthJournalsAtStartup`; 407:ktavEntryExists — `ktavMigrateOAuthJournalsAtStartup`; 407:oauthJournalKtavPath — `ktavMigrateOAuthJournalsAtStartup`; 408:oauthJournalKtavPath — `ktavMigrateOAuthJournalsAtStartup`; 408:warnKtavArtifactConflictOnce — `ktavMigrateOAuthJournalsAtStartup`; 410:warnMigrationDeferredOnce — `ktavMigrateOAuthJournalsAtStartup`; 422:validOAuthJournal — `ktavValidateOAuthJournalBytes`; 436:oauthJournalLegacyPath — `ktavMigrateOAuthJournal`; 437:oauthJournalKtavPath — `ktavMigrateOAuthJournal`; 438:ktavEntryExists — `ktavMigrateOAuthJournal`; 449:ktavEntryExists — `ktavMigrateOAuthJournal`; 449:migrationRecordPathFor — `ktavMigrateOAuthJournal`; 457:backupPathFor — `ktavMigrateOAuthJournal`; 458:ktavEntryExists — `ktavMigrateOAuthJournal` |
| `docread` | `load.go` | 22 | 111:checkAuthoritativeBackupOnly — `loadOnce`; 121:lookupConfigCandidates — `loadOnce`; 143:normalizeReloadPath — `loadOnce`; 143:selectConfigSlot — `loadOnce`; 144:normalizeDiscoveryPath — `loadOnce`; 144:workspaceConfigPath — `loadOnce`; 145:normalizeReloadPath — `loadOnce`; 146:checkBackupOnlySlot — `loadOnce`; 152:fileExists — `loadOnce`; 152:ktavSiblingOf — `loadOnce`; 153:normalizeDiscoveryPath — `loadOnce`; 153:workspaceConfigPath — `loadOnce`; 154:normalizeReloadPath — `loadOnce`; 173:pathAlreadyLoaded — `loadOnce`; 174:configOwnerForWorkingDir — `loadOnce`; 178:readStableConfigFileOwned — `loadOnce`; 181:decodeConfigDocument — `loadOnce`; 215:normalizeReloadPath — `loadOnce`; 235:mcpJSONCandidatePaths — `loadOnce`; 236:loadExternalMCPDocumentsStableForWorkingDir — `loadOnce`; 243:mergeExternalMCPServersFromDocuments — `loadOnce`; 370:lookupConfigCandidates — `ResolveDataDirectory` |
| `docread` | `load_files.go` | 2 | 21:lookupConfigCandidateGroups — `lookupConfigs`; 113:readStableConfigDocumentsWithOwner — `readStableConfigDocuments` |
| `docread` | `model_catalog_cache.go` | 3 | 126:ktavSiblingOf — `clearAllConfirmedModels`; 136:ktavSiblingOf — `clearConfirmedModels`; 147:ktavSiblingOf — `clearModelCatalog` |
| `docread` | `provider.go` | 4 | 87:decodeConfigDocument — `UpdateProviders`; 127:decodeConfigDocument — `UpdateHyper`; 277:ktavEntryExists — `cache.Age`; 277:ktavSiblingOf — `cache.Age` |
| `docread` | `provider_peak_hours_read.go` | 2 | 32:readStableConfigFileOwned — `*ConfigStore.PeakHoursEntryForScope`; 39:decodeSettingsDocument — `*ConfigStore.PeakHoursEntryForScope` |
| `docread` | `server_listeners.go` | 3 | 152:selectConfigSlot — `GlobalServerListenersWithSources`; 153:readStableConfigFileOwned — `GlobalServerListenersWithSources`; 160:decodeConfigDocument — `GlobalServerListenersWithSources` |
| `docread` | `settings_lock.go` | 16 | 34:readSlotOf — `newUnrecognizedSettingsLock`; 36:readSlotOf — `newUnrecognizedSettingsLock`; 173:readSettingsDocument — `CheckPasswordAgainstDisk`; 278:readSettingsDocument — `freshSettingsHash`; 356:normalizeReloadPath — `readSettingsHash`; 365:readSettingsDocument — `readSettingsHash`; 403:normalizeReloadPath — `*ConfigStore.lockHashesForTarget`; 406:normalizeReloadPath — `*ConfigStore.lockHashesForTarget`; 409:normalizeReloadPath — `*ConfigStore.lockHashesForTarget`; 472:normalizeReloadPath — `*ConfigStore.UnlockSettings`; 491:readStableConfigFileOwned — `*ConfigStore.writeSettingsLockRaw`; 495:settingsDocumentFormatOf — `*ConfigStore.writeSettingsLockRaw`; 501:decodeSettingsDocument — `*ConfigStore.writeSettingsLockRaw`; 513:encodeSettingsDocument — `*ConfigStore.writeSettingsLockRaw`; 518:readStableConfigFileOwned — `*ConfigStore.writeSettingsLockRaw`; 609:normalizeReloadPath — `*ConfigStore.settingsScopeForPath` |
| `docread` | `settings_lock_sources.go` | 5 | 17:readSlotOf — `<package>`; 34:readSlotOf — `naturalWorkspaceSettingsPath`; 71:normalizeReloadPath — `*ConfigStore.checkExternalSettingsSources`; 89:normalizeReloadPath — `*ConfigStore.checkExternalSettingsSources`; 94:normalizeReloadPath — `*ConfigStore.checkExternalSettingsSources` |
| `docread` | `settings_lock_sources_other.go` | 1 | 18:readSlotOf — `platformCanonicalGlobalDataPath` |
| `docread` | `settings_lock_sources_windows.go` | 1 | 19:readSlotOf — `platformCanonicalGlobalDataPath` |
| `docread` | `settings_models_integrity.go` | 3 | 24:readSlotOf — `*ConfigStore.anySettingsLockExists`; 44:readStableConfigFile — `*ConfigStore.verifyModelsIntegrityAtLoad`; 52:decodeSettingsDocument — `*ConfigStore.verifyModelsIntegrityAtLoad` |
| `docread` | `store.go` | 3 | 395:configAndMCPStalenessPaths — `*ConfigStore.publishInitialSnapshot`; 395:lookupConfigCandidates — `*ConfigStore.publishInitialSnapshot`; 397:reloadStalenessState — `*ConfigStore.publishInitialSnapshot` |
| `docread` | `store_config_disk_sync.go` | 7 | 212:sameReloadPathSet — `*ConfigStore.syncConfigFromDisk`; 290:lookupConfigCandidates — `configDiskSyncCandidatePaths`; 291:mcpJSONCandidatePaths — `configDiskSyncCandidatePaths`; 296:expandConfigSlotSiblings — `configDiskSyncCandidatePaths`; 304:readReloadFingerprint — `configDiskSyncDigestsMatch`; 326:normalizeDiscoveryPath — `configDiskSyncDigestKey`; 332:readReloadFingerprint — `configDiskSyncDigestKey` |
| `docread` | `store_custom_roles.go` | 1 | 80:readSettingsDocument — `*ConfigStore.CustomRolesForScope` |
| `docread` | `store_mcp.go` | 3 | 197:normalizeReloadPath — `*ConfigStore.PersistMCPFields`; 274:normalizeReloadPath — `*ConfigStore.PersistMCPFieldsExact`; 392:normalizeDiscoveryPath — `*ConfigStore.publishMCPValueAndStalenessLocked` |
| `docread` | `store_mcp_admission.go` | 10 | 374:configDiscoveryFingerprint — `mcpAdmissionAbsenceFingerprint`; 375:configDiscoveryFingerprint — `mcpAdmissionAbsenceFingerprint`; 377:configAliasChainFingerprint — `mcpAdmissionAbsenceFingerprint`; 426:configDiscoveryFingerprint — `validatePreparedAdmissionFile`; 427:configDiscoveryFingerprint — `validatePreparedAdmissionFile`; 429:configAliasChainFingerprint — `validatePreparedAdmissionFile`; 567:configDiscoveryFingerprint — `validateFinalMCPAdmissionFile`; 568:configDiscoveryFingerprint — `validateFinalMCPAdmissionFile`; 570:configAliasChainFingerprint — `validateFinalMCPAdmissionFile`; 684:trackedSlotFingerprint — `*ConfigStore.mcpMutationResultCurrentLocked` |
| `docread` | `store_mcp_locks.go` | 18 | 61:normalizeDiscoveryPath — `mcpRecordKey`; 65:normalizeDiscoveryPath — `*mcpLockedFiles.bindMCPPath`; 75:normalizeReloadPath — `*mcpLockedFiles.bindMCPPath`; 84:normalizeReloadPath — `*mcpLockedFiles.bindMCPPath`; 88:normalizeReloadPath — `*mcpLockedFiles.bindMCPPath`; 94:normalizeReloadPath — `*mcpLockedFiles.bindMCPPath`; 101:normalizeDiscoveryPath — `*mcpLockedFiles.mcpRecord`; 105:normalizeReloadPath — `*mcpLockedFiles.mcpRecord`; 117:normalizeDiscoveryPath — `*mcpLockedFiles.mcpData`; 121:normalizeDiscoveryPath — `*mcpLockedFiles.mcpFingerprint`; 136:normalizeDiscoveryPath — `*mcpLockedFiles.setMCPData`; 160:normalizeReloadPath — `*mcpLockedFiles.validateMutableTopology`; 273:normalizeDiscoveryPath — `*ConfigStore.withMCPLocksUsingCaseSensitivity`; 275:normalizeDiscoveryPath — `*ConfigStore.withMCPLocksUsingCaseSensitivity`; 327:readStableConfigFileOwned — `*ConfigStore.withMCPLocksUsingCaseSensitivity`; 328:normalizeDiscoveryPath — `*ConfigStore.withMCPLocksUsingCaseSensitivity`; 329:normalizeDiscoveryPath — `*ConfigStore.withMCPLocksUsingCaseSensitivity`; 347:decodeConfigDocument — `*ConfigStore.withMCPLocksUsingCaseSensitivity` |
| `docread` | `store_mcp_transaction.go` | 50 | 38:normalizeReloadPath — `*ConfigStore.mutatePendingRemoveMCPWithAuthority`; 104:normalizeReloadPath — `*ConfigStore.mutateMCPEnableRollbackWithAuthority`; 192:normalizeDiscoveryPath — `mcpEvaluationFingerprint`; 194:normalizeDiscoveryPath — `mcpEvaluationFingerprint`; 208:normalizeReloadPath — `*ConfigStore.mutateMCPWithMode`; 351:encodeSettingsDocument — `*ConfigStore.writeMCPFileChanges`; 351:settingsDocumentFormatOf — `*ConfigStore.writeMCPFileChanges`; 357:configDiscoveryFingerprint — `*ConfigStore.writeMCPFileChanges`; 363:configDiscoveryFingerprint — `*ConfigStore.writeMCPFileChanges`; 462:readStableConfigFileOwned — `*ConfigStore.reconcileMCPCommit`; 463:sameBytesFingerprint — `*ConfigStore.reconcileMCPCommit`; 472:normalizeDiscoveryPath — `*ConfigStore.applyCommittedMCPFingerprint`; 474:normalizeDiscoveryPath — `*ConfigStore.applyCommittedMCPFingerprint`; 475:normalizeDiscoveryPath — `*ConfigStore.applyCommittedMCPFingerprint`; 483:readStableConfigFileOwned — `*ConfigStore.applyCommittedMCPFingerprint`; 498:mcpJSONCandidatePaths — `*ConfigStore.evaluateMCPFiles`; 570:loadMCPJSONBytes — `*ConfigStore.evaluateMCPFiles`; 580:normalizeReloadPath — `*ConfigStore.evaluateMCPFiles`; 595:eligibleConfigCandidate — `*ConfigStore.orderedMCPPaths`; 612:lookupConfigCandidates — `*ConfigStore.orderedMCPPaths`; 614:configOwnerForWorkingDir — `*ConfigStore.orderedMCPPaths`; 614:eligibleConfigCandidate — `*ConfigStore.orderedMCPPaths`; 629:normalizeDiscoveryPath — `*ConfigStore.mcpPathData`; 633:rebindReloadFingerprintPath — `*ConfigStore.mcpPathData`; 644:readStableConfigFileOwned — `*ConfigStore.mcpPathData`; 658:decodeConfigDocument — `*ConfigStore.mcpPathData`; 669:normalizeDiscoveryPath — `*ConfigStore.mcpOwnerPolicy`; 670:normalizeDiscoveryPath — `*ConfigStore.mcpOwnerPolicy`; 676:configOwnerForWorkingDir — `*ConfigStore.mcpOwnerPolicy`; 684:normalizeReloadPath — `*ConfigStore.mcpOwnerPolicy`; 685:normalizeReloadPath — `*ConfigStore.mcpOwnerPolicy`; 688:matchesLogicalSlot — `*ConfigStore.mcpOwnerPolicy`; 691:normalizeReloadPath — `*ConfigStore.mcpOwnerPolicy`; 696:configOwnerForWorkingDir — `*ConfigStore.mcpOwnerPolicy`; 706:normalizeReloadPath — `entryOrigins`; 707:normalizeReloadPath — `entryOrigins`; 708:normalizeReloadPath — `entryOrigins`; 709:normalizeReloadPath — `entryOrigins`; 718:isMCPDisabledOnlyEntry — `entryOrigins`; 728:matchesLogicalSlot — `entryOrigins`; 730:matchesLogicalSlot — `entryOrigins`; 732:matchesLogicalSlot — `entryOrigins`; 763:mcpEntryFromJSON — `externalOverlayValue`; 764:isMCPDisabledOnlyEntry — `externalOverlayValue`; 782:normalizeReloadPath — `*ConfigStore.verifyMCPReadOnlyInputs`; 789:readStableConfigFileOwned — `*ConfigStore.verifyMCPReadOnlyInputs`; 873:normalizeReloadPath — `*ConfigStore.publishMCPMutationLocked`; 877:readReloadFingerprint — `*ConfigStore.publishMCPMutationLocked`; 911:normalizeDiscoveryPath — `uniqueMCPPaths`; 923:readReloadFingerprint — `dataFingerprint` |
| `docread` | `store_oauth.go` | 6 | 383:checkBackupOnlySlot — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 383:legacySlotOf — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 395:readStableConfigFileOwned — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 399:decodeSettingsDocument — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 428:encodeSettingsDocument — `*ConfigStore.setOAuthTokenIfCurrentWithLock`; 526:readSettingsDocument — `*ConfigStore.loadTokenFromDisk` |
| `docread` | `store_oauth_disk_sync.go` | 3 | 82:readSlotOf — `*ConfigStore.syncOAuthFromDisk`; 93:readSettingsDocument — `*ConfigStore.syncOAuthFromDisk`; 105:decodeOAuthJournalDiskToken — `*ConfigStore.syncOAuthFromDisk` |
| `docread` | `store_oauth_journal.go` | 11 | 93:normalizeReloadPath — `oauthJournalPath`; 106:decodeConfigDocument — `readOAuthJournal`; 112:validOAuthJournal — `readOAuthJournal`; 142:validOAuthJournal — `writeOAuthJournalOnce`; 149:encodeSettingsDocument — `writeOAuthJournalOnce`; 149:settingsDocumentFormatOf — `writeOAuthJournalOnce`; 165:readSettingsDocument — `readOAuthJournalDiskToken`; 169:decodeOAuthJournalDiskToken — `readOAuthJournalDiskToken`; 205:readSlotOf — `verifyOAuthJournalConfigDurable`; 215:decodeConfigDocument — `verifyOAuthJournalConfigDurable`; 219:decodeOAuthJournalDiskToken — `verifyOAuthJournalConfigDurable` |
| `docread` | `store_oauth_journal_outbox.go` | 7 | 40:normalizeReloadPath — `*ConfigStore.installOAuthOutbox`; 50:normalizeReloadPath — `*ConfigStore.installOAuthOutbox`; 57:normalizeReloadPath — `*ConfigStore.oauthOutboxCurrent`; 63:normalizeReloadPath — `*ConfigStore.clearOAuthOutbox`; 96:normalizeReloadPath — `*ConfigStore.settleOAuthOutbox`; 134:decodeConfigDocument — `verifyOAuthOutboxJournal`; 229:normalizeReloadPath — `logOAuthJournalFailureAt` |
| `docread` | `store_oauth_journal_worker.go` | 6 | 33:configWriteLockAnchor — `oauthRefreshLockPath`; 33:normalizeReloadPath — `oauthRefreshLockPath`; 235:normalizeReloadPath — `*ConfigStore.withOAuthJournalRecoveryWriteLock`; 236:configWriteLockAnchor — `*ConfigStore.withOAuthJournalRecoveryWriteLock`; 236:normalizeReloadPath — `*ConfigStore.withOAuthJournalRecoveryWriteLock`; 242:normalizeReloadPath — `*ConfigStore.withOAuthJournalRecoveryWriteLock` |
| `docread` | `store_oauth_refresh.go` | 1 | 82:normalizeReloadPath — `*ConfigStore.codexOAuthAuthorityRecord` |
| `docread` | `store_reload.go` | 20 | 191:lookupConfigCandidates — `*ConfigStore.buildAndPublishReload`; 194:checkAuthoritativeBackupOnly — `*ConfigStore.buildAndPublishReload`; 197:mcpJSONCandidatePaths — `*ConfigStore.buildAndPublishReload`; 215:normalizeDiscoveryPath — `*ConfigStore.buildAndPublishReload`; 215:workspaceConfigPath — `*ConfigStore.buildAndPublishReload`; 216:normalizeReloadPath — `*ConfigStore.buildAndPublishReload`; 217:checkBackupOnlySlot — `*ConfigStore.buildAndPublishReload`; 220:pathAlreadyLoaded — `*ConfigStore.buildAndPublishReload`; 221:configOwnerForWorkingDir — `*ConfigStore.buildAndPublishReload`; 225:readStableConfigFileOwned — `*ConfigStore.buildAndPublishReload`; 228:decodeConfigDocument — `*ConfigStore.buildAndPublishReload`; 261:normalizeReloadPath — `*ConfigStore.buildAndPublishReload`; 280:loadExternalMCPDocumentsStableForWorkingDir — `*ConfigStore.buildAndPublishReload`; 284:mergeExternalMCPServersFromDocuments — `*ConfigStore.buildAndPublishReload`; 362:configAndMCPStalenessPaths — `*ConfigStore.buildAndPublishReload`; 362:lookupConfigCandidates — `*ConfigStore.buildAndPublishReload`; 364:reloadStalenessState — `*ConfigStore.buildAndPublishReload`; 441:normalizeReloadPath — `stableConfigDocumentForPath`; 443:matchesLogicalSlot — `stableConfigDocumentForPath`; 443:normalizeReloadPath — `stableConfigDocumentForPath` |
| `docread` | `store_staleness.go` | 6 | 33:trackedSlotFingerprint — `*ConfigStore.ConfigStaleness`; 101:readReloadFingerprint — `*ConfigStore.refreshStalenessSnapshotLocked`; 132:normalizeDiscoveryPath — `*ConfigStore.captureStalenessSnapshotLocked`; 139:normalizeDiscoveryPath — `*ConfigStore.captureStalenessSnapshotLocked`; 142:normalizeDiscoveryPath — `*ConfigStore.captureStalenessSnapshotLocked`; 149:expandConfigSlotSiblings — `*ConfigStore.captureStalenessSnapshotLocked` |
| `docread` | `store_write.go` | 23 | 60:readSettingsDocument — `*ConfigStore.HasConfigField`; 165:readStableConfigFileOwned — `*ConfigStore.resolveConfigWriteTarget`; 169:normalizeReloadPath — `*ConfigStore.resolveConfigWriteTarget`; 180:configWriteLockAnchor — `*ConfigStore.resolveConfigWriteTarget`; 180:normalizeDiscoveryPath — `*ConfigStore.resolveConfigWriteTarget`; 180:normalizeReloadPath — `*ConfigStore.resolveConfigWriteTarget`; 193:readStableConfigFileOwned — `verifyConfigTarget`; 208:normalizeReloadPath — `verifyConfigTargetBinding`; 225:normalizeReloadPath — `verifyConfigTargetBinding`; 235:normalizeReloadPath — `verifyConfigTargetBinding`; 282:legacySlotOf — `*ConfigStore.withConfigWriteLockCtx`; 283:ktavEntryExists — `*ConfigStore.withConfigWriteLockCtx`; 283:migrationRecordPathFor — `*ConfigStore.withConfigWriteLockCtx`; 335:normalizeReloadPath — `*ConfigStore.noteInitialLoadWriteLocked`; 338:normalizeReloadPath — `*ConfigStore.noteInitialLoadWriteLocked`; 421:readStableConfigFileOwned — `*ConfigStore.setConfigFieldsAuthority`; 427:settingsDocumentFormatOf — `*ConfigStore.setConfigFieldsAuthority`; 429:decodeSettingsDocument — `*ConfigStore.setConfigFieldsAuthority`; 475:encodeSettingsDocument — `*ConfigStore.setConfigFieldsAuthority`; 480:readStableConfigFileOwned — `*ConfigStore.setConfigFieldsAuthority`; 633:readStableConfigFileOwned — `*ConfigStore.removeConfigFieldAt`; 637:decodeSettingsDocument — `*ConfigStore.removeConfigFieldAt`; 667:encodeSettingsDocument — `*ConfigStore.removeConfigFieldAt` |
| **docread total** | — | **278** | — |
| `fileid` | `apitoken_store.go` | 5 | 368:configFileOwner — `*APITokenStore.read`; 369:homeConfigOwner — `*APITokenStore.read`; 375:configFileIdentityOf — `*APITokenStore.read`; 377:configFileIdentityAtPath — `*APITokenStore.read`; 396:homeConfigOwner — `*APITokenStore.read` |
| `fileid` | `io/cfgcommit/config_write_identity.go` | 2 | 20:configFileIdentityAtPath — `configWriteTargetDedupKeyWithCaseFold`; 26:configParentIdentity — `configWriteTargetDedupKeyWithCaseFold` |
| `fileid` | `io/docread/fingerprints.go` | 1 | 172:homeConfigOwner — `candidateInputsChanged` |
| `fileid` | `server_listeners.go` | 3 | 143:systemConfigOwner — `GlobalServerListenersWithSources`; 144:homeConfigOwner — `GlobalServerListenersWithSources`; 145:homeConfigOwner — `GlobalServerListenersWithSources` |
| `fileid` | `store_mcp_admission.go` | 17 | 321:openStableConfigFile — `*ConfigStore.prepareMCPAdmissionToken`; 376:configParentIdentity — `mcpAdmissionAbsenceFingerprint`; 395:configFileOwner — `validatePreparedAdmissionFile`; 399:readOpenedConfigBytes — `validatePreparedAdmissionFile`; 406:readOpenedConfigBytes — `validatePreparedAdmissionFile`; 420:configFileOwner — `validatePreparedAdmissionFile`; 421:configFileNlinkOfOpened — `validatePreparedAdmissionFile`; 428:configParentIdentity — `validatePreparedAdmissionFile`; 432:configFilePathIdentityMatches — `validatePreparedAdmissionFile`; 536:configFileOwner — `validateFinalMCPAdmissionFile`; 540:configFileIdentityOfOpened — `validateFinalMCPAdmissionFile`; 544:configFileNlinkOfOpened — `validateFinalMCPAdmissionFile`; 560:configFileOwner — `validateFinalMCPAdmissionFile`; 561:configFileIdentityOfOpened — `validateFinalMCPAdmissionFile`; 562:configFileNlinkOfOpened — `validateFinalMCPAdmissionFile`; 569:configParentIdentity — `validateFinalMCPAdmissionFile`; 573:configFilePathIdentityMatches — `validateFinalMCPAdmissionFile` |
| `fileid` | `store_mcp_transaction.go` | 7 | 600:systemConfigOwner — `*ConfigStore.orderedMCPPaths`; 607:homeConfigOwner — `*ConfigStore.orderedMCPPaths`; 608:homeConfigOwner — `*ConfigStore.orderedMCPPaths`; 672:homeConfigOwner — `*ConfigStore.mcpOwnerPolicy`; 686:systemConfigOwner — `*ConfigStore.mcpOwnerPolicy`; 689:homeConfigOwner — `*ConfigStore.mcpOwnerPolicy`; 692:homeConfigOwner — `*ConfigStore.mcpOwnerPolicy` |
| **fileid total** | — | **35** | — |
| `fswap` | `io/cfgcommit/config_write_identity.go` | 2 | 18:configWriteIdentityKey — `configWriteTargetDedupKeyWithCaseFold`; 21:configWriteIdentityKey — `configWriteTargetDedupKeyWithCaseFold` |
| `fswap` | `store_mcp_transaction.go` | 1 | 812:configEquivalentLeafNames — `configEquivalentMissingMCPInput` |
| `fswap` | `store_oauth_journal.go` | 1 | 272:syncConfigParent — `removeOAuthJournal` |
| `fswap` | `store_oauth_journal_outbox.go` | 1 | 140:syncConfigParent — `verifyOAuthOutboxJournal` |
| `fswap` | `store_oauth_journal_quarantine.go` | 2 | 52:syncConfigParent — `quarantineOAuthJournalWithLogging`; 111:syncConfigParent — `pruneOAuthJournalQuarantines` |
| **fswap total** | — | **7** | — |
| `slotmigrate` | `io/slotmigrate/ktav_migration_artifacts.go` | 1 | 454:migrateKtavArtifactToWithLock — `ktavMigrateOAuthJournal` |
| `slotmigrate` | `store_write.go` | 1 | 284:runKtavMigrationStateLocked — `*ConfigStore.withConfigWriteLockCtx` |
| **slotmigrate total** | — | **2** | — |
| `slots` | `apitoken_store.go` | 1 | 462:newConfigFileIsKtav — `*APITokenStore.selectedPath` |
| `slots` | `load.go` | 2 | 116:ktavMigrateStartupSlots — `loadOnce`; 149:ktavMigrateWorkspaceIfNeeded — `loadOnce` |
| `slots` | `model_catalog_cache.go` | 2 | 52:readCacheDocument — `readModelCatalog`; 86:writeCacheDocument — `cachedProviderModels` |
| `slots` | `model_verify.go` | 4 | 177:writeCacheDocument — `writeConfirmedModels`; 181:readCacheDocument — `readConfirmedModels`; 205:readCacheDocument — `CachedConfirmedModels`; 218:readCacheDocument — `readFreshConfirmedModels` |
| `slots` | `provider.go` | 2 | 289:readCacheDocument — `cache.Get`; 312:writeCacheDocument — `cache.Store` |
| `slots` | `provider_peak_hours_read.go` | 1 | 27:writeTargetSlot — `*ConfigStore.PeakHoursEntryForScope` |
| `slots` | `settings_lock.go` | 2 | 432:writeTargetSlot — `*ConfigStore.LockSettings`; 453:writeTargetSlot — `*ConfigStore.UnlockSettings` |
| `slots` | `store_mcp_transaction.go` | 1 | 607:ActiveConfigFileName — `*ConfigStore.orderedMCPPaths` |
| `slots` | `store_models.go` | 1 | 51:ReadConfigDocumentLight — `*ConfigStore.ReadAllModelsAtScope` |
| `slots` | `store_oauth.go` | 1 | 386:writeTargetSlot — `*ConfigStore.setOAuthTokenIfCurrentWithLock` |
| `slots` | `store_oauth_journal_outbox.go` | 1 | 167:selectOAuthJournalPath — `*ConfigStore.flushOAuthOutbox` |
| `slots` | `store_oauth_journal_worker.go` | 3 | 82:selectOAuthJournalPath — `*ConfigStore.recoverCodexOAuthJournal`; 259:selectOAuthJournalPath — `*ConfigStore.recoverOAuthJournalsAtLoad`; 386:selectOAuthJournalPath — `*ConfigStore.oauthJournalWorkerAttempt` |
| `slots` | `store_oauth_refresh.go` | 3 | 70:selectOAuthJournalPath — `*ConfigStore.codexRefreshAlreadyCurrent`; 134:selectOAuthJournalPath — `*ConfigStore.adoptCodexDiskAuthority`; 311:selectOAuthJournalPath — `*ConfigStore.refreshCodexOAuthLeader` |
| `slots` | `store_reload.go` | 2 | 119:ktavMigrateStartupSlots — `*ConfigStore.runReloadLocked`; 129:ktavMigrateWorkspaceIfNeeded — `*ConfigStore.runReloadLocked` |
| `slots` | `store_write.go` | 4 | 265:writeTargetSlot — `*ConfigStore.withConfigWriteLockCtx`; 288:writeTargetSlot — `*ConfigStore.withConfigWriteLockCtx`; 403:writeTargetSlot — `*ConfigStore.setConfigFieldsAuthority`; 599:writeTargetSlot — `*ConfigStore.removeConfigFieldAt` |
| **slots total** | — | **30** | — |
| **Grand total** | — | **409** | **Cross-check: casefold=1, cfgcommit=26, cfghooks=30, docread=278, fileid=35, fswap=7, slotmigrate=2, slots=30; expected 409.** |

Projection call-count cross-check: 409 distinct ROOT production locations; each total agrees with the projection edge call count. Same source location/name repeated calls are a known undercount, not separate AST call multiplicity.

### E. ROOT test-file references to IO symbols (no tests moved)

Every ROOT `_test.go` file with a ledger reference whose `TargetFiles` includes an `io/` source is included. References to `configTestHooks`, root-kept composites, root-owned hook variables, and `ktavAutoMigrate` physical storage are excluded. `references` counts ledger occurrences; names are distinct, exact original spellings. This inventory does not move or edit tests.

| ROOT test file | Distinct names | References | Exact names | Handling |
|---|---:|---:|---|---|
| `apitoken_cache_test.go` | 1 | 2 | `SetKtavAutoMigrateForTest` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `apitoken_security_test.go` | 1 | 1 | `SetKtavAutoMigrateForTest` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `apitoken_store_test.go` | 3 | 5 | `SetKtavAutoMigrateForTest`, `encodeSettingsDocument`, `settingsDocumentFormatOf` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `atomicwrite_test.go` | 1 | 2 | `atomicWriteFile` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `catwalk_test.go` | 2 | 2 | `ktavEntryExists`, `ktavSiblingOf` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `config_case_sensitivity_behavioral_unix_test.go` | 1 | 1 | `configDirectoryCaseSensitivityByEntries` | pure case-fold/OS test: same-package whole-function move with that package |
| `config_case_sensitivity_darwin_test.go` | 4 | 8 | `configCaseSensitivityInsensitive`, `configCaseSensitivitySensitive`, `configCaseSensitivityUnknown`, `configPlatformCaseSensitivity` | pure case-fold/OS test: same-package whole-function move with that package |
| `config_case_sensitivity_linux_test.go` | 1 | 2 | `configPlatformCaseFoldLeaf` | pure case-fold/OS test: same-package whole-function move with that package |
| `config_case_sensitivity_windows_test.go` | 1 | 2 | `configWindowsCaseInsensitiveFromFlags` | pure case-fold/OS test: same-package whole-function move with that package |
| `config_commit_outcome_test.go` | 4 | 4 | `CommitOutcome`, `errConfigCommitCommitted`, `errConfigCommitUncertain`, `normalizeReloadPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_commit_recovery_unix_test.go` | 1 | 2 | `ErrConfigHardLink` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_commit_unix_test.go` | 10 | 19 | `CommitOutcome`, `ErrConfigHardLink`, `commitConfigFile`, `configFileIdentity`, `configFileIdentityOf`, `errConfigCommitDurabilityUncertain`, `errConfigCommitUncertain`, `errConfigCommitVerification`, `normalizeReloadPath`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_commit_windows_mcp_test.go` | 4 | 4 | `CommitOutcome`, `errAtomicWriteCommitted`, `errConfigCommitDurabilityUncertain`, `normalizeReloadPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_commit_windows_test.go` | 6 | 20 | `CommitOutcome`, `ErrConfigHardLink`, `commitConfigFile`, `errConfigCommitVerification`, `readStableConfigFile`, `windowsFileRenameInfo` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_cycle7_acceptance_test.go` | 2 | 2 | `normalizeReloadPath`, `systemConfigOwner` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_cycle7_test.go` | 8 | 17 | `ErrConfigReloadUnstable`, `mcpEntryFromJSON`, `normalizeReloadPath`, `openStableConfigFile`, `readOpenedConfigBytes`, `readStableConfigFile`, `reloadFileFingerprint`, `writeStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_cycle9_test.go` | 2 | 4 | `mergeExternalMCPServersFromDocuments`, `stableConfigDocument` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `config_durability_unix_test.go` | 3 | 3 | `errConfigCommitDurabilityUncertain`, `normalizeDiscoveryPath`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_regression_test.go` | 8 | 11 | `CommitOutcomeFromError`, `configFileIdentity`, `errConfigCommitCommitted`, `errConfigCommitUncertain`, `errConfigCommitVerification`, `normalizeDiscoveryPath`, `normalizeReloadPath`, `reloadFileFingerprint` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_rename_darwin_test.go` | 1 | 6 | `renameConfigTempAt` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `config_rename_unix_test.go` | 7 | 12 | `CommitOutcome`, `ErrConfigHardLink`, `commitConfigFile`, `configFileNlinkOfOpened`, `errConfigCommitDurabilityUncertain`, `readStableConfigFile`, `renameConfigTempAt` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_rename_windows_test.go` | 16 | 49 | `CommitOutcome`, `commitConfigFile`, `deleteWindowsConfigHandle`, `errAtomicWriteCommitted`, `errConfigCommitCommitted`, `errConfigCommitDurabilityUncertain`, `errConfigCommitVerification`, `openWindowsConfigHandle`, `readStableConfigFile`, `reloadFileFingerprint`, `renameConfigTemp`, `renameConfigTempHandle`, `stageConfigFileHandle`, `syncConfigParentOnDisk`, `windowsConfigAPIPath`, `windowsFileRenameInfo` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `config_renameat2_linux_test.go` | 1 | 2 | `renameConfigTempAt` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `config_sidecar_identity_test.go` | 30 | 147 | `acquireConfigFileLock`, `commitConfigFile`, `configAliasChainFingerprint`, `configCaseDirectoryBudgetExceeded`, `configCaseDirectoryEntry`, `configCaseDirectoryMaxEntries`, `configCaseDirectoryMaxNameBytes`, `configCaseDirectorySnapshot`, `configCaseSensitivity`, `configCaseSensitivityFromDirectorySnapshots`, `configCaseSensitivityFromMetadata`, `configCaseSensitivityInsensitive`, `configCaseSensitivitySensitive`, `configCaseSensitivityUnknown`, `configDarwinCaseSensitivityFromPathconf`, `configDirectoryIsCaseInsensitive`, `configFileIdentity`, `configFoldCaseLeaf`, `configFreeBSDCaseSensitivityFromFilesystemName`, `configLinuxCaseSensitivityFromMetadata`, `configLinuxExFATSuperMagic`, `configLinuxExt4SuperMagic`, `configLinuxF2FSSuperMagic`, `configLinuxFSCasefoldFlag`, `configLinuxMSDOSSuperMagic`, `configParentIdentity`, `errConfigCaseDirectoryBudgetExceeded`, `normalizeReloadPath`, `readStableConfigFileOwned`, `reloadFileFingerprint` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `custom_roles_integrity_test.go` | 3 | 3 | `applyKtavMigrationModelsIntegrity`, `decodeSettingsDocument`, `reloadFileFingerprint` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `custom_roles_write_test.go` | 3 | 4 | `encodeSettingsDocument`, `readSettingsDocument`, `settingsDocumentFormatOf` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `docker_mcp_test.go` | 1 | 4 | `normalizeReloadPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `global_dir_isolation_test.go` | 4 | 7 | `normalizeReloadPath`, `oauthJournalDigest`, `oauthJournalRecord`, `selectOAuthJournalPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `hyper_test.go` | 2 | 2 | `ktavEntryExists`, `ktavSiblingOf` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_both_formats_test.go` | 2 | 6 | `readCacheDocument`, `selectConfigSlot` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_cache_test.go` | 4 | 11 | `atomicWriteFile`, `ktavSiblingOf`, `readCacheDocument`, `writeCacheDocument` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_discovery_test.go` | 12 | 44 | `backupPathFor`, `checkAuthoritativeBackupOnly`, `checkBackupOnlySlot`, `configOwnerPolicyForPath`, `eligibleConfigCandidate`, `errKtavBackupOnly`, `ktavBothFormatsWarned`, `ktavSiblingOf`, `lookupConfigCandidates`, `normalizeDiscoveryPath`, `selectConfigSlot`, `workspaceConfigPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_light_test.go` | 6 | 20 | `ActiveConfigFileName`, `KtavAutoMigrateEnabled`, `ReadConfigDocumentLight`, `SetKtavAutoMigrateForTest`, `errKtavBackupOnly`, `ktavSiblingOf` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_lock_anchor_test.go` | 1 | 1 | `configWriteLockAnchor` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_migration_artifacts_test.go` | 15 | 103 | `acquireConfigFileLock`, `backupPathFor`, `errKtavMigrationConflict`, `errKtavMigrationCrashInjected`, `fileExists`, `ktavMigrateCacheArtifactsStartup`, `ktavSiblingOf`, `migrateKtavArtifact`, `migrationRecordPathFor`, `normalizeReloadPath`, `oauthJournalKtavPath`, `oauthJournalLegacyPath`, `readCacheDocument`, `releaseConfigFileLock`, `selectOAuthJournalPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_migration_load_test.go` | 1 | 2 | `normalizeReloadPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_migration_symlink_test.go` | 9 | 34 | `backupPathFor`, `configParentIdentity`, `ktavMigrationRecord`, `ktavMigrationRecordVersion`, `ktavSiblingOf`, `loadKtavMigrationRecord`, `migrationRecordPathFor`, `readStableConfigFileOwned`, `runKtavMigrationState` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_migration_test.go` | 20 | 78 | `acquireConfigFileLock`, `configFileOwner`, `errConfigOwnerMismatch`, `errKtavBackupOnly`, `errKtavMigrationConflict`, `errKtavMigrationCrashInjected`, `errKtavMigrationRecord`, `fileExists`, `ktavMigrateStartupSlots`, `ktavMigrateWorkspaceIfNeeded`, `ktavSiblingOf`, `loadKtavMigrationRecord`, `lookupConfigCandidates`, `migrateLegacyConfigFile`, `migrationRecordPathFor`, `normalizeDiscoveryPath`, `normalizeReloadPath`, `ownerPolicyForMigration`, `readStableConfigFileOwned`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `ktav_slot_fingerprint_test.go` | 3 | 12 | `ktavConfigExt`, `ktavSiblingOf`, `matchesLogicalSlot` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_slot_sync_test.go` | 1 | 4 | `ktavSiblingOf` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_write_target_test.go` | 4 | 12 | `backupPathFor`, `ktavSiblingOf`, `migrationRecordPathFor`, `writeTargetSlot` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `ktav_write_test.go` | 4 | 9 | `decodeConfigDocument`, `errConfigCommitVerification`, `newConfigFileIsKtav`, `rejectUnconvertedKtavWrite` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `load_discovery_owner_unix_test.go` | 3 | 3 | `eligibleWorkspaceConfig`, `lookupConfigCandidates`, `mcpJSONCandidatePaths` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `load_discovery_test.go` | 3 | 3 | `lookupConfigCandidates`, `normalizeReloadPath`, `pathAlreadyLoaded` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `mcp_admission_final_verify_test.go` | 2 | 4 | `openStableConfigFile`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `mcp_admission_io_test.go` | 4 | 5 | `normalizeDiscoveryPath`, `normalizeReloadPath`, `openStableConfigFile`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `mcp_admission_io_unix_test.go` | 2 | 2 | `ErrConfigNonRegular`, `readStableConfigFileOwned` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `mcp_revision_diagnostic_test.go` | 3 | 5 | `ActiveConfigFileName`, `SetKtavAutoMigrateForTest`, `ktavSiblingOf` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `model_catalog_cache_test.go` | 1 | 3 | `atomicWriteFile` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `model_metadata_golden_test.go` | 1 | 1 | `atomicWriteFile` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `recent_models_test.go` | 2 | 3 | `decodeConfigDocument`, `selectConfigSlot` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `settings_models_integrity_test.go` | 1 | 3 | `applyKtavMigrationModelsIntegrity` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_config_disk_sync_boundary_test.go` | 1 | 2 | `normalizeDiscoveryPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_config_disk_sync_test.go` | 8 | 21 | `decodeConfigDocument`, `fileSnapshot`, `isKtavConfigPath`, `normalizeDiscoveryPath`, `normalizeReloadPath`, `reloadFileFingerprint`, `reloadStalenessState`, `selectConfigSlot` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_crossprocess_ktav_test.go` | 2 | 6 | `configWriteLockAnchor`, `normalizeReloadPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_admission_config_slot_test.go` | 1 | 1 | `SetKtavAutoMigrateForTest` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_admission_kept_legacy_test.go` | 2 | 2 | `SetKtavAutoMigrateForTest`, `normalizeDiscoveryPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_admission_sibling_fence_test.go` | 3 | 3 | `ktavSiblingOf`, `normalizeDiscoveryPath`, `reloadFileFingerprint` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_format_test.go` | 1 | 1 | `reloadFileFingerprint` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_mcp_identity_unix_test.go` | 6 | 10 | `eligibleConfigCandidate`, `errConfigOwnerMismatch`, `mcpJSONCandidatePaths`, `normalizeDiscoveryPath`, `readStableConfigFile`, `readStableConfigFileOwned` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_ktav_test.go` | 4 | 17 | `acquireConfigFileLock`, `configWriteLockAnchor`, `mcpEntryFromJSON`, `normalizeReloadPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_mcp_test.go` | 5 | 21 | `mcpEntryFromJSON`, `normalizeDiscoveryPath`, `normalizeReloadPath`, `readStableConfigFile`, `reloadFileFingerprint` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_models_ktav_test.go` | 1 | 2 | `ktavSiblingOf` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_burned_digest_test.go` | 3 | 4 | `acquireConfigFileLock`, `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_disk_sync_test.go` | 1 | 3 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_journal_contract_test.go` | 1 | 1 | `atomicWriteFile` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_journal_order_test.go` | 1 | 1 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_journal_outbox_test.go` | 3 | 6 | `normalizeReloadPath`, `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_journal_recovery_test.go` | 4 | 5 | `acquireConfigFileLock`, `atomicWriteFile`, `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_journal_refresh_recovery_test.go` | 1 | 1 | `normalizeReloadPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_journal_test.go` | 1 | 2 | `normalizeReloadPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_journal_testsupport_test.go` | 1 | 1 | `syncConfigParentOnDisk` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_ktav_test.go` | 8 | 35 | `decodeConfigDocument`, `errKtavBackupOnly`, `errOAuthJournalInvalid`, `normalizeReloadPath`, `oauthJournalKtavPath`, `oauthJournalLegacyPath`, `oauthJournalRecord`, `selectOAuthJournalPath` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_matrix_test.go` | 1 | 2 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_outbox_regress_test.go` | 2 | 2 | `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_predecessor_authority_test.go` | 2 | 2 | `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_refresh_pending_test.go` | 3 | 3 | `acquireConfigFileLock`, `normalizeReloadPath`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_task1292_authority_test.go` | 4 | 11 | `acquireConfigFileLock`, `oauthJournalRecord`, `releaseConfigFileLock`, `validOAuthJournal` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_task1292_engine_test.go` | 3 | 3 | `acquireConfigFileLock`, `oauthJournalRecord`, `releaseConfigFileLock` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_oauth_task1292_legacy_test.go` | 2 | 2 | `acquireConfigFileLock`, `releaseConfigFileLock` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_task1296_digestless_test.go` | 1 | 2 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_task1300_adoption_test.go` | 1 | 3 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_oauth_test.go` | 1 | 1 | `CommitOutcome` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_reload_consistency_test.go` | 1 | 1 | `ErrConfigReloadUnstable` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_reload_round13_test.go` | 3 | 3 | `commitConfigFile`, `normalizeReloadPath`, `readStableConfigFileOwned` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_reload_test.go` | 4 | 5 | `ktavSiblingOf`, `normalizeDiscoveryPath`, `reloadFileFingerprint`, `reloadStalenessState` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_reload_windows_test.go` | 2 | 3 | `configFileIdentityAtPath`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_slot_lock_regression_test.go` | 5 | 14 | `errKtavMigrationCrashInjected`, `errKtavMigrationRecord`, `ktavSiblingOf`, `migrationRecordPathFor`, `runKtavMigrationState` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_write_durability_test.go` | 4 | 8 | `atomicWriteFile`, `errAtomicWriteCommitted`, `normalizeDiscoveryPath`, `readStableConfigFile` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_write_lock_timeout_test.go` | 1 | 2 | `normalizeReloadPath` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `store_write_reload_fixture_test.go` | 1 | 1 | `ActiveConfigFileName` | mixed/root-facing test: keep uppercase/root assertions in ROOT; use narrow accessors or byte-copy fixture for lower internals |
| `store_write_test.go` | 2 | 3 | `normalizeDiscoveryPath`, `reloadFileFingerprint` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |
| `task1314_oauth_disk_sync_test.go` | 1 | 1 | `oauthJournalRecord` | same-package test references; keep test in ROOT for now, consider whole-function move only with package-owned test |

Files represented: 93; no test moves were performed. `configTestHooks` and the five migration hook fields remain root/provider state; tests are not made package dependencies as part of this projection.

### F. Reproducible AST/projection generator sources

The following are the complete checked-in analysis artifacts as read for this appendix, followed by an executable in-memory correction wrapper. The wrapper runs the actual parent `project.py` with `__file__` set to that artifact; it does not replace or edit `project.py`. Its corrections are ownership of every `config_linkat_unix.go` declaration (except `readConfigEntryAt`→`docread` and `configFileNlinkOfInfo`→`fileid`), `physicalWindowsConfigPath`→`fileid`, `newConfigFileIsKtav`/`writeTargetSlot`→`slots`, `errReloadDiskChanged` and `ktavAutoMigrate`→`ROOT`, per-line minimal-span source ownership, test-reference exclusion from production edges, and exact self-declaration-line filtering.

Run from the repository root only when regenerating the analysis artifacts:

```text
go run .rush/tmp/io-design/astledger.go
python .rush/tmp/io-design/corrected_project.py
```

The first command is an AST-ledger analysis helper, not a package test/build. The second command regenerates the two ignored projection outputs. Do not run `project.py` bare when the correction wrapper is the intended graph. Neither command was run by this document generator.

#### `.rush/tmp/io-design/astledger.go` — complete source

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Decl struct {
 File string `json:"File"`; Start int `json:"Start"`; End int `json:"End"`; DocStart int `json:"DocStart"`
 Name string `json:"Name"`; Kind string `json:"Kind"`; Receiver string `json:"Receiver"`; Names []string `json:"Names"`
 Snippet string `json:"Snippet"`
}
type Ref struct { File string `json:"File"`; Line int `json:"Line"`; Name string `json:"Name"`; EnclosingDecl string `json:"EnclosingDecl"`; TargetFiles []string `json:"TargetFiles"`; Call bool `json:"Call"` }
type ImportRef struct { File string `json:"File"`; ImportPath string `json:"ImportPath"`; Sel string `json:"Sel"`; Line int `json:"Line"`; EnclosingDecl string `json:"EnclosingDecl"` }
type Ledger struct { Files []string `json:"Files"`; Declarations []Decl `json:"Declarations"`; References []Ref `json:"References"`; Imports []ImportRef `json:"Imports"` }
type source struct { path, rel string; f *ast.File; src []byte }
type global struct { file string; pos token.Pos }
func main(){
 root:="internal/config"; abs,e:=filepath.Abs(root); must(e); ioabs:=filepath.Join(abs,"io")
 var paths []string
 es,e:=os.ReadDir(abs); must(e); for _,x:=range es {if !x.IsDir()&&strings.HasSuffix(x.Name(),".go"){paths=append(paths,filepath.Join(abs,x.Name()))}}
 must(filepath.WalkDir(ioabs,func(p string,d os.DirEntry,e error)error{if e!=nil{return e};if !d.IsDir()&&strings.HasSuffix(d.Name(),".go"){paths=append(paths,p)};return nil}))
 sort.Strings(paths); fs:=token.NewFileSet(); var ss []source; globals:=map[string][]global{}; decls:=[]Decl{}; methods:=map[*ast.FuncDecl]string{}
 for _,p:=range paths {src,e:=os.ReadFile(p);must(e); f,e:=parser.ParseFile(fs,p,src,parser.ParseComments|parser.AllErrors);if e!=nil{panic(fmt.Errorf("parse %s: %w",p,e))}; rel:=slash(mustRel(abs,p));s:=source{p,rel,f,src};ss=append(ss,s)
  for _,d:=range f.Decls {switch x:=d.(type){case *ast.FuncDecl:
   if x.Recv==nil {name:=x.Name.Name;globals[name]=append(globals[name],global{rel,x.Name.Pos()});decls=append(decls,makeDecl(fs,s,x,x.Doc,name,"func","",[]string{name}))} else {recv:="?";if len(x.Recv.List)>0{recv=exprName(x.Recv.List[0].Type)};methods[x]=recv;decls=append(decls,makeDecl(fs,s,x,x.Doc,recv+"."+x.Name.Name,"method",recv,[]string{x.Name.Name}))}
  case *ast.GenDecl: for _,sp:=range x.Specs {switch y:=sp.(type){case *ast.TypeSpec: doc:=y.Doc;if doc==nil{doc=x.Doc}; globals[y.Name.Name]=append(globals[y.Name.Name],global{rel,y.Name.Pos()});decls=append(decls,makeDecl(fs,s,y,doc,y.Name.Name,"type","",[]string{y.Name.Name}));case *ast.ValueSpec: doc:=y.Doc;if doc==nil{doc=x.Doc};for _,n:=range y.Names{if n.Name=="_"{continue};globals[n.Name]=append(globals[n.Name],global{rel,n.Pos()});decls=append(decls,makeDecl(fs,s,y,doc,n.Name,strings.ToLower(x.Tok.String()),"",[]string{n.Name}))}}}
  }}
 }
 ledger:=Ledger{Files:[]string{},Declarations:decls,References:[]Ref{},Imports:[]ImportRef{}};for _,s:=range ss{ledger.Files=append(ledger.Files,s.rel)}
 for _,s:=range ss {parents:=map[ast.Node]ast.Node{};ast.Inspect(s.f,func(n ast.Node)bool{if n==nil{return false};for _,ch:=range children(n){if ch!=nil{parents[ch]=n}};return true})
  imports:=map[string]string{};for _,im:=range s.f.Imports{v:=strings.Trim(im.Path.Value,"\"");alias:=filepath.Base(v);if im.Name!=nil{alias=im.Name.Name};imports[alias]=v}
  enclosing:=func(n ast.Node)string{for p:=n;p!=nil;p=parents[p]{if fn,ok:=p.(*ast.FuncDecl);ok{if fn.Recv!=nil{return methods[fn]+"."+fn.Name.Name};return fn.Name.Name};if gd,ok:=p.(*ast.GenDecl);ok{for _,sp:=range gd.Specs{if ts,ok:=sp.(*ast.TypeSpec);ok{return ts.Name.Name}}};};return ""}
  selectorSel:=map[token.Pos]bool{};selectorX:=map[token.Pos]bool{};call:=map[token.Pos]bool{}
  ast.Inspect(s.f,func(n ast.Node)bool{switch x:=n.(type){case *ast.SelectorExpr:selectorSel[x.Sel.Pos()]=true;if id,ok:=x.X.(*ast.Ident);ok{if ip,yes:=imports[id.Name];yes{selectorX[id.Pos()]=true;ledger.Imports=append(ledger.Imports,ImportRef{s.rel,ip,x.Sel.Name,fs.Position(x.Sel.Pos()).Line,enclosing(x)})}};case *ast.CallExpr:switch z:=x.Fun.(type){case *ast.Ident:call[z.Pos()]=true;case *ast.SelectorExpr:call[z.Sel.Pos()]=true}};return true})
  seen:=map[string]bool{};ast.Inspect(s.f,func(n ast.Node)bool{id,ok:=n.(*ast.Ident);if !ok||id.Name=="_"||id==s.f.Name||selectorSel[id.Pos()]||selectorX[id.Pos()]{return true};var targets []string
   if id.Obj!=nil {for _,gs:=range globals[id.Name]{if gs.pos==id.Obj.Pos(){targets=append(targets,gs.file)}}} else {for _,gs:=range globals[id.Name]{targets=append(targets,gs.file)}}
   if len(targets)>0 {sort.Strings(targets);targets=uniq(targets);pos:=fs.Position(id.Pos());key:=fmt.Sprintf("%s:%d:%s:%s",s.rel,pos.Line,id.Name,enclosing(id));if !seen[key]{ledger.References=append(ledger.References,Ref{s.rel,pos.Line,id.Name,enclosing(id),targets,call[id.Pos()]});seen[key]=true}}
   return true})
 }
 sort.Slice(ledger.Declarations,func(i,j int)bool{a,b:=ledger.Declarations[i],ledger.Declarations[j];if a.File!=b.File{return a.File<b.File};if a.Start!=b.Start{return a.Start<b.Start};return a.Name<b.Name})
 sort.Slice(ledger.References,func(i,j int)bool{a,b:=ledger.References[i],ledger.References[j];if a.File!=b.File{return a.File<b.File};if a.Line!=b.Line{return a.Line<b.Line};return a.Name<b.Name});sort.Slice(ledger.Imports,func(i,j int)bool{a,b:=ledger.Imports[i],ledger.Imports[j];if a.File!=b.File{return a.File<b.File};if a.Line!=b.Line{return a.Line<b.Line};return a.Sel<b.Sel})
 enc,e:=json.MarshalIndent(ledger,"","  ");must(e);must(os.WriteFile(filepath.Join(abs,"../../.rush/tmp/io-design/ledger.json"),append(enc,'\n'),0644));fmt.Printf("files=%d declarations=%d references=%d imports=%d\n",len(ledger.Files),len(ledger.Declarations),len(ledger.References),len(ledger.Imports))
}
func makeDecl(fs *token.FileSet,s source,n ast.Node,doc *ast.CommentGroup,name,kind,recv string,names []string)Decl{start:=fs.Position(n.Pos()).Line;end:=fs.Position(n.End()).Line;ds:=start;if doc!=nil{ds=fs.Position(doc.Pos()).Line};var b bytes.Buffer;_ = format.Node(&b,fs,n);return Decl{s.rel,start,end,ds,name,kind,recv,names,b.String()}}
func children(n ast.Node)[]ast.Node{var out []ast.Node;ast.Inspect(n,func(x ast.Node)bool{if x==nil{return false};if x!=n{out=append(out,x);return false};return true});return out}
func uniq(x []string)[]string{if len(x)<2{return x};o:=x[:1];for _,v:=range x[1:]{if v!=o[len(o)-1]{o=append(o,v)}};return o}
func slash(s string)string{return filepath.ToSlash(s)}
func mustRel(a,b string)string{x,e:=filepath.Rel(a,b);must(e);return x}
func must(e error){if e!=nil{panic(e)}}
func exprName(e ast.Expr)string{switch x:=e.(type){case *ast.Ident:return x.Name;case *ast.StarExpr:return "*"+exprName(x.X);case *ast.IndexExpr:return exprName(x.X);case *ast.IndexListExpr:return exprName(x.X);case *ast.SelectorExpr:return exprName(x.X)+"."+x.Sel.Name};return "?"}
```

#### `.rush/tmp/io-design/project.py` — complete source

```python
#!/usr/bin/env python3
"""Project internal/config's flat-name AST ledger onto proposed IO owners.

Reads ledger.json and source files only. It does not modify Go source or docs.
"""
from __future__ import annotations
import collections
import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
CONFIG = REPO / "internal" / "config"
LEDGER_PATH = HERE / "ledger.json"
OUTPUT_JSON = HERE / "projection.json"
OUTPUT_TEXT = HERE / "projection.txt"

ROOT_TO_DOCREAD = {
    "lookupConfigCandidateGroups", "configCandidateGroups", "configOwnersMatch",
    "configDocumentBytes", "addDocumentAliasFingerprints", "stableReadMaxAttempts",
    "configFingerprintPath", "writeConfigDiscoveryInfo", "captureReloadFingerprints",
    "addReloadFingerprint", "readReloadFingerprint", "reloadFingerprintsChanged",
    "reloadFingerprintsChangedWithOwner", "sameReloadPathSet", "trackedSlotFingerprint",
    "reloadFingerprintForPath", "rebindReloadFingerprintPath",
}
ROOT_TO_CFGMODEL = {
    "canonicalModelsAndRolesJSON", "appName", "defaultDataDirectory", "cacheRevKey",
    "modelsIntegritySignature", "canonicalModelsJSON", "settingsKeyFirstComponent", "hashSalt",
}
DOCREAD_TO_ROOT = {
    "loadConfigCandidateStableForWorkingDir", "candidateInputsChanged", "initialMCPRevisions",
    "mcpInputFingerprints", "mergeExternalMCPServers", "readMCPDisabledOverride",
    "mcpRevisionDiff", "mcpNames",
}
CFGHooks = {
    "runConfigBeforeOpenHook", "runConfigAfterStableReadHook", "runConfigBeforeCommitCheckHook",
    "runConfigAfterCommitRenameHook", "runConfigAfterCommitRenameHookForPath",
    "runConfigAfterMCPCommitHook", "runConfigBeforeCommitRenameHook",
    "runConfigBeforeMCPReconcileHook", "acquireConfigFileLock", "releaseConfigFileLock",
    "configContextWithTimeout",
}
ROOT_CFGCOMMIT = {
    "configTestHooks", "runConfigAfterMCPResolveTargetHook",
    "runConfigBeforeMCPTargetBindingVerificationHook", "SetBeforeConfigCommitForTest",
    "configWriteTargetDedupKey", "configWriteTargetDedupKeyWithCaseFold",
    "configWriteTargetDedupKeyWithCaseSensitivity",
}
MIGRATION_HOOK_VARS = {
    "ktavMigrationStepHook", "ktavArtifactMigrationStepHook", "ktavArtifactStatFile",
    "ktavArtifactChtimes", "ktavMigrationBudget",
}
ROOT_MANUAL_PORTS = {
    "runConfigAfterMCPResolveTargetHook", "runConfigBeforeMCPTargetBindingVerificationHook",
    "SetBeforeConfigCommitForTest", "configWriteTargetDedupKey",
    "configWriteTargetDedupKeyWithCaseFold", "configWriteTargetDedupKeyWithCaseSensitivity",
}
MODEL_HELPERS = {
    "cacheRevKey", "modelsIntegritySignature", "canonicalModelsJSON",
    "settingsKeyFirstComponent", "hashSalt", "appName", "defaultDataDirectory",
    "canonicalModelsAndRolesJSON",
}
FACADE_ALIASES = ("cfgmodel", "envresolve", "datadir", "cfgmerge", "peak")


def original_owner(file: str) -> str:
    if file.startswith("io/"):
        parts = file.split("/")
        return parts[1] if len(parts) > 2 else "ROOT"
    return "ROOT"


def projected_owner(decl: dict) -> str:
    name, file = decl["Name"], decl["File"]
    pkg = original_owner(file)
    if file == "facade.go":
        snippet = decl.get("Snippet", "")
        for alias in FACADE_ALIASES:
            if re.search(r"\b" + re.escape(alias) + r"\s*\.", snippet):
                return alias
    if name in ROOT_TO_DOCREAD and not file.startswith("io/"):
        return "docread"
    if name in ROOT_TO_CFGMODEL and not file.startswith("io/"):
        return "cfgmodel"
    if pkg == "docread" and name in DOCREAD_TO_ROOT:
        return "ROOT"
    if pkg == "cfgcommit":
        if name in ROOT_CFGCOMMIT:
            return "ROOT"
        if name in {"runConfigBeforeOpenHook", "runConfigAfterStableReadHook", "runConfigBeforeCommitCheckHook",
                    "runConfigAfterCommitRenameHook", "runConfigAfterCommitRenameHookForPath",
                    "runConfigAfterMCPCommitHook", "runConfigBeforeCommitRenameHook",
                    "runConfigBeforeMCPReconcileHook", "acquireConfigFileLock",
                    "releaseConfigFileLock", "configContextWithTimeout"}:
            return "cfghooks"
        if name in {"syncConfigParent", "syncConfigParentFile", "syncConfigParentOnDisk",
                    "configWriteIdentityKey", "configDirectoryIsCaseInsensitive",
                    "configEquivalentLeafNames", "otherlinkat"}:
            return "fswap"
        if name == "sameBytesFingerprint":
            return "docread"
        if name in {"errConfigCommitVerification", "ErrConfigHardLink"}:
            return "fileid"
        if name == "readConfigEntryAt":
            return "docread"
    if pkg == "fswap" and name in {"openStableConfigFile", "writeStableConfigFile"}:
        return "fileid"
    if pkg == "fileid" and name == "readStableConfigHandle":
        return "docread"
    if pkg == "slots":
        if file.endswith("/ktav_cache.go") and name in {"readCacheDocument", "writeCacheDocument"}:
            return "slots"
        if file.endswith("/ktav_light.go") and name in {
            "ReadConfigDocumentLight", "ActiveConfigFileName", "SetKtavAutoMigrateForTest",
            "KtavAutoMigrateEnabled",
        }:
            return "slots"
        if file.endswith("/ktav_oauth.go") and name == "selectOAuthJournalPath":
            return "slots"
        if file.endswith("/oauthjournal.go"):
            if name in {"readOAuthJournalWithRetry", "oauthJournalConfigSyncParent"}:
                return "ROOT"
            return "docread"
        return "docread"
    if pkg == "slotmigrate":
        if name in {"ktavAutoMigrate", "ktavMigrateStartupSlots", "ktavMigrateWorkspaceIfNeeded",
                    "ktavMigrateCacheArtifactsStartup", "ktavCacheArtifactPaths"}:
            return "slots"
        if name in {"ktavMigrateOAuthJournalsAtStartup", "ktavValidateOAuthJournalBytes",
                    "ktavMigrateOAuthJournal"}:
            return "ROOT"
        if name == "warnKtavArtifactConflictOnce":
            return "docread"
        if name in MIGRATION_HOOK_VARS:
            return "ROOT"
        return "slotmigrate"
    if name in {"errOAuthJournalDisk", "errKtavWriteNotSupported"} and not file.startswith("io/"):
        return "docread"
    if name == "otherlinkat" and file.startswith("io/"):
        return "fswap"
    return pkg


def field_names(snippet: str) -> list[str]:
    """Extract top-level names from a struct type snippet (not selector refs)."""
    m = re.search(r"\bstruct\s*\{", snippet)
    if not m:
        return []
    start = m.end()
    depth = 1
    end = start
    while end < len(snippet) and depth:
        if snippet[end] == "{":
            depth += 1
        elif snippet[end] == "}":
            depth -= 1
        end += 1
    body = snippet[start:end - 1]
    out = []
    for line in body.splitlines():
        line = line.split("//", 1)[0].strip()
        if not line:
            continue
        # Named and multi-name fields; embedded fields are intentionally not
        # guessed from a qualified/type expression.
        left = line.split("`", 1)[0].strip()
        left = re.sub(r"\s+.*$", "", left)
        for n in left.split(","):
            n = n.strip()
            if re.fullmatch(r"[A-Za-z_]\w*", n):
                out.append(n)
    return sorted(set(out))


def main() -> None:
    ledger = json.loads(LEDGER_PATH.read_text(encoding="utf-8"))
    decls = ledger["Declarations"]
    by_file_name: dict[tuple[str, str], list[dict]] = collections.defaultdict(list)
    for d in decls:
        d["_owner"] = projected_owner(d)
        by_file_name[(d["File"], d["Name"])].append(d)

    # Source locations let us discard the false unqualified reference emitted
    # for a function's own identifier on its declaration/signature line.
    source_lines: dict[str, list[str]] = {}
    for f in ledger["Files"]:
        p = CONFIG / f
        if p.exists():
            source_lines[f] = p.read_text(encoding="utf-8", errors="replace").splitlines()

    edges: dict[tuple[str, str], dict] = {}
    raw_root_refs: dict[str, dict] = {}
    transformed: list[dict] = []
    signature_filtered = 0
    unresolved_target_refs = []
    for ref in ledger["References"]:
        source_candidates = by_file_name.get((ref["File"], ref["EnclosingDecl"]), []) if ref["EnclosingDecl"] else []
        source_owner = source_candidates[0]["_owner"] if source_candidates else original_owner(ref["File"])
        # Go records FuncDecl.Name as an Ident. astledger's package-flat name
        # resolver consequently reports it as a self-reference; drop only that
        # exact declaration-name occurrence, not body references.
        if source_candidates and ref["Line"] == source_candidates[0]["Start"]:
            for sd in source_candidates:
                if sd["Kind"] == "func" and sd["Name"].split(".")[-1] == ref["Name"]:
                    lines = source_lines.get(ref["File"], [])
                    sigline = lines[ref["Line"] - 1] if 0 < ref["Line"] <= len(lines) else ""
                    if re.match(r"\s*func\b", sigline):
                        signature_filtered += 1
                        break
            else:
                pass
            if source_candidates and ref["Line"] == source_candidates[0]["Start"]:
                # Only skip if this ref is itself the declared function name.
                if any(sd["Kind"] == "func" and sd["Name"].split(".")[-1] == ref["Name"] for sd in source_candidates):
                    continue

        targets = []
        target_decl_labels = []
        for target_file in ref["TargetFiles"]:
            for td in by_file_name.get((target_file, ref["Name"]), []):
                targets.append(td["_owner"])
                target_decl_labels.append(f"{td['Name']}@{td['File']}:{td['Start']}-{td['End']}")
        targets = sorted(set(targets))
        if not targets:
            unresolved_target_refs.append({"file": ref["File"], "line": ref["Line"], "name": ref["Name"],
                                           "enclosing": ref["EnclosingDecl"], "target_files": ref["TargetFiles"]})
            continue
        if source_owner == "ROOT" and ref["File"] == "facade.go":
            # The AST ledger intentionally omits imported selector identifiers.
            # Restore actual facade-to-package delegation from its import rows.
            pass
        for target_owner in targets:
            effective_target = target_owner
            transform = None
            if ref["Name"] == "configTestHooks" and source_owner != "ROOT":
                effective_target = "cfghooks"
                transform = "low-code provider snapshot (configTestHooks)"
            elif ref["Name"] in MIGRATION_HOOK_VARS and source_owner != "ROOT":
                effective_target = "cfghooks"
                transform = "low-code provider snapshot (mutable migration hook)"
            if transform:
                transformed.append({"from": source_owner, "raw_target": target_owner,
                                    "effective_target": effective_target, "name": ref["Name"],
                                    "file": ref["File"], "line": ref["Line"], "call": ref["Call"],
                                    "transformation": transform})
            if source_owner == effective_target:
                continue
            key = (source_owner, effective_target)
            edge = edges.setdefault(key, {"from": source_owner, "to": effective_target,
                                          "references": 0, "calls": 0,
                                          "names": collections.Counter(),
                                          "call_names": collections.Counter(), "evidence": []})
            edge["references"] += 1
            edge["names"][ref["Name"]] += 1
            if ref["Call"]:
                edge["calls"] += 1
                edge["call_names"][ref["Name"]] += 1
            ev = {"name": ref["Name"], "file": ref["File"], "line": ref["Line"],
                  "enclosing": ref["EnclosingDecl"], "call": ref["Call"],
                  "target_files": ref["TargetFiles"], "target_declarations": target_decl_labels}
            edge["evidence"].append(ev)
            if effective_target == "ROOT" and source_owner != "ROOT":
                ent = raw_root_refs.setdefault(ref["Name"], {"target": ref["Name"], "references": 0,
                    "calls": 0, "consumers": collections.Counter(), "call_consumers": collections.Counter(),
                    "evidence": []})
                ent["references"] += 1
                ent["consumers"][source_owner] += 1
                if ref["Call"]:
                    ent["calls"] += 1
                    ent["call_consumers"][source_owner] += 1
                ent["evidence"].append(ev | {"consumer": source_owner})

    # Explicit ROOT ports are reported separately and removed only from the
    # residual graph used for SCC/topological analysis.
    manual_root = {n: v for n, v in raw_root_refs.items() if n in ROOT_MANUAL_PORTS}
    unresolved_root = {n: v for n, v in raw_root_refs.items() if n not in ROOT_MANUAL_PORTS}

    # Add exact synthetic provider endpoints for transformations; these are
    # conceptual ports, not declarations silently erased from the ledger.
    owner_decls: dict[str, list[dict]] = collections.defaultdict(list)
    for d in decls:
        owner_decls[d["_owner"]].append({"name": d["Name"], "file": d["File"],
                                          "kind": d["Kind"], "start": d["Start"],
                                          "end": d["End"]})
    for name in sorted({x["name"] for x in transformed}):
        owner_decls["cfghooks"].append({"name": f"providerSnapshot({name})", "file": "<projection>"
                                        , "kind": "synthetic-provider", "start": None, "end": None})
    for owner in ("ROOT", "casefold", "fileid", "fswap", "cfgcommit", "docread", "slots",
                  "slotmigrate", "cfghooks", "cfgmodel", "cfgmerge", "envresolve", "datadir", "peak"):
        owner_decls.setdefault(owner, [])
    for owner in owner_decls:
        owner_decls[owner].sort(key=lambda x: (x["file"], x["start"] or 0, x["name"]))

    # Report struct field names separately: package-flat identifier refs omit
    # selector fields (cfg.Field) by construction in astledger.go.
    fields = []
    for d in decls:
        fs = field_names(d.get("Snippet", ""))
        if fs:
            fields.append({"type": d["Name"], "owner": d["_owner"], "file": d["File"], "fields": fs})
    lower_field_table = {}
    for row in fields:
        if row["owner"] != "ROOT":
            lower_field_table.setdefault(row["owner"], {})[row["type"]] = row["fields"]

    # Attribute each actual input-file byte exactly once to the owner of the
    # declaration covering its line; unclaimed scaffolding remains with the
    # source package. Shared lines spanning declarations in different owners
    # are divided evenly and flagged.
    packing = collections.Counter()
    packing_files: dict[str, collections.Counter] = collections.defaultdict(collections.Counter)
    mixed_lines = []
    for f in ledger["Files"]:
        lines = source_lines.get(f)
        if lines is None:
            continue
        default = original_owner(f)
        claims: dict[int, set[str]] = collections.defaultdict(set)
        for d in decls:
            if d["File"] != f:
                continue
            for ln in range(max(1, d["DocStart"]), min(len(lines), d["End"]) + 1):
                claims[ln].add(d["_owner"])
        for lineno, text in enumerate(lines, 1):
            byte_count = len((text + "\n").encode("utf-8"))
            owners = sorted(claims.get(lineno) or {default})
            if len(owners) > 1:
                mixed_lines.append({"file": f, "line": lineno, "owners": owners, "bytes": byte_count})
            quotient, rem = divmod(byte_count, len(owners))
            for i, owner in enumerate(owners):
                amount = quotient + (1 if i < rem else 0)
                packing[owner] += amount
                packing_files[owner][f] += amount

    # Graph utilities; manual ROOT ports are excluded only for the residual
    # SCC view, while complete edge data stays in the output.
    nodes = sorted(owner_decls)
    all_pairs = {(a, b) for a, b in edges}
    residual_pairs = {(a, b) for a, b in edges
                      if not (b == "ROOT" and any(n in ROOT_MANUAL_PORTS for n in edges[(a, b)]["names"]))}
    def components(pairs):
        adj = {n: [] for n in nodes}
        for a, b in pairs:
            adj.setdefault(a, []).append(b)
            adj.setdefault(b, [])
        index = 0
        stack, onstack, indexes, low, result = [], set(), {}, {}, []
        def visit(v):
            nonlocal index
            indexes[v] = low[v] = index
            index += 1
            stack.append(v); onstack.add(v)
            for w in sorted(adj.get(v, [])):
                if w not in indexes:
                    visit(w); low[v] = min(low[v], low[w])
                elif w in onstack:
                    low[v] = min(low[v], indexes[w])
            if low[v] == indexes[v]:
                comp = []
                while True:
                    w = stack.pop(); onstack.remove(w); comp.append(w)
                    if w == v: break
                result.append(sorted(comp))
        for v in nodes:
            if v not in indexes: visit(v)
        return sorted(result, key=lambda c: (c[0], len(c)))
    def scc_records(pairs):
        out = []
        for comp in components(pairs):
            cyc = len(comp) > 1 or any((n, n) in pairs for n in comp)
            if not cyc: continue
            inside = []
            for a, b in sorted(pairs):
                if a in comp and b in comp:
                    e = edges[(a, b)]
                    inside.append({"from": a, "to": b, "references": e["references"],
                                   "calls": e["calls"], "names": sorted(e["names"]),
                                   "evidence": e["evidence"]})
            out.append({"owners": comp, "edges": inside})
        return out
    def topo(pairs):
        indeg = {n: 0 for n in nodes}; adj = {n: set() for n in nodes}
        for a, b in pairs:
            if b not in adj.setdefault(a, set()):
                adj[a].add(b); indeg[b] = indeg.get(b, 0) + 1
        ready = sorted(n for n, d in indeg.items() if d == 0)
        order = []
        while ready:
            n = ready.pop(0); order.append(n)
            for v in sorted(adj.get(n, ())):
                indeg[v] -= 1
                if indeg[v] == 0:
                    ready.append(v); ready.sort()
        return order if len(order) == len(indeg) else None

    def edge_json(e):
        return {"from": e["from"], "to": e["to"], "references": e["references"],
                "calls": e["calls"], "names": dict(sorted(e["names"].items())),
                "call_names": dict(sorted(e["call_names"].items())),
                "evidence": sorted(e["evidence"], key=lambda x: (x["file"], x["line"], x["name"]))}

    root_calls = {}
    for n, item in sorted(raw_root_refs.items()):
        if item["calls"]:
            root_calls[n] = {"calls": item["calls"], "consumers": dict(sorted(item["call_consumers"].items())),
                             "references": item["references"]}
    helper_usage = {}
    for name in sorted(MODEL_HELPERS):
        occurrences = []
        for e in edges.values():
            if name in e["names"]:
                occurrences.extend(x for x in e["evidence"] if x["name"] == name)
        if occurrences:
            helper_usage[name] = occurrences

    result = {
        "input": {"ledger": str(LEDGER_PATH), "files": len(ledger["Files"]),
                  "io_files": sum(f.startswith("io/") for f in ledger["Files"]),
                  "declarations": len(decls), "references": len(ledger["References"]),
                  "signature_declaration_occurrences_filtered": signature_filtered,
                  "unresolved_ledger_targets": unresolved_target_refs},
        "owners": {k: owner_decls[k] for k in sorted(owner_decls)},
        "owner_file_sizes_bytes": {k: packing[k] for k in sorted(owner_decls)},
        "owner_file_sizes_by_file_bytes": {k: dict(sorted(v.items())) for k, v in sorted(packing_files.items())},
        "packing_method": "Source bytes assigned by AST DocStart..End line spans; uncovered package/import/scaffolding stays with original source owner; lines claimed by multiple owners split evenly. This is an explicit line-span estimate, not rewritten-file sizing.",
        "mixed_owner_source_lines": mixed_lines,
        "edges": [edge_json(edges[k]) for k in sorted(edges)],
        "root_production_call_counts": root_calls,
        "manual_root_ports": {n: manual_root[n] for n in sorted(manual_root)},
        "unresolved_root_dependencies": {n: unresolved_root[n] for n in sorted(unresolved_root)},
        "transformation_ledger": transformed,
        "full_graph_sccs": scc_records(all_pairs),
        "residual_graph_after_manual_ports_sccs": scc_records(residual_pairs),
        "residual_topological_order": topo(residual_pairs),
        "struct_field_names_from_snippets": fields,
        "lower_field_export_table_for_docwriter": lower_field_table,
        "selector_field_dependency_limitation": "Selector field references (x.Field) are excluded by astledger.go; field names are listed separately and are not graph edges.",
        "cfgmodel_helper_use_evidence": helper_usage,
        "virtual_owner_contracts": {"cfghooks": {"imports": ["context", "os", "time", "internal/kit/filelock"],
            "role": "pure wrappers/provider snapshots; no mutable exports"}},
    }
    OUTPUT_JSON.write_text(json.dumps(result, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")

    def fmt_counts(counter):
        return ", ".join(f"{k}={v}" for k, v in sorted(counter.items())) or "—"
    lines = [
        "IO DESIGN OWNER PROJECTION (generated by project.py)",
        f"Input: {len(ledger['Files'])} files ({result['input']['io_files']} IO), {len(decls)} declarations, {len(ledger['References'])} ledger refs; filtered {signature_filtered} decl-name occurrences.",
        "Reference evidence is AST-ledger line/name evidence; selector-field dependencies are reported separately.",
        "",
        "OWNER DECLARATIONS (name @ file:lines)",
    ]
    for owner in sorted(owner_decls):
        names = [f"{d['name']}@{d['file']}:{d['start']}-{d['end']}" for d in owner_decls[owner]]
        lines.append(f"{owner} [{len(names)}]: " + (", ".join(names) if names else "—"))
    lines += ["", "INTER-OWNER EDGES (all names; call counts by name)"]
    for k in sorted(edges):
        e = edges[k]
        lines.append(f"{k[0]} -> {k[1]} refs={e['references']} calls={e['calls']} names=[{fmt_counts(e['names'])}] calls-by-name=[{fmt_counts(e['call_names'])}]")
        for ev in sorted(e["evidence"], key=lambda x:(x["file"],x["line"],x["name"])):
            lines.append(f"  {ev['file']}:{ev['line']} {ev['enclosing'] or '<package>'} -> {ev['name']}{'()' if ev['call'] else ''} targets={','.join(ev['target_files'])}")
    lines += ["", "ROOT-PRODUCTION CALL COUNTS (incoming calls to ROOT declarations)"]
    if not root_calls:
        lines.append("—")
    for name, item in sorted(root_calls.items()):
        lines.append(f"{name}: calls={item['calls']} refs={item['references']} consumers=[{fmt_counts(collections.Counter(item['consumers']))}]")
    lines += ["", "EXPLICIT MANUAL ROOT PORTS (excluded from residual SCC only)"]
    if not manual_root: lines.append("—")
    for name, item in sorted(manual_root.items()):
        lines.append(f"{name}: refs={item['references']} calls={item['calls']} consumers=[{fmt_counts(item['consumers'])}]")
        for ev in item["evidence"]:
            lines.append(f"  {ev['file']}:{ev['line']} {ev['consumer']} {ev['enclosing']} calls={ev['call']}")
    lines += ["", "UNRESOLVED ROOT DEPENDENCIES (not auto-erased)"]
    if not unresolved_root: lines.append("—")
    for name, item in sorted(unresolved_root.items()):
        lines.append(f"{name}: refs={item['references']} calls={item['calls']} consumers=[{fmt_counts(item['consumers'])}]")
        for ev in item["evidence"]:
            lines.append(f"  {ev['file']}:{ev['line']} {ev['consumer']} {ev['enclosing']} {'call' if ev['call'] else 'ref'}")
    lines += ["", "TRANSFORMED LOW-CODE PROVIDER REFERENCES"]
    for t in transformed:
        lines.append(f"{t['from']} {t['file']}:{t['line']} {t['name']} -> {t['effective_target']} ({t['transformation']})")
    if not transformed: lines.append("—")
    lines += ["", "GRAPH ANALYSIS"]
    lines.append(f"Full graph SCCs: {len(result['full_graph_sccs'])}")
    for scc in result["full_graph_sccs"]:
        lines.append("  SCC: " + ", ".join(scc["owners"]))
        for e in scc["edges"]:
            lines.append(f"    {e['from']} -> {e['to']} refs={e['references']} calls={e['calls']} names=[{', '.join(e['names'])}]")
    lines.append(f"Residual SCCs after manual ports: {len(result['residual_graph_after_manual_ports_sccs'])}")
    for scc in result["residual_graph_after_manual_ports_sccs"]:
        lines.append("  SCC: " + ", ".join(scc["owners"]))
        for e in scc["edges"]:
            lines.append(f"    {e['from']} -> {e['to']} refs={e['references']} calls={e['calls']} names=[{', '.join(e['names'])}]")
    lines.append("Residual topological order: " + (" -> ".join(result["residual_topological_order"]) if result["residual_topological_order"] else "CYCLIC"))
    lines += ["", "PACKED SOURCE FILE SIZES (bytes; AST line-span estimate)"]
    for owner, size in sorted(result["owner_file_sizes_bytes"].items(), key=lambda x:(-x[1],x[0])):
        lines.append(f"{owner}: {size} bytes across {len(packing_files.get(owner, {}))} source files")
    lines.append(f"Mixed-owner source lines split: {len(mixed_lines)}")
    lines += ["", "STRUCT FIELD NAMES / LOWER FIELD EXPORT TABLE (not graph refs)"]
    for row in fields:
        lines.append(f"{row['owner']} {row['type']}@{row['file']}: " + ", ".join(row['fields']))
    lines.append("Selector-field limitation: x.Field references are hidden from this graph; use the table above separately in docwriter.")
    lines += ["", "CFGMODEL HELPER USE EVIDENCE"]
    if not helper_usage: lines.append("—")
    for name, evs in sorted(helper_usage.items()):
        lines.append(f"{name}: " + "; ".join(f"{e['file']}:{e['line']} {e['enclosing']} ({'call' if e['call'] else 'ref'})" for e in evs))
    lines += ["", f"JSON: {OUTPUT_JSON}"]
    OUTPUT_TEXT.write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"wrote {OUTPUT_TEXT} and {OUTPUT_JSON}")
    print(f"owners={len(owner_decls)} edges={len(edges)} root_dependencies={len(raw_root_refs)} unresolved_root={len(unresolved_root)} manual_ports={len(manual_root)}")
    print(f"full_sccs={len(result['full_graph_sccs'])} residual_sccs={len(result['residual_graph_after_manual_ports_sccs'])} topo={'acyclic' if result['residual_topological_order'] else 'cyclic'}")
    print("packing_bytes=" + json.dumps(result["owner_file_sizes_bytes"], sort_keys=True))

if __name__ == "__main__":
    main()
```

#### `.rush/tmp/io-design/corrected_project.py` — executable correction wrapper

```python
#!/usr/bin/env python3
"""Execute the existing project.py with documented projection corrections."""
from pathlib import Path

HERE = Path(__file__).resolve().parent
ORIGINAL = HERE / "project.py"
text = ORIGINAL.read_text(encoding="utf-8")

# Correct source-owner assignments before the original projection logic runs.
needle = "    pkg = original_owner(file)\n"
insert = """    pkg = original_owner(file)
    if file.endswith("config_linkat_unix.go"):
        if name == "readConfigEntryAt":
            return "docread"
        if name == "configFileNlinkOfInfo":
            return "fileid"
        return "fswap"
    if name == "physicalWindowsConfigPath":
        return "fileid"
    if name in {"newConfigFileIsKtav", "writeTargetSlot"}:
        return "slots"
    if name in {"errReloadDiskChanged", "ktavAutoMigrate"}:
        return "ROOT"
"""
if needle not in text:
    raise SystemExit("project.py projected_owner insertion point changed")
text = text.replace(needle, insert, 1)

# Give the root-source resolver the narrowest declaration spanning the location;
# a missing enclosing-name candidate must not inherit an unrelated owner.
old = '        source_candidates = by_file_name.get((ref["File"], ref["EnclosingDecl"]), []) if ref["EnclosingDecl"] else []\n'
new = """        containing = [d for d in decls if d["File"] == ref["File"] and d["Start"] <= ref["Line"] <= d["End"]]
        source_candidates = [min(containing, key=lambda d: (d["End"] - d["Start"], d["Start"], d["Name"]))] if containing else []
"""
if old not in text:
    raise SystemExit("project.py source-candidate line changed")
text = text.replace(old, new, 1)

# Exclude test references from the production dependency graph; the document's
# test appendix is calculated independently from the complete raw ledger.
old = '    for ref in ledger["References"]:\n'
new = '    for ref in ledger["References"]:\n        if ref["File"].endswith("_test.go"):\n            continue\n'
if old not in text:
    raise SystemExit("project.py reference loop changed")
text = text.replace(old, new, 1)

# Mutable KTAV auto-migration state remains physically owned by ROOT. Lower
# packages receive its value through the approved provider snapshot boundary.
old = """            elif ref["Name"] in MIGRATION_HOOK_VARS and source_owner != "ROOT":
                effective_target = "cfghooks"
                transform = "low-code provider snapshot (mutable migration hook)"
"""
new = """            elif ref["Name"] in MIGRATION_HOOK_VARS and source_owner != "ROOT":
                effective_target = "cfghooks"
                transform = "low-code provider snapshot (mutable migration hook)"
            elif ref["Name"] == "ktavAutoMigrate" and source_owner != "ROOT":
                effective_target = "cfghooks"
                transform = "root-owned KTAV flag provider snapshot (slots API; no mutable export)"
"""
if old not in text:
    raise SystemExit("project.py provider transformation block changed")
text = text.replace(old, new, 1)

# Count and drop only an exact function self-identifier on its declaration line.
old = """        if source_candidates and ref["Line"] == source_candidates[0]["Start"]:
            for sd in source_candidates:
                if sd["Kind"] == "func" and sd["Name"].split(".")[-1] == ref["Name"]:
                    lines = source_lines.get(ref["File"], [])
                    sigline = lines[ref["Line"] - 1] if 0 < ref["Line"] <= len(lines) else ""
                    if re.match(r"\\s*func\\b", sigline):
                        signature_filtered += 1
                        break
            else:
                pass
            if source_candidates and ref["Line"] == source_candidates[0]["Start"]:
                # Only skip if this ref is itself the declared function name.
                if any(sd["Kind"] == "func" and sd["Name"].split(".")[-1] == ref["Name"] for sd in source_candidates):
                    continue
"""
new = """        if (source_candidates and ref["Line"] == source_candidates[0]["Start"]
                and any(sd["Kind"] == "func" and sd["Name"].split(".")[-1] == ref["Name"]
                        for sd in source_candidates)):
            signature_filtered += 1
            continue
"""
if old not in text:
    raise SystemExit("project.py declaration-self-reference block changed")
text = text.replace(old, new, 1)

# This is the same parent script executed in memory, with __file__ anchored to
# the original artifact so HERE/REPO resolve correctly. No bare root runner.
namespace = {"__name__": "__main__", "__file__": str(ORIGINAL), "__package__": None}
exec(compile(text, str(ORIGINAL), "exec"), namespace)
```

The corrected graph is **acyclic** with no unresolved root dependency. The original `project.py` projection script has a genuine pre-correction cycle from incorrectly placing `ktavAutoMigrate` in the lower migration package; this wrapper routes references to the root-owned flag through the approved provider snapshot endpoint and retains the physical root storage. The three-point correction set in this artifact is source-owner location selection, migration-state provider transformation, and exact declaration self-reference removal; the full analysis script itself remains unmodified.

### G. Layout and measured destination-span budgets

The proposed tree has seven immediate IO children; `write/` has exactly two children (`cfgcommit`, `slotmigrate`). Source spans are measured as the union of `DocStart..End` lines for declarations assigned to each proposed destination, clamping `DocStart` to `Start` and deduplicating overlapping group-doc lines within a destination. Add a 60-line source-header/import allowance to every destination file. This is an explicit source-span planning estimate, not exact formatted output LOC. The synthetic `cfghooks` provider is contract-sized separately and is not falsely claimed as a ledger-measured file.

| Package | Planned Go files | Files |
|---|---|---:|
| `casefold` | `config_case_sensitivity.go`, `config_case_sensitivity_behavioral_unix.go`, `config_case_sensitivity_darwin.go`, `config_case_sensitivity_freebsd.go`, `config_case_sensitivity_linux.go`, `config_case_sensitivity_other_unix.go`, `config_case_sensitivity_windows.go` | 7 |
| `cfgcommit` | `atomicwrite.go`, `commit_unix.go`, `commit_windows.go` | 3 |
| `cfghooks` | `hooks.go` | 1 |
| `docread` | `candidates.go`, `fingerprints.go`, `mcpdocs.go`, `slotdocs.go`, `stabledocs.go`, `stabledocs_unix.go`, `stabledocs_windows.go` | 7 |
| `fileid` | `identity.go`, `identity_unix.go`, `identity_windows.go` | 3 |
| `fswap` | `rename_darwin.go`, `rename_unix.go`, `rename_windows.go`, `renameat2_linux.go`, `renameat2_other_unix.go`, `linkat_unix.go`, `write_identity.go` | 7 |
| `slotmigrate` | `ktav_migration.go`, `ktav_migration_artifacts.go`, `ktav_migration_cache_rev.go`, `ktav_migration_symlink.go`, `ktav_migration_unix.go`, `ktav_migration_windows.go` | 6 |
| `slots` | `ktav_gate.go`, `ktav_startup.go`, `ktav_cache.go`, `ktav_light.go`, `ktav_oauth.go` | 5 |
| **IO tree** | Seven children: `cfghooks`, `fileid`, `casefold`, `docread`, `fswap`, `slots`, `write`; `write/{cfgcommit,slotmigrate}` | **8 packages; 2 write children** |

| Package | Destination file | Decl count | Union source-span lines | Header/import allowance | Estimated lines | Bound/status |
|---|---|---:|---:|---:|---:|---|
| `casefold` | `config_case_sensitivity.go` | 26 | 172 | 60 | 232 | ≤900 |
| `casefold` | `config_case_sensitivity_behavioral_unix.go` | 2 | 54 | 60 | 114 | ≤900 |
| `casefold` | `config_case_sensitivity_darwin.go` | 3 | 15 | 60 | 75 | ≤900 |
| `casefold` | `config_case_sensitivity_freebsd.go` | 3 | 34 | 60 | 94 | ≤900 |
| `casefold` | `config_case_sensitivity_linux.go` | 3 | 32 | 60 | 92 | ≤900 |
| `casefold` | `config_case_sensitivity_other_unix.go` | 2 | 6 | 60 | 66 | ≤900 |
| `casefold` | `config_case_sensitivity_windows.go` | 5 | 53 | 60 | 113 | ≤900 |
| `cfgcommit` | `atomicwrite.go` | 16 | 166 | 60 | 226 | ≤900 |
| `cfgcommit` | `commit_unix.go` | 4 | 244 | 60 | 304 | ≤900 |
| `cfgcommit` | `commit_windows.go` | 5 | 281 | 60 | 341 | ≤900 |
| `cfghooks` | `hooks.go` | 11 | 97 | n/a | ≤250 contract cap | synthetic contract, not measured |
| `docread` | `candidates.go` | 9 | 163 | 60 | 223 | ≤900 |
| `docread` | `fingerprints.go` | 8 | 162 | 60 | 222 | ≤900 |
| `docread` | `mcpdocs.go` | 17 | 244 | 60 | 304 | ≤900 |
| `docread` | `slotdocs.go` | 49 | 515 | 60 | 575 | ≤900 |
| `docread` | `stabledocs.go` | 21 | 332 | 60 | 392 | ≤900 |
| `docread` | `stabledocs_unix.go` | 1 | 43 | 60 | 103 | ≤900 |
| `docread` | `stabledocs_windows.go` | 1 | 38 | 60 | 98 | ≤900 |
| `fileid` | `identity.go` | 6 | 23 | 60 | 83 | ≤900 |
| `fileid` | `identity_unix.go` | 14 | 105 | 60 | 165 | ≤900 |
| `fileid` | `identity_windows.go` | 29 | 424 | 60 | 484 | ≤900 |
| `fswap` | `rename_darwin.go` | 1 | 31 | 60 | 91 | ≤900 |
| `fswap` | `rename_unix.go` | 4 | 41 | 60 | 101 | ≤900 |
| `fswap` | `rename_windows.go` | 9 | 265 | 60 | 325 | ≤900 |
| `fswap` | `renameat2_linux.go` | 1 | 25 | 60 | 85 | ≤900 |
| `fswap` | `renameat2_other_unix.go` | 1 | 11 | 60 | 71 | ≤900 |
| `fswap` | `linkat_unix.go` | 9 | 187 | 60 | 247 | ≤900 |
| `fswap` | `write_identity.go` | 3 | 12 | 60 | 72 | ≤900 |
| `slotmigrate` | `ktav_migration.go` | 19 | 438 | 60 | 498 | ≤900 |
| `slotmigrate` | `ktav_migration_artifacts.go` | 10 | 336 | 60 | 396 | ≤900 |
| `slotmigrate` | `ktav_migration_cache_rev.go` | 1 | 69 | 60 | 129 | ≤900 |
| `slotmigrate` | `ktav_migration_symlink.go` | 6 | 216 | 60 | 276 | ≤900 |
| `slotmigrate` | `ktav_migration_unix.go` | 1 | 81 | 60 | 141 | ≤900 |
| `slotmigrate` | `ktav_migration_windows.go` | 1 | 80 | 60 | 140 | ≤900 |
| `slots` | `ktav_gate.go` | 2 | 29 | 60 | 89 | ≤900 |
| `slots` | `ktav_startup.go` | 4 | 120 | 60 | 180 | ≤900 |
| `slots` | `ktav_cache.go` | 2 | 51 | 60 | 111 | ≤900 |
| `slots` | `ktav_light.go` | 4 | 41 | 60 | 101 | ≤900 |
| `slots` | `ktav_oauth.go` | 1 | 23 | 60 | 83 | ≤900 |

The 46-file baseline’s `ktavAutoMigrate` location at `io/slots/ktav_discovery.go` is incorrect: that file does not declare this flag. The actual declaration is `io/slotmigrate/ktav_migration.go:25`; the mutable storage remains root-owned and lower consumers use an approved provider snapshot, never an exported global.

### H. Production declaration moves and root policy boundary

Every production declaration whose current package (`ROOT` or the original `io/<pkg>`) differs from its corrected final owner is listed below. `final destination` is a proposed file. A witness is one ledger use outside the original source file when available; otherwise the row explicitly says the declaration has no external identifier-use witness in this ledger. This inventory does not imply the file move occurred.

| Identifier | From owner/source | To owner/file | Use witness |
|---|---|---|---|
| `errConfigCommitVerification` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:16` | `fileid` / `identity.go` | `config_commit_unix_test.go:34` (`errConfigCommitVerification`) |
| `ErrConfigHardLink` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:35` | `fileid` / `identity.go` | `config_commit_recovery_unix_test.go:47` (`ErrConfigHardLink`) |
| `configTestHooks` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:86` | `ROOT` / `atomicwrite.go` | `config_commit_outcome_test.go:14` (`configTestHooks`) |
| `runConfigBeforeOpenHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:115` | `cfghooks` / `hooks.go` | `io/docread/stabledocs.go:121` (`runConfigBeforeOpenHook`) |
| `runConfigAfterStableReadHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:124` | `cfghooks` / `hooks.go` | `io/docread/stabledocs.go:158` (`runConfigAfterStableReadHook`) |
| `runConfigBeforeCommitCheckHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:133` | `cfghooks` / `hooks.go` | `io/cfgcommit/config_commit_unix.go:125` (`runConfigBeforeCommitCheckHook`) |
| `runConfigAfterCommitRenameHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:142` | `cfghooks` / `hooks.go` | no external identifier-use witness in ledger |
| `runConfigAfterCommitRenameHookForPath` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:152` | `cfghooks` / `hooks.go` | `io/cfgcommit/config_commit_unix.go:180` (`runConfigAfterCommitRenameHookForPath`) |
| `runConfigAfterMCPCommitHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:166` | `cfghooks` / `hooks.go` | `store_mcp_transaction.go:444` (`runConfigAfterMCPCommitHook`) |
| `runConfigAfterMCPResolveTargetHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:175` | `ROOT` / `atomicwrite.go` | `store_mcp_locks.go:285` (`runConfigAfterMCPResolveTargetHook`) |
| `runConfigBeforeMCPTargetBindingVerificationHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:184` | `ROOT` / `atomicwrite.go` | `store_write.go:198` (`runConfigBeforeMCPTargetBindingVerificationHook`) |
| `runConfigBeforeCommitRenameHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:193` | `cfghooks` / `hooks.go` | `io/cfgcommit/config_commit_unix.go:142` (`runConfigBeforeCommitRenameHook`) |
| `runConfigBeforeMCPReconcileHook` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:202` | `cfghooks` / `hooks.go` | `store_mcp_transaction.go:461` (`runConfigBeforeMCPReconcileHook`) |
| `acquireConfigFileLock` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:211` | `cfghooks` / `hooks.go` | `config_sidecar_identity_test.go:355` (`acquireConfigFileLock`) |
| `releaseConfigFileLock` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:221` | `cfghooks` / `hooks.go` | `io/slotmigrate/ktav_migration.go:122` (`releaseConfigFileLock`) |
| `configContextWithTimeout` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:231` | `cfghooks` / `hooks.go` | `store_mcp_admission.go:229` (`configContextWithTimeout`) |
| `syncConfigParent` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:241` | `fswap` / `rename_unix.go` | `io/slotmigrate/ktav_migration.go:164` (`syncConfigParent`) |
| `syncConfigParentFile` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:251` | `fswap` / `rename_unix.go` | `io/cfgcommit/config_commit_unix.go:183` (`syncConfigParentFile`) |
| `sameBytesFingerprint` | `cfgcommit` / `io/cfgcommit/atomicwrite.go:271` | `docread` / `stabledocs.go` | `io/cfgcommit/config_commit_unix.go:35` (`sameBytesFingerprint`) |
| `SetBeforeConfigCommitForTest` | `cfgcommit` / `io/cfgcommit/config_commit_test_seam.go:4` | `ROOT` / `config_commit_test_seam.go` | no external identifier-use witness in ledger |
| `physicalWindowsConfigPath` | `cfgcommit` / `io/cfgcommit/config_commit_windows.go:197` | `fileid` / `identity_windows.go` | `io/fileid/config_parent_identity_windows.go:6` (`physicalWindowsConfigPath`) |
| `syncConfigParentOnDisk` | `cfgcommit` / `io/cfgcommit/config_durability_unix.go:12` | `fswap` / `rename_unix.go` | `config_rename_windows_test.go:339` (`syncConfigParentOnDisk`) |
| `syncConfigParentOnDisk` | `cfgcommit` / `io/cfgcommit/config_durability_windows.go:14` | `fswap` / `rename_windows.go` | `config_rename_windows_test.go:339` (`syncConfigParentOnDisk`) |
| `publishConfigTempNoReplaceAt` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:19` | `fswap` / `linkat_unix.go` | `io/fswap/config_rename_darwin.go:22` (`publishConfigTempNoReplaceAt`) |
| `configTempIdentityAt` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:52` | `fswap` / `linkat_unix.go` | `io/fswap/config_rename_darwin.go:24` (`configTempIdentityAt`) |
| `cleanupConfigTempAlias` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:73` | `fswap` / `linkat_unix.go` | `io/cfgcommit/config_commit_unix.go:93` (`cleanupConfigTempAlias`) |
| `recoverConfigTempAlias` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:87` | `fswap` / `linkat_unix.go` | `io/cfgcommit/config_commit_unix.go:61` (`recoverConfigTempAlias`) |
| `readConfigEntryAt` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:150` | `docread` / `stabledocs_unix.go` | `io/slotmigrate/ktav_migration_unix.go:53` (`readConfigEntryAt`) |
| `configEntryInfoAt` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:194` | `fswap` / `linkat_unix.go` | no external identifier-use witness in ledger |
| `configFileNlinkOfInfo` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:208` | `fileid` / `identity_unix.go` | no external identifier-use witness in ledger |
| `cleanupConfigTempAliasOwned` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:212` | `fswap` / `linkat_unix.go` | no external identifier-use witness in ledger |
| `configTempAliasPolicyAcceptable` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:226` | `fswap` / `linkat_unix.go` | no external identifier-use witness in ledger |
| `isConfigTempAliasName` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:234` | `fswap` / `linkat_unix.go` | no external identifier-use witness in ledger |
| `unlinkConfigTempAt` | `cfgcommit` / `io/cfgcommit/config_linkat_unix.go:248` | `fswap` / `linkat_unix.go` | `io/cfgcommit/config_commit_unix.go:86` (`unlinkConfigTempAt`) |
| `configWriteTargetDedupKey` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:12` | `ROOT` / `config_write_identity.go` | `config_sidecar_identity_test.go:46` (`configWriteTargetDedupKey`) |
| `configWriteTargetDedupKeyWithCaseFold` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:16` | `ROOT` / `config_write_identity.go` | `config_sidecar_identity_test.go:289` (`configWriteTargetDedupKeyWithCaseFold`) |
| `configWriteTargetDedupKeyWithCaseSensitivity` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:42` | `ROOT` / `config_write_identity.go` | `store_mcp_locks.go:219` (`configWriteTargetDedupKeyWithCaseSensitivity`) |
| `configWriteIdentityKey` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:48` | `fswap` / `write_identity.go` | no external identifier-use witness in ledger |
| `configDirectoryIsCaseInsensitive` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:52` | `fswap` / `write_identity.go` | `config_sidecar_identity_test.go:78` (`configDirectoryIsCaseInsensitive`) |
| `configEquivalentLeafNames` | `cfgcommit` / `io/cfgcommit/config_write_identity.go:57` | `fswap` / `write_identity.go` | `io/cfgcommit/atomicwrite.go:329` (`configEquivalentLeafNames`) |
| `candidateInputsChanged` | `docread` / `io/docread/fingerprints.go:166` | `ROOT` / `fingerprints.go` | `load.go:244` (`candidateInputsChanged`) |
| `errReloadDiskChanged` | `docread` / `io/docread/fingerprints.go:178` | `ROOT` / `fingerprints.go` | `load.go:99` (`errReloadDiskChanged`) |
| `initialMCPRevisions` | `docread` / `io/docread/fingerprints.go:180` | `ROOT` / `fingerprints.go` | `load.go:227` (`initialMCPRevisions`) |
| `mcpInputFingerprints` | `docread` / `io/docread/fingerprints.go:193` | `ROOT` / `fingerprints.go` | `load.go:300` (`mcpInputFingerprints`) |
| `mergeExternalMCPServers` | `docread` / `io/docread/mcpdocs.go:197` | `ROOT` / `mcpdocs.go` | no external identifier-use witness in ledger |
| `readMCPDisabledOverride` | `docread` / `io/docread/mcpdocs.go:272` | `ROOT` / `mcpdocs.go` | `store_mcp_test.go:460` (`readMCPDisabledOverride`) |
| `mcpRevisionDiff` | `docread` / `io/docread/mcpdocs.go:328` | `ROOT` / `mcpdocs.go` | `store.go:624` (`mcpRevisionDiff`) |
| `mcpNames` | `docread` / `io/docread/mcpdocs.go:370` | `ROOT` / `mcpdocs.go` | no external identifier-use witness in ledger |
| `loadConfigCandidateStableForWorkingDir` | `docread` / `io/docread/stabledocs.go:30` | `ROOT` / `stabledocs.go` | `ktav_discovery_test.go:340` (`loadConfigCandidateStableForWorkingDir`) |
| `readStableConfigHandle` | `fileid` / `io/fileid/config_file_identity_windows.go:368` | `docread` / `stabledocs_windows.go` | `io/cfgcommit/config_commit_windows.go:107` (`readStableConfigHandle`) |
| `openStableConfigFile` | `fswap` / `io/fswap/config_open_unix.go:11` | `fileid` / `identity_unix.go` | `config_cycle7_test.go:324` (`openStableConfigFile`) |
| `writeStableConfigFile` | `fswap` / `io/fswap/config_open_unix.go:38` | `fileid` / `identity_unix.go` | `config_cycle7_test.go:180` (`writeStableConfigFile`) |
| `openStableConfigFile` | `fswap` / `io/fswap/config_open_windows.go:12` | `fileid` / `identity_windows.go` | `config_cycle7_test.go:324` (`openStableConfigFile`) |
| `writeStableConfigFile` | `fswap` / `io/fswap/config_open_windows.go:18` | `fileid` / `identity_windows.go` | `config_cycle7_test.go:180` (`writeStableConfigFile`) |
| `ktavAutoMigrate` | `slotmigrate` / `io/slotmigrate/ktav_migration.go:25` | `ROOT` / `ktav_migration.go` | `io/slotmigrate/ktav_migration_artifacts.go:393` (`ktavAutoMigrate`) |
| `ktavMigrationBudget` | `slotmigrate` / `io/slotmigrate/ktav_migration.go:26` | `ROOT` / `ktav_migration.go` | `io/slotmigrate/ktav_migration_artifacts.go:99` (`ktavMigrationBudget`) |
| `ktavMigrationStepHook` | `slotmigrate` / `io/slotmigrate/ktav_migration.go:33` | `ROOT` / `ktav_migration.go` | `ktav_migration_artifacts_test.go:213` (`ktavMigrationStepHook`) |
| `ktavMigrateStartupSlots` | `slotmigrate` / `io/slotmigrate/ktav_migration.go:489` | `slots` / `ktav_startup.go` | `ktav_migration_test.go:596` (`ktavMigrateStartupSlots`) |
| `ktavMigrateWorkspaceIfNeeded` | `slotmigrate` / `io/slotmigrate/ktav_migration.go:529` | `slots` / `ktav_startup.go` | `ktav_migration_test.go:532` (`ktavMigrateWorkspaceIfNeeded`) |
| `ktavArtifactMigrationStepHook` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:32` | `ROOT` / `ktav_migration_artifacts.go` | `ktav_migration_artifacts_test.go:212` (`ktavArtifactMigrationStepHook`) |
| `ktavArtifactStatFile` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:36` | `ROOT` / `ktav_migration_artifacts.go` | `ktav_migration_artifacts_test.go:531` (`ktavArtifactStatFile`) |
| `ktavArtifactChtimes` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:37` | `ROOT` / `ktav_migration_artifacts.go` | `ktav_migration_artifacts_test.go:531` (`ktavArtifactChtimes`) |
| `ktavMigrateOAuthJournalsAtStartup` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:392` | `ROOT` / `ktav_migration_artifacts.go` | `ktav_migration_artifacts_test.go:411` (`ktavMigrateOAuthJournalsAtStartup`) |
| `ktavValidateOAuthJournalBytes` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:420` | `ROOT` / `ktav_migration_artifacts.go` | no external identifier-use witness in ledger |
| `ktavMigrateOAuthJournal` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:435` | `ROOT` / `ktav_migration_artifacts.go` | `ktav_migration_artifacts_test.go:92` (`ktavMigrateOAuthJournal`) |
| `warnKtavArtifactConflictOnce` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:493` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `ktavMigrateCacheArtifactsStartup` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:507` | `slots` / `ktav_startup.go` | `io/slotmigrate/ktav_migration.go:538` (`ktavMigrateCacheArtifactsStartup`) |
| `ktavCacheArtifactPaths` | `slotmigrate` / `io/slotmigrate/ktav_migration_artifacts.go:530` | `slots` / `ktav_startup.go` | no external identifier-use witness in ledger |
| `ktavConfigExt` | `slots` / `io/slots/ktav_discovery.go:24` | `docread` / `slotdocs.go` | `io/slots/ktav_doc.go:144` (`ktavConfigExt`) |
| `ktavBackupSuffix` | `slots` / `io/slots/ktav_discovery.go:26` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `ktavMigrationRecordSuffix` | `slots` / `io/slots/ktav_discovery.go:29` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `errKtavBackupOnly` | `slots` / `io/slots/ktav_discovery.go:35` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:148` (`errKtavBackupOnly`) |
| `errKtavMigrationConflict` | `slots` / `io/slots/ktav_discovery.go:38` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:181` (`errKtavMigrationConflict`) |
| `errKtavMigrationRecord` | `slots` / `io/slots/ktav_discovery.go:41` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:349` (`errKtavMigrationRecord`) |
| `ktavSiblingOf` | `slots` / `io/slots/ktav_discovery.go:45` | `docread` / `slotdocs.go` | `catwalk_test.go:92` (`ktavSiblingOf`) |
| `configSlotBaseNames` | `slots` / `io/slots/ktav_discovery.go:53` | `docread` / `slotdocs.go` | `load_files.go:70` (`configSlotBaseNames`) |
| `expandConfigSlotSiblings` | `slots` / `io/slots/ktav_discovery.go:65` | `docread` / `slotdocs.go` | `io/docread/fingerprints.go:130` (`expandConfigSlotSiblings`) |
| `backupPathFor` | `slots` / `io/slots/ktav_discovery.go:86` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:88` (`backupPathFor`) |
| `migrationRecordPathFor` | `slots` / `io/slots/ktav_discovery.go:93` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:89` (`migrationRecordPathFor`) |
| `isKtavConfigPath` | `slots` / `io/slots/ktav_discovery.go:98` | `docread` / `slotdocs.go` | `io/docread/candidates.go:27` (`isKtavConfigPath`) |
| `isKtavBackupPath` | `slots` / `io/slots/ktav_discovery.go:104` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:507` (`isKtavBackupPath`) |
| `isKtavMigrationRecordPath` | `slots` / `io/slots/ktav_discovery.go:110` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:507` (`isKtavMigrationRecordPath`) |
| `ktavBothFormatsWarned` | `slots` / `io/slots/ktav_discovery.go:114` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:494` (`ktavBothFormatsWarned`) |
| `warnBothFormatsOnce` | `slots` / `io/slots/ktav_discovery.go:118` | `docread` / `slotdocs.go` | `io/slots/ktav_cache.go:41` (`warnBothFormatsOnce`) |
| `warnMigrationDeferredOnce` | `slots` / `io/slots/ktav_discovery.go:136` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:523` (`warnMigrationDeferredOnce`) |
| `selectConfigSlot` | `slots` / `io/slots/ktav_discovery.go:151` | `docread` / `slotdocs.go` | `io/slots/ktav_doc.go:83` (`selectConfigSlot`) |
| `decodeConfigDocument` | `slots` / `io/slots/ktav_discovery.go:177` | `docread` / `slotdocs.go` | `io/docread/stabledocs.go:81` (`decodeConfigDocument`) |
| `workspaceConfigPath` | `slots` / `io/slots/ktav_discovery.go:194` | `docread` / `slotdocs.go` | `ktav_discovery_test.go:298` (`workspaceConfigPath`) |
| `matchesLogicalSlot` | `slots` / `io/slots/ktav_discovery.go:205` | `docread` / `slotdocs.go` | `io/docread/candidates.go:115` (`matchesLogicalSlot`) |
| `checkBackupOnlySlot` | `slots` / `io/slots/ktav_discovery.go:215` | `docread` / `slotdocs.go` | `io/slots/ktav_doc.go:84` (`checkBackupOnlySlot`) |
| `checkAuthoritativeBackupOnly` | `slots` / `io/slots/ktav_discovery.go:233` | `docread` / `slotdocs.go` | `ktav_discovery_test.go:170` (`checkAuthoritativeBackupOnly`) |
| `ktavProjectLegacySlots` | `slots` / `io/slots/ktav_discovery.go:252` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `ktavMigrationPendingSlots` | `slots` / `io/slots/ktav_discovery.go:282` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:498` (`ktavMigrationPendingSlots`) |
| `ktavEntryExists` | `slots` / `io/slots/ktav_discovery.go:309` | `docread` / `slotdocs.go` | `catwalk_test.go:92` (`ktavEntryExists`) |
| `ktavEntryAbsentErr` | `slots` / `io/slots/ktav_discovery.go:319` | `docread` / `slotdocs.go` | `io/slots/ktav_cache.go:27` (`ktavEntryAbsentErr`) |
| `fileExists` | `slots` / `io/slots/ktav_discovery.go:323` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration.go:510` (`fileExists`) |
| `settingsDocumentFormat` | `slots` / `io/slots/ktav_doc.go:17` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `settingsDocumentJSON` | `slots` / `io/slots/ktav_doc.go:21` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `settingsDocumentKtav` | `slots` / `io/slots/ktav_doc.go:23` | `docread` / `slotdocs.go` | no external identifier-use witness in ledger |
| `settingsDocument` | `slots` / `io/slots/ktav_doc.go:30` | `docread` / `slotdocs.go` | `settings_lock.go:492` (`settingsDocument`) |
| `settingsDocumentFormatOf` | `slots` / `io/slots/ktav_doc.go:39` | `docread` / `slotdocs.go` | `apitoken_store.go:523` (`settingsDocumentFormatOf`) |
| `decodeSettingsDocument` | `slots` / `io/slots/ktav_doc.go:50` | `docread` / `slotdocs.go` | `apitoken_store.go:405` (`decodeSettingsDocument`) |
| `readSettingsDocument` | `slots` / `io/slots/ktav_doc.go:81` | `docread` / `slotdocs.go` | `custom_roles_write_test.go:89` (`readSettingsDocument`) |
| `encodeSettingsDocument` | `slots` / `io/slots/ktav_doc.go:99` | `docread` / `slotdocs.go` | `apitoken_store.go:524` (`encodeSettingsDocument`) |
| `rejectUnconvertedKtavWrite` | `slots` / `io/slots/ktav_doc.go:114` | `docread` / `slotdocs.go` | `ktav_write_test.go:372` (`rejectUnconvertedKtavWrite`) |
| `legacySlotOf` | `slots` / `io/slots/ktav_doc.go:140` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:397` (`legacySlotOf`) |
| `configWriteLockAnchor` | `slots` / `io/slots/ktav_doc.go:152` | `docread` / `slotdocs.go` | `apitoken_store.go:488` (`configWriteLockAnchor`) |
| `readSlotOf` | `slots` / `io/slots/ktav_doc.go:170` | `docread` / `slotdocs.go` | `settings_lock.go:34` (`readSlotOf`) |
| `oauthJournalConfigSyncParent` | `slots` / `io/slots/oauthjournal.go:16` | `ROOT` / `oauthjournal.go` | `store_oauth_journal.go:233` (`oauthJournalConfigSyncParent`) |
| `oauthJournalRecord` | `slots` / `io/slots/oauthjournal.go:19` | `docread` / `slotdocs.go` | `global_dir_isolation_test.go:107` (`oauthJournalRecord`) |
| `errOAuthJournalInvalid` | `slots` / `io/slots/oauthjournal.go:25` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:423` (`errOAuthJournalInvalid`) |
| `validOAuthJournal` | `slots` / `io/slots/oauthjournal.go:27` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:422` (`validOAuthJournal`) |
| `readOAuthJournalWithRetry` | `slots` / `io/slots/oauthjournal.go:47` | `ROOT` / `oauthjournal.go` | `io/slotmigrate/ktav_migration_artifacts.go:439` (`readOAuthJournalWithRetry`) |
| `decodeOAuthJournalDiskToken` | `slots` / `io/slots/oauthjournal.go:62` | `docread` / `slotdocs.go` | `store_oauth_disk_sync.go:105` (`decodeOAuthJournalDiskToken`) |
| `oauthJournalDigest` | `slots` / `io/slots/oauthjournal.go:99` | `docread` / `slotdocs.go` | `global_dir_isolation_test.go:61` (`oauthJournalDigest`) |
| `oauthJournalLegacyPath` | `slots` / `io/slots/oauthjournal.go:108` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:405` (`oauthJournalLegacyPath`) |
| `oauthJournalKtavPath` | `slots` / `io/slots/oauthjournal.go:116` | `docread` / `slotdocs.go` | `io/slotmigrate/ktav_migration_artifacts.go:407` (`oauthJournalKtavPath`) |
| `configCandidateGroups` | `ROOT` / `load_files.go:34` | `docread` / `candidates.go` | no external identifier-use witness in ledger |
| `lookupConfigCandidateGroups` | `ROOT` / `load_files.go:39` | `docread` / `candidates.go` | `io/docread/candidates.go:17` (`lookupConfigCandidateGroups`) |
| `configOwnersMatch` | `ROOT` / `load_files.go:90` | `docread` / `candidates.go` | `io/docread/candidates.go:45` (`configOwnersMatch`) |
| `configDocumentBytes` | `ROOT` / `load_files.go:116` | `docread` / `stabledocs.go` | `io/docread/stabledocs.go:47` (`configDocumentBytes`) |
| `addDocumentAliasFingerprints` | `ROOT` / `load_files.go:133` | `docread` / `stabledocs.go` | `io/docread/stabledocs.go:48` (`addDocumentAliasFingerprints`) |
| `errOAuthJournalDisk` | `ROOT` / `store_oauth_journal.go:22` | `docread` / `slotdocs.go` | `io/slots/oauthjournal.go:65` (`errOAuthJournalDisk`) |
| `stableReadMaxAttempts` | `ROOT` / `store_reload.go:26` | `docread` / `stabledocs.go` | `io/docread/stabledocs.go:118` (`stableReadMaxAttempts`) |
| `configFingerprintPath` | `ROOT` / `store_reload.go:31` | `docread` / `stabledocs.go` | `io/docread/fingerprints.go:49` (`configFingerprintPath`) |
| `writeConfigDiscoveryInfo` | `ROOT` / `store_reload.go:40` | `docread` / `stabledocs.go` | `io/docread/fingerprints.go:77` (`writeConfigDiscoveryInfo`) |
| `captureReloadFingerprints` | `ROOT` / `store_reload.go:549` | `docread` / `stabledocs.go` | no external identifier-use witness in ledger |
| `addReloadFingerprint` | `ROOT` / `store_reload.go:559` | `docread` / `stabledocs.go` | no external identifier-use witness in ledger |
| `readReloadFingerprint` | `ROOT` / `store_reload.go:575` | `docread` / `stabledocs.go` | `store_config_disk_sync.go:304` (`readReloadFingerprint`) |
| `reloadFingerprintsChanged` | `ROOT` / `store_reload.go:583` | `docread` / `stabledocs.go` | `config_cycle7_test.go:138` (`reloadFingerprintsChanged`) |
| `reloadFingerprintsChangedWithOwner` | `ROOT` / `store_reload.go:587` | `docread` / `stabledocs.go` | `io/docread/fingerprints.go:169` (`reloadFingerprintsChangedWithOwner`) |
| `sameReloadPathSet` | `ROOT` / `store_reload.go:612` | `docread` / `stabledocs.go` | `io/docread/fingerprints.go:167` (`sameReloadPathSet`) |
| `trackedSlotFingerprint` | `ROOT` / `store_reload.go:639` | `docread` / `stabledocs.go` | `io/docread/fingerprints.go:150` (`trackedSlotFingerprint`) |
| `reloadFingerprintForPath` | `ROOT` / `store_reload.go:646` | `docread` / `stabledocs.go` | no external identifier-use witness in ledger |
| `rebindReloadFingerprintPath` | `ROOT` / `store_reload.go:662` | `docread` / `stabledocs.go` | `store_mcp_transaction.go:633` (`rebindReloadFingerprintPath`) |
| `errKtavWriteNotSupported` | `ROOT` / `store_write.go:50` | `docread` / `stabledocs.go` | `io/slots/ktav_doc.go:116` (`errKtavWriteNotSupported`) |

Root policy/high-level names retained in ROOT include: `candidateInputsChanged`, `initialMCPRevisions`, `mcpInputFingerprints`, `mergeExternalMCPServers`, `readMCPDisabledOverride`, `mcpRevisionDiff`, `mcpNames`, `loadConfigCandidateStableForWorkingDir`, `errReloadDiskChanged`, `ktavMigrateOAuthJournalsAtStartup`, `ktavValidateOAuthJournalBytes`, `ktavMigrateOAuthJournal`, `readOAuthJournalWithRetry`, and `oauthJournalConfigSyncParent`. Root hook/config-test state remains root-owned: `configTestHooks`, `runConfigAfterMCPResolveTargetHook`, `runConfigBeforeMCPTargetBindingVerificationHook`, `SetBeforeConfigCommitForTest`, root `configWriteTargetDedupKey*`, and the five migration hook variables. The root-owned journal error getter/alias must retain one error identity; do not recreate a second `ErrOAuthJournalDisk` value.

### I. Implementation gates, graph result, and source fingerprint

Projection graph result: full graph SCC count **0**; residual SCC count after approved/manual-port handling **0**; the recorded residual topological order is `ROOT, cfgmerge, envresolve, peak, slots, slotmigrate, cfgcommit, fswap, docread, casefold, cfgmodel, datadir, fileid, cfghooks`. The current artifact has no unresolved ROOT dependencies and no residual cycle; therefore no fallback dependency port or package consolidation is warranted by this graph. The `cfghooks` provider is the specifically approved low-code snapshot boundary, not a general escape hatch. If a later compile-only check exposes an additional ROOT use, retain the root declaration and satisfy it with the smallest rule-18-style accessor/provider; do not broaden exports or pretend that this projection proves compilation.

Rule 18/19 context recorded for implementation: preserve root ownership for mutable hook/migration state and use a narrow snapshot/accessor rather than exported globals; after source moves, validate with the mandated compile-only gate (`go test -c -o NUL -gcflags=-e <package>`), not by running tests. Vet/build coverage and formatting gates described in the earlier plan remain implementation requirements. This appendix did not run Go commands, tests, builds, or vet, and makes no pass claim.

Source fingerprint: the existing body records a parent-measured identical before/after fingerprint over **353 files**, SHA-256 `52d808c35fad3f9f28dfee643e97d0563d996889fc27f0e0a9b294d7e5b964bd`. This is quoted as recorded data and was not recomputed during this documentation generation. No source edits were made by the generator.

Generator output scope: append-only update to `docs/plans/2026-11-16-sprint0-config-io-amendment.md`; `.rush/tmp/io-design/finaldoc.py` is the ignored generator. No Go source or git metadata was written.


The original IO script’s line 59 is `func must(e error)`, not a graph assignment: graph ownership corrections are applied by the executable wrapper at the `projected_owner` decision point. No declaration is misattributed to `ktav_discovery.go`.

The original IO script’s line 59 is `func must(e error)`, not a graph assignment: graph ownership corrections are applied by the executable wrapper at the `projected_owner` decision point. No declaration is misattributed to `ktav_discovery.go`.

### Appendix completion note

Appendix A supersedes the earlier reserved-for-appendix placeholder; all requested tables are present.


## L1 layout correction (2026-11-16)

The dir-entries guard `.githooks/check_dir_entries.sh` (at most 7 elements per code directory) failed on `io` (8 subpackages) and `io/cfgcommit` (9 files). The layout is therefore amended structurally with no allowlist entries, before L2, while import paths are young.

```
internal/config/io/
  cfghooks/
  fsx/                      low-level filesystem primitives
    fileid/
    casefold/
    fswap/
  docread/
  cfgcommit/
  slotmigrate/
  slots/
```

`cfgcommit` merged its untagged files `config_commit_test_seam.go` into `atomicwrite.go` and `targets.go` into `config_write_identity.go` (pure in-package merges, declaration-identical); the seven remaining `cfgcommit` files are listed.

The dependency graph edges are unchanged; only import paths gained the `fsx` segment (`io/fsx/fileid`, `io/fsx/casefold`, `io/fsx/fswap`); the re-runnable rewrite script lives at `D:/dev/go/crush/.rush/tmp/sprint0/config-io-relayout.py`.
