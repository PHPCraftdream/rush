# Sprint 0 config I/O L1 — lane-wide status report

**Date:** 2026-11-16  
**Scope:** Lane-wide L1 `docread` extraction/export-minimization status, incorporating source edits and gate observations from the parent and workers. This is not a report-only task summary. The committed and shared-worktree source changes are inventoried below. Parent/worker build, compile, vet, and formatting activity is distinguished from completed final gates. **No tests were executed anywhere in this lane.** This report revision itself changes only this durable document; it did not edit source, build, run tests, or create a commit.

## Result and evidence boundary

The L1 source move and export minimization are present in the shared worktree. The target has seven production Go files (tests excluded):

- `internal/config/io/docread/candidates.go`
- `internal/config/io/docread/fingerprints.go`
- `internal/config/io/docread/mcpdocs.go`
- `internal/config/io/docread/slotdocs.go`
- `internal/config/io/docread/stabledocs.go`
- `internal/config/io/docread/stabledocs_unix.go`
- `internal/config/io/docread/stabledocs_windows.go`

`internal/config/io/docread/candidates_unix_test.go` is test-only and is not included in the production-file or production-use counts. The complete current lexical exported-identifier table is embedded below so the evidence is retained in this durable report. It is lexical evidence, not compiler/type-check proof; selector counts can include false positives, and method receiver/type binding is not performed.

## Exported package identifiers

Selector occurrences count external `importAlias.Name` tokens only; `prod` excludes `_test.go`, `test` includes them. Up to 3 `file:line` examples per class; zero-use names are retained.

