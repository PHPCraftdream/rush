# Final P3/API hygiene review status

Audited against HEAD 3e7f9df262f8cdb982cfe01059661c73bd79aa9a on
2026-09-07. Historical review text is not treated as proof of a current
defect; where a source report is absent, this record does not reconstruct it.

## Per-ID disposition

| ID | Disposition |
|---|---|
| C1-8 | Partially historical, otherwise accepted design note. 7c9637381 now caps each rendered block, lazily creates grep collectors only for paths with hits, and caches list/find result resolution. RunFSBatch still has no separate batch clock, but every item receives the caller context and grep's configured per-item timeout; the outer tool watchdog is the current 45-minute cap. No safe fixed shorter timeout is specified by the API, so no speculative timeout was added. |
| C1-9 | Historical documentation/commit-message wording issue. The old CI invocation lost cross-group -p 2 parallelism despite preserving per-step -p 2; no source behavior requires a fix. |
| C1-15 | Historical and unverifiable from this branch. The R1/R2/R3 reports cited by older commits are absent from main; this review records their absence and does not recreate them. |
| C2 CR-5 | Historical traceability issue. 2026-09-03-round-14-1328.md is present, but the separately cited 2026-09-03-sdk-library-review-round-14-0928.md remains absent and R14 identifiers were reused for incompatible reports. No unverifiable report was added. |
| C2 CR-6 | Fixed. The CallOptionsSpec doc comment now keeps + permission.BuildFolderScope inside the FolderScope bullet. |
| C2 CR-8 | Accepted bounded/security performance tradeoff. 7c9637381 only deduplicates repeated spellings; unique fs_find results are capped at 100 and fs_list uses its configured/default listing cap (default 1000), so worst-case resolution remains linear in returned results. Per-result resolveScopedPath is necessary because individual results may traverse symlinks or fall under deny carve-outs; no safe O(1) shortcut preserves those checks. |
| C3-6 | Historical documentation correction. The round-15 review now says its driveless fixture did not establish the Windows attribution. |
| C3-8 | Fixed. The helper comment now truthfully says failed observations are retried while attempts remain, with the final attempt reporting the failure; it no longer claims only timing noise can return false. |
| C3-11 | Historical documentation correction. The round-15 review now records macOS /tmp as /private/tmp, and retracts its blanket correctness attribution. |
| Chunk4 F8 | Fixed in docs. README and CHANGELOG now document fail-closed workspace trust: foreign-owned or unstatable workspace inputs fail load/reload, while missing workspace config and foreign global/project candidates retain their existing behavior. |
| F9 | Private dead helpers publishMCPConfigLocked, fingerprintForBytes, and uniqueNormalizedPaths were removed after repository-wide call-site checks. Exported Persist*AtScope/Persist*InScope wrappers remain as compatibility surface. ErrMCPAmbiguous remains the intentional alias for compatibility; no live producer uses it, so no errors.Is contract was changed. |
| F10 | Accepted design note. The 1 ms TryLock loop is cancellation-aware and avoids blocking the lifecycle path; changing it would be a synchronization redesign outside this hygiene session. |
| F11 | Fixed. renewalActive was always true and had no assignment; the deferred renewal cleanup now runs directly. Existing renewal lifecycle tests still cover the begin/end flow. |
| F12 | Accepted design note. Retired-session Close is performed after the operation reference reaches zero and after cancellation, outside operationMu; synchronous close can add tool-call latency but no correctness defect was found. |
| F13 | Fixed. The sdk.Open doc comment now names os.Chdir correctly. |
| F14 | Already fixed at current HEAD. getOrRenewClient passes operationCtx; published sessions use the handoff context in createSessionWithAdmission, and the current comments describe that flow. |
| F15 | Accepted residual compatibility debt. MCP mutation still parses and pretty-prints the complete JSON document; preserving arbitrary user formatting would require a parser/editor rewrite. No risky rewrite was attempted. |
| F16 | Fixed with regression coverage. The test uses a real file symlink whose discovery spelling differs from the physical spelling while reload normalization agrees; it binds realistic readStableConfigFile bytes, removes the physical file, then requires mcpPathData to return the transaction-bound bytes. |
| CH5-9 | Process note recorded here. Earlier empty-body commits were not amended or rewritten. The durable rationale is the current fencing chain: admission snapshots and revisions, server leases, operation references, session retirement, and context promotion together prevent stale MCP generations from publishing or being closed under live operations. |

## Additional remaining-P3 cross-check

- F5 is stale at this HEAD: the two named tests use the package Migrate
  path, while direct goose tests already set goose.SetBaseFS(FS).
- F6 is disproven by the current test shape: the handler waits for either
  request cancellation or an explicit release, and cleanup closes release
  before closing the server.
- F7 is already fixed: toCodexWrushSkillMD uses a word-bounded regexp and
  TestToCodexWrushSkillMD_DoesNotRewriteWrushFilename covers the former
  oracle gap.
- F17 is already fixed: TestInitializePublishesStdioSessionBeyondAdmission
  exercises a live stdio helper and calls both Ping and CallTool after
  initialization.

## Verification

All commands used the required GOMAXPROCS=2 and caps 600 capm 3g capc 25
capt 2 belownormal wrapper, one package per command:

Executed commands:

    $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./internal/config
    $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./internal/agent/tools
    $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./internal/agent/tools/mcp
    $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./internal/session
    $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./sdk

- caps ... go test ./internal/config — PASS (40.743s).
- caps ... go test ./internal/agent/tools — PASS (32.576s).
- caps ... go test ./internal/agent/tools/mcp — PASS (41.152s).
- caps ... go test ./internal/session — PASS (85.992s).
- caps ... go test ./sdk — PASS (22.145s).

The original verification passed; the follow-up run and its one diagnosed
Windows test flake are recorded below.

Follow-up acceptance verification:

- The first full config-package run after strengthening F16 exposed a
  Windows-only Access Denied flake in
  TestSetConfigFields_TwoStoresSameFile_BothUpdatesSurvive at iteration 37.
  The test's own two-writer oracle was sound, but its outer t.Parallel allowed
  interference from package-global commit seams. The test now runs outside
  package-level parallelism while retaining its internal concurrent writers.
- $env:GOMAXPROCS='2'; caps 600 capm 3g capc 25 capt 2 belownormal go test ./internal/config — PASS (39.631s).