| Export | Prod n / examples | Test n / examples |
|---|---:|---:|
| `(FileSnapshot).Fingerprint` | unavailable (receiver binding required) | unavailable (receiver binding required) |
| `AddDocumentAliasFingerprints` | 1 - `internal/config/stabledocs_policy.go:28` | 0 - none |
| `BackupPathFor` | 10 - `internal/config/io/slotmigrate/ktav_migration.go:90`, `internal/config/io/slotmigrate/ktav_migration.go:138`, `internal/config/io/slotmigrate/ktav_migration.go:412` | 1 - `internal/config/docread_testsupport_test.go:87` |
| `CheckAuthoritativeBackupOnly` | 2 - `internal/config/load.go:112`, `internal/config/store_reload.go:165` | 1 - `internal/config/docread_testsupport_test.go:102` |
| `CheckBackupOnlySlot` | 4 - `internal/config/io/slots/ktav_light.go:23`, `internal/config/load.go:148`, `internal/config/store_oauth.go:384` | 1 - `internal/config/docread_testsupport_test.go:99` |
| `ConfigAliasChainFingerprint` | 6 - `internal/config/io/slotmigrate/ktav_migration.go:312`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:306`, `internal/config/io/slotmigrate/ktav_migration_symlink.go:212` | 1 - `internal/config/docread_testsupport_test.go:34` |
| `ConfigAndMCPStalenessPaths` | 2 - `internal/config/store.go:397`, `internal/config/store_reload.go:333` | 0 - none |
| `ConfigDiscoveryFingerprint` | 20 - `internal/config/io/cfgcommit/config_commit_unix.go:57`, `internal/config/io/cfgcommit/config_commit_windows.go:46`, `internal/config/io/cfgcommit/config_commit_windows.go:229` | 0 - none |
| `ConfigDocumentBytes` | 1 - `internal/config/stabledocs_policy.go:27` | 1 - `internal/config/docread_testsupport_test.go:18` |
| `ConfigOwnerForWorkingDir` | 5 - `internal/config/load.go:176`, `internal/config/store_mcp_transaction.go:615`, `internal/config/store_mcp_transaction.go:677` | 0 - none |
| `ConfigOwnerPolicyForPath` | 4 - `internal/config/fingerprints_policy.go:31`, `internal/config/io/slotmigrate/ktav_migration.go:482`, `internal/config/io/slotmigrate/ktav_migration.go:488` | 1 - `internal/config/docread_testsupport_test.go:30` |
| `ConfigWriteLockAnchor` | 4 - `internal/config/apitoken_store.go:492`, `internal/config/store_oauth_journal_worker.go:34`, `internal/config/store_oauth_journal_worker.go:237` | 1 - `internal/config/docread_testsupport_test.go:107` |
| `DecodeConfigDocument` | 11 - `internal/config/io/slots/ktav_light.go:32`, `internal/config/load.go:183`, `internal/config/provider.go:88` | 1 - `internal/config/docread_testsupport_test.go:82` |
| `DecodeSettingsDocument` | 7 - `internal/config/apitoken_store.go:409`, `internal/config/provider_peak_hours_read.go:42`, `internal/config/settings_lock.go:502` | 1 - `internal/config/docread_testsupport_test.go:133` |
| `EligibleConfigCandidate` | 2 - `internal/config/store_mcp_transaction.go:596`, `internal/config/store_mcp_transaction.go:615` | 1 - `internal/config/docread_testsupport_test.go:22` |
| `EncodeSettingsDocument` | 7 - `internal/config/apitoken_store.go:528`, `internal/config/settings_lock.go:514`, `internal/config/store_mcp_transaction.go:352` | 1 - `internal/config/docread_testsupport_test.go:141` |
| `ErrConfigReloadUnstable` | 2 - `internal/config/facade.go:403`, `internal/config/load.go:108` | 0 - none |
| `ErrKtavBackupOnly` | 4 - `internal/config/io/slotmigrate/ktav_migration.go:150`, `internal/config/io/slotmigrate/ktav_migration.go:519`, `internal/config/io/slotmigrate/ktav_migration.go:553` | 1 - `internal/config/docread_testsupport_test.go:114` |
| `ErrKtavMigrationConflict` | 34 - `internal/config/io/slotmigrate/ktav_migration.go:183`, `internal/config/io/slotmigrate/ktav_migration.go:185`, `internal/config/io/slotmigrate/ktav_migration.go:193` | 1 - `internal/config/docread_testsupport_test.go:115` |
| `ErrKtavMigrationRecord` | 19 - `internal/config/io/slotmigrate/ktav_migration.go:351`, `internal/config/io/slotmigrate/ktav_migration.go:397`, `internal/config/io/slotmigrate/ktav_migration.go:399` | 1 - `internal/config/docread_testsupport_test.go:116` |
| `ErrOAuthJournalInvalid` | 3 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:424`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:442`, `internal/config/store_oauth_journal.go:24` | 0 - none |
| `ExpandConfigSlotSiblings` | 2 - `internal/config/store_config_disk_sync.go:298`, `internal/config/store_staleness.go:147` | 0 - none |
| `FileExists` | 5 - `internal/config/io/slotmigrate/ktav_migration.go:512`, `internal/config/io/slotmigrate/ktav_migration.go:515`, `internal/config/io/slotmigrate/ktav_migration.go:548` | 1 - `internal/config/docread_testsupport_test.go:109` |
| `FileSnapshot` | 3 - `internal/config/fingerprints_policy.go:16`, `internal/config/store.go:47`, `internal/config/store_staleness.go:97` | 0 - none |
| `Fingerprint` | 136 - `internal/config/fingerprints_policy.go:15`, `internal/config/io/cfgcommit/config_commit_unix.go:23`, `internal/config/io/cfgcommit/config_commit_unix.go:23` | 0 - none |
| `IsKtavBackupPath` | 1 - `internal/config/io/slotmigrate/ktav_migration.go:509` | 0 - none |
| `IsKtavConfigPath` | 5 - `internal/config/fingerprints_policy.go:79`, `internal/config/io/slotmigrate/ktav_migration.go:509`, `internal/config/io/slots/ktav_doc.go:17` | 1 - `internal/config/docread_testsupport_test.go:91` |
| `IsKtavMigrationRecordPath` | 1 - `internal/config/io/slotmigrate/ktav_migration.go:509` | 0 - none |
| `IsMCPDisabledOnlyEntry` | 3 - `internal/config/mcpdocs_policy.go:45`, `internal/config/store_mcp_transaction.go:719`, `internal/config/store_mcp_transaction.go:765` | 0 - none |
| `KtavEntryAbsentErr` | 1 - `internal/config/io/slots/ktav_cache.go:29` | 0 - none |
| `KtavEntryExists` | 27 - `internal/config/io/slotmigrate/ktav_migration.go:88`, `internal/config/io/slotmigrate/ktav_migration.go:89`, `internal/config/io/slotmigrate/ktav_migration.go:90` | 1 - `internal/config/docread_testsupport_test.go:105` |
| `KtavMigrationPendingSlots` | 1 - `internal/config/io/slotmigrate/ktav_migration.go:500` | 0 - none |
| `KtavSiblingOf` | 24 - `internal/config/io/slotmigrate/ktav_migration.go:89`, `internal/config/io/slotmigrate/ktav_migration.go:141`, `internal/config/io/slotmigrate/ktav_migration.go:216` | 1 - `internal/config/docread_testsupport_test.go:85` |
| `LegacySlotOf` | 5 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:398`, `internal/config/io/slots/ktav_light.go:22`, `internal/config/io/slots/ktav_light.go:42` | 0 - none |
| `LoadExternalMCPDocumentsStableForWorkingDir` | 2 - `internal/config/load.go:238`, `internal/config/store_reload.go:251` | 0 - none |
| `LoadMCPJSONBytes` | 1 - `internal/config/store_mcp_transaction.go:571` | 0 - none |
| `LookupConfigCandidateGroups` | 4 - `internal/config/load.go:122`, `internal/config/load.go:372`, `internal/config/load_files.go:20` | 0 - none |
| `LookupConfigCandidates` | 6 - `internal/config/fingerprints_policy.go:24`, `internal/config/io/slotmigrate/ktav_migration.go:508`, `internal/config/store_config_disk_sync.go:292` | 1 - `internal/config/docread_testsupport_test.go:14` |
| `MatchesLogicalSlot` | 9 - `internal/config/io/slotmigrate/ktav_migration.go:481`, `internal/config/io/slotmigrate/ktav_migration.go:481`, `internal/config/io/slotmigrate/ktav_migration.go:481` | 1 - `internal/config/docread_testsupport_test.go:97` |
| `McpEntryFromJSON` | 2 - `internal/config/mcpdocs_policy.go:44`, `internal/config/store_mcp_transaction.go:764` | 1 - `internal/config/docread_testsupport_test.go:78` |
| `McpJSONCandidatePaths` | 5 - `internal/config/fingerprints_policy.go:25`, `internal/config/load.go:237`, `internal/config/store_config_disk_sync.go:293` | 1 - `internal/config/docread_testsupport_test.go:74` |
| `MergeExternalMCPServersFromDocuments` | 2 - `internal/config/load.go:245`, `internal/config/store_reload.go:255` | 1 - `internal/config/docread_testsupport_test.go:145` |
| `MigrationRecordPathFor` | 9 - `internal/config/io/slotmigrate/ktav_migration.go:91`, `internal/config/io/slotmigrate/ktav_migration.go:139`, `internal/config/io/slotmigrate/ktav_migration.go:522` | 1 - `internal/config/docread_testsupport_test.go:89` |
| `NewFileSnapshot` | 3 - `internal/config/store_mcp.go:399`, `internal/config/store_mcp_transaction.go:890`, `internal/config/store_staleness.go:104` | 1 - `internal/config/store_config_disk_sync_test.go:87` |
| `NormalizeDiscoveryPath` | 30 - `internal/config/fingerprints_policy.go:67`, `internal/config/fingerprints_policy.go:114`, `internal/config/load.go:146` | 1 - `internal/config/docread_testsupport_test.go:58` |
| `NormalizeReloadPath` | 84 - `internal/config/fingerprints_policy.go:28`, `internal/config/fingerprints_policy.go:28`, `internal/config/fingerprints_policy.go:65` | 1 - `internal/config/docread_testsupport_test.go:54` |
| `OAuthJournalDigest` | 2 - `internal/config/io/slots/oauthjournal.go:10`, `internal/config/store_oauth_journal.go:94` | 0 - none |
| `OAuthJournalKtavPath` | 5 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:408`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:409`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:438` | 0 - none |
| `OAuthJournalLegacyPath` | 4 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:406`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:437`, `internal/config/io/slots/ktav_oauth.go:17` | 0 - none |
| `OAuthJournalRecord` | 11 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:422`, `internal/config/oauthjournal_policy.go:14`, `internal/config/oauthjournal_policy.go:17` | 0 - none |
| `PathAlreadyLoaded` | 2 - `internal/config/load.go:175`, `internal/config/store_reload.go:191` | 1 - `internal/config/docread_testsupport_test.go:26` |
| `ReadConfigEntryAt` | 4 - `internal/config/io/cfgcommit/config_linkat_unix.go:89`, `internal/config/io/cfgcommit/config_linkat_unix.go:136`, `internal/config/io/slotmigrate/ktav_migration_unix.go:55` | 0 - none |
| `ReadReloadFingerprint` | 5 - `internal/config/store_config_disk_sync.go:306`, `internal/config/store_config_disk_sync.go:334`, `internal/config/store_mcp_transaction.go:878` | 1 - `internal/config/docread_testsupport_test.go:42` |
| `ReadSettingsDocument` | 9 - `internal/config/mcpdocs_policy.go:40`, `internal/config/settings_lock.go:174`, `internal/config/settings_lock.go:279` | 1 - `internal/config/docread_testsupport_test.go:137` |
| `ReadSlotOf` | 9 - `internal/config/settings_lock.go:35`, `internal/config/settings_lock.go:37`, `internal/config/settings_lock_sources.go:19` | 0 - none |
| `ReadStableConfigDocumentsWithOwner` | 1 - `internal/config/stabledocs_policy.go:11` | 1 - `internal/config/docread_testsupport_test.go:62` |
| `ReadStableConfigFile` | 1 - `internal/config/settings_models_integrity.go:46` | 1 - `internal/config/docread_testsupport_test.go:70` |
| `ReadStableConfigFileOwned` | 40 - `internal/config/apitoken_store.go:400`, `internal/config/io/cfgcommit/config_commit_unix.go:27`, `internal/config/io/cfgcommit/config_commit_unix.go:128` | 1 - `internal/config/docread_testsupport_test.go:66` |
| `ReadStableConfigHandle` | 6 - `internal/config/io/cfgcommit/config_commit_windows.go:109`, `internal/config/io/cfgcommit/config_commit_windows.go:222`, `internal/config/io/cfgcommit/config_commit_windows.go:278` | 0 - none |
| `RebindReloadFingerprintPath` | 1 - `internal/config/store_mcp_transaction.go:634` | 0 - none |
| `ReloadFingerprintsChangedWithOwner` | 1 - `internal/config/fingerprints_policy.go:26` | 1 - `internal/config/docread_testsupport_test.go:46` |
| `ReloadStalenessState` | 2 - `internal/config/store.go:399`, `internal/config/store_reload.go:335` | 1 - `internal/config/docread_testsupport_test.go:38` |
| `SameBytesFingerprint` | 24 - `internal/config/io/cfgcommit/config_commit_unix.go:37`, `internal/config/io/cfgcommit/config_commit_unix.go:130`, `internal/config/io/cfgcommit/config_commit_unix.go:188` | 0 - none |
| `SameReloadPathSet` | 3 - `internal/config/fingerprints_policy.go:24`, `internal/config/fingerprints_policy.go:25`, `internal/config/store_config_disk_sync.go:214` | 0 - none |
| `SelectConfigSlot` | 5 - `internal/config/io/slots/ktav_doc.go:20`, `internal/config/io/slots/ktav_light.go:26`, `internal/config/io/slots/ktav_light.go:42` | 1 - `internal/config/docread_testsupport_test.go:93` |
| `SettingsDocument` | 4 - `internal/config/settings_lock.go:493`, `internal/config/settings_lock.go:496`, `internal/config/store_write.go:435` | 3 - `internal/config/docread_testsupport_test.go:121`, `internal/config/docread_testsupport_test.go:132`, `internal/config/docread_testsupport_test.go:136` |
| `SettingsDocumentFormat` | 0 - none | 3 - `internal/config/docread_testsupport_test.go:119`, `internal/config/docread_testsupport_test.go:128`, `internal/config/docread_testsupport_test.go:140` |
| `SettingsDocumentFormatOf` | 5 - `internal/config/apitoken_store.go:527`, `internal/config/settings_lock.go:496`, `internal/config/store_mcp_transaction.go:352` | 1 - `internal/config/docread_testsupport_test.go:129` |
| `SettingsDocumentJSON` | 0 - none | 1 - `internal/config/docread_testsupport_test.go:124` |
| `SettingsDocumentKtav` | 0 - none | 1 - `internal/config/docread_testsupport_test.go:125` |
| `StableConfigAlias` | 3 - `internal/config/fingerprints_policy.go:18`, `internal/config/load.go:193`, `internal/config/load.go:202` | 0 - none |
| `StableConfigDocument` | 8 - `internal/config/fingerprints_policy.go:17`, `internal/config/load.go:190`, `internal/config/load.go:200` | 0 - none |
| `TrackedSlotFingerprint` | 2 - `internal/config/store_mcp_admission.go:686`, `internal/config/store_staleness.go:35` | 1 - `internal/config/docread_testsupport_test.go:50` |
| `ValidOAuthJournal` | 3 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:423`, `internal/config/store_oauth_journal.go:113`, `internal/config/store_oauth_journal.go:143` | 0 - none |
| `WarnBothFormatsOnce` | 1 - `internal/config/io/slots/ktav_cache.go:43` | 0 - none |
| `WarnKtavArtifactConflictOnce` | 2 - `internal/config/io/slotmigrate/ktav_migration_artifacts.go:409`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:505` | 0 - none |
| `WarnMigrationDeferredOnce` | 8 - `internal/config/io/slotmigrate/ktav_migration.go:525`, `internal/config/io/slotmigrate/ktav_migration.go:554`, `internal/config/io/slotmigrate/ktav_migration_artifacts.go:155` | 0 - none |
| `WorkspaceConfigPath` | 3 - `internal/config/load.go:146`, `internal/config/load.go:155`, `internal/config/store_reload.go:186` | 1 - `internal/config/docread_testsupport_test.go:95` |


## Grouped exported field boundary

This is the actual Go visibility boundary for struct fields in the moved package. Private JSON decode structs are internal serialization shapes, not externally exposed API types. `FileSnapshot` keeps its fingerprint value in a private field and exposes an accessor.

| Type | Exported fields | Boundary note |
|---|---|---|
| `FileSnapshot` | `Path string`; `Exists bool`; `Size int64`; `ModTime int64`; `ContentHash [32]byte` | `fingerprint Fingerprint` is private. `Fingerprint() Fingerprint` is the exported accessor to that private value. |
| `Fingerprint` | `Exists bool`; `Size int64`; `ModTime int64`; `Digest [sha256.Size]byte`; `Owner int`; `Nlink uint64`; `Identity fileid.FileIdentity`; `Discovery [sha256.Size]byte`; `ParentDiscovery [sha256.Size]byte`; `ParentIdentity fileid.FileIdentity`; `AliasChain [sha256.Size]byte` | Exported value fields. |
| `StableConfigDocument` | `Path string`; `Data []byte`; `Fingerprint Fingerprint`; `Present bool`; `Aliases []StableConfigAlias` | Exported document boundary. |
| `StableConfigAlias` | `Path string`; `Fingerprint Fingerprint` | Exported alias boundary. |
| `SettingsDocument` | `Path string`; `JSONBytes []byte`; `RawBytes []byte`; `RawFingerprint Fingerprint`; `Format SettingsDocumentFormat` | Exported document boundary. |
| `OAuthJournalRecord` | `PredecessorRefreshTokenSHA256 string`; `Token *oauth.Token`; `BurnedRefreshTokenSHA256 []string` | Exported record boundary. |
| `configCandidateGroups` | `Global []string`; `Project []string` | Fields are exported, but the containing type is unexported; the exported function's implicit return does not make this a nameable external type. |
| `mcpJSONFile` / `mcpJSONEntry` | `MCPServers`; `Type`, `Command`, `Args`, `Env`, `URL`, `Headers` | Containing types are unexported. These fields support JSON serialization/deserialization only and are not an external Go API boundary. |

A repository `rg '\.Fingerprint\(\)' internal/config` count found 8 production selector calls and 8 test selector calls. The generated table marks `(FileSnapshot).Fingerprint` usage unavailable because receiver binding is required; the direct selector count is separate evidence.

## Test relocation

The exact-body test `TestWorkspaceConfigRejectsForeignOwnedFile` moved from `internal/config/load_discovery_owner_unix_test.go` to `internal/config/io/docread/candidates_unix_test.go`. Assertions and test body are unchanged. Any reset removals are limited to fresh `t.TempDir`-keyed state; there are no fixture byte-copies. This records the move only; the test was not run.

## Gate status

Statuses below record parent-supplied actual successful gate results (exit 0); this report revision did not rerun Go commands. Test compilation is compile-only and does not execute tests. No test execution occurred.

| Gate | Observed status |
|---|---|
| L0 `cfghooks` / `fileid` / `casefold` | Parent reports build and compile gates passed; vet green on Linux, Darwin, and Windows. Unresolved L0 test-ownership/fixture warnings remain. |
| L1 `fileid` Windows move | **PASS (exit 0):** Windows move build, native compile, and Windows vet passed. |
| L1 `docread` before final export minimization | Parent reports build/native compile and vet green on all three OSes; this predates final export minimization. |
| L1 `docread` after final export minimization | **PASS (exit 0):** `go build`, native compile with the same compile flags, and vet passed; vet was green for `GOOS=linux`, `darwin`, and `windows`. |
| Moved test compilation | **PASS (exit 0):** `go test -c -o NUL -gcflags=-e -ldflags="-s -w"` and `go vet` passed for `GOOS=linux`, `darwin`, and `windows`. Compile-only; no tests were executed. |
| Formatting / whitespace | Parent reports the latest full-checkpoint formatting check clean and `git diff --check` clean. |
| Root package | **BLOCKED** by later dependency crossings involving `cfgcommit` (`configWriteTarget`, `configCaseSensitivity`, `configFileIdentity`, `windowsStagedConfigFile`, `renameConfigTemp`) and `slots` (`ktavAutoMigrate`). No root compile pass is claimed. |

Unresolved L0 test-ownership/fixture defects involving private `fileid`/`casefold` fixtures remain unresolved; this report does not claim they are fixed. No test functions or test binaries were executed.

## Final gate evidence and scope

The successful gate results above were supplied by the parent as actual exit-0 outcomes. They cover the named packages and targets only; they do not establish a root-package compile. The moved-test cross-target command was compile-only:

```text
GOOS=<linux|darwin|windows> bash D:/dev/go/crush/.rush/tmp/golock.sh go build ./internal/config/io/docread
GOOS=<linux|darwin|windows> bash D:/dev/go/crush/.rush/tmp/golock.sh go test -c -o NUL -gcflags=-e -ldflags="-s -w" ./internal/config/io/docread
GOOS=<linux|darwin|windows> bash D:/dev/go/crush/.rush/tmp/golock.sh go vet ./internal/config/io/docread
```

The same build, compile-only, and vet gates were run individually for `./internal/config/io/cfghooks`, `./internal/config/io/fileid`, and `./internal/config/io/casefold`.

The final minimized `docread` gate comprised `go build`, native compile with the same compile flags, and `go vet` for Linux, Darwin, and Windows. The `fileid` move gate comprised Windows build, native compile, and Windows vet. No tests or test binaries were executed. The root remains blocked on later dependencies as listed above; unresolved L0 test-ownership/fixture warnings remain unresolved.

## Git-readonly change inventory

The inventory below comes from read-only Git output in the worktree. The L0 checkpoint is commit `aea8285bf0fe3108d44f79232395a1d80e293b34`; current HEAD is `a43cbd472e118d2643abe6d3917a0bdc6bc101c6`. The generated evidence records these two ranges too. The following lists describe the config implementation delta, not this report/evidence deliverables.

### Committed after L0 through HEAD (40 paths; `aea8285..HEAD`)

```text
M internal/config/apitoken_store.go
M internal/config/facade.go
A internal/config/fingerprints_policy.go
M internal/config/io/cfgcommit/atomicwrite.go
M internal/config/io/docread/candidates.go
M internal/config/io/docread/fingerprints.go
M internal/config/io/docread/mcpdocs.go
D internal/config/io/docread/normalize.go
A internal/config/io/docread/slotdocs.go
M internal/config/io/docread/stabledocs.go
A internal/config/io/docread/stabledocs_unix.go
M internal/config/io/docread/stabledocs_windows.go
M internal/config/io/fileid/identity_unix.go
M internal/config/io/fileid/identity_windows.go
D internal/config/io/slots/ktav_discovery.go
M internal/config/io/slots/ktav_doc.go
M internal/config/io/slots/oauthjournal.go
M internal/config/load.go
M internal/config/load_files.go
A internal/config/mcpdocs_policy.go
A internal/config/oauthjournal_policy.go
M internal/config/provider_peak_hours_read.go
M internal/config/settings_lock.go
M internal/config/settings_lock_sources.go
M internal/config/settings_lock_sources_windows.go
M internal/config/settings_models_integrity.go
A internal/config/stabledocs_policy.go
M internal/config/store.go
M internal/config/store_config_disk_sync.go
M internal/config/store_custom_roles.go
M internal/config/store_mcp.go
M internal/config/store_mcp_admission.go
M internal/config/store_mcp_locks.go
M internal/config/store_mcp_transaction.go
M internal/config/store_oauth.go
M internal/config/store_oauth_disk_sync.go
M internal/config/store_oauth_journal.go
M internal/config/store_reload.go
M internal/config/store_staleness.go
M internal/config/store_write.go
```

### Worktree relative to HEAD before report-deliverable creation (60 paths)

```text
M internal/config/apitoken_store.go
M internal/config/config_commit_windows_test.go
M internal/config/config_cycle7_test.go
M internal/config/config_cycle9_test.go
M internal/config/config_durability_unix_test.go
M internal/config/config_regression_test.go
M internal/config/config_rename_windows_test.go
M internal/config/config_sidecar_identity_test.go
M internal/config/custom_roles_integrity_test.go
M internal/config/custom_roles_write_test.go
M internal/config/facade.go
M internal/config/io/cfgcommit/atomicwrite.go
M internal/config/io/cfgcommit/config_commit_unix.go
M internal/config/io/cfgcommit/config_commit_windows.go
M internal/config/io/cfgcommit/config_linkat_unix.go
M internal/config/io/docread/candidates.go
M internal/config/io/docread/fingerprints.go
M internal/config/io/docread/slotdocs.go
M internal/config/io/fileid/identity_unix.go
M internal/config/io/fileid/identity_windows.go
D internal/config/io/fswap/config_open_unix.go
D internal/config/io/fswap/config_open_windows.go
M internal/config/io/fswap/config_rename_windows.go
M internal/config/io/slotmigrate/ktav_migration.go
M internal/config/io/slotmigrate/ktav_migration_artifacts.go
M internal/config/io/slotmigrate/ktav_migration_symlink.go
M internal/config/io/slotmigrate/ktav_migration_unix.go
M internal/config/io/slotmigrate/ktav_migration_windows.go
M internal/config/io/slots/ktav_cache.go
M internal/config/io/slots/ktav_light.go
M internal/config/io/slots/ktav_oauth.go
M internal/config/ktav_both_formats_test.go
M internal/config/ktav_discovery_test.go
M internal/config/ktav_migration_artifacts_test.go
M internal/config/ktav_migration_symlink_test.go
M internal/config/load_discovery_owner_unix_test.go
M internal/config/mcp_admission_io_test.go
M internal/config/model_catalog_cache.go
M internal/config/oauthjournal_policy.go
M internal/config/provider.go
M internal/config/server_listeners.go
M internal/config/settings_lock_sources_other.go
M internal/config/store_config_disk_sync_test.go
M internal/config/store_mcp.go
M internal/config/store_mcp_admission.go
M internal/config/store_mcp_admission_sibling_fence_test.go
M internal/config/store_mcp_identity_unix_test.go
M internal/config/store_mcp_test.go
M internal/config/store_mcp_transaction.go
M internal/config/store_oauth_journal.go
M internal/config/store_oauth_journal_outbox.go
M internal/config/store_oauth_journal_worker.go
M internal/config/store_oauth_refresh.go
M internal/config/store_reload.go
M internal/config/store_reload_test.go
M internal/config/store_reload_windows_test.go
M internal/config/store_write_durability_test.go
M internal/config/store_write_reload_fixture_test.go
? internal/config/docread_testsupport_test.go
? internal/config/io/docread/candidates_unix_test.go
```

`?` denotes untracked at the time of the read-only status snapshot. The test-body relocation above accounts for the deletion from the root test and addition under `docread`; the lane-specific test remains test-only.

## Reproduction / authorization

The lexical evidence was generated with:

```text
python .rush/tmp/io-design/l1-evidence.py > .rush/tmp/io-design/l1-report.md
```

This reproduces lexical evidence only; it is not compiler/type-check proof. The command record above distinguishes completed parent-reported gates from root-package work that remains blocked. **No tests were executed anywhere in the lane.** This report-only revision made no source edits, ran no build or test command, and created no commit; it updates the final gate statuses only. Exported-identifier, grouped exported-field, production-file, and Git inventory tables are unchanged. The concrete report diff is limited to recording the exit-0 `fileid` Windows move gates, moved-test compile/vet gates for all three OSes, final minimized `docread` build/compile/vet gates, and the unchanged root-blocked and L0-warning boundaries.
