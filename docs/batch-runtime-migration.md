# Migrating to the batch-only runtime

Rush runs the batch runtime exclusively. The legacy execution path, its
`async_jobs` ledger and the `job_output` / `job_kill` control tools were
removed; there are no aliases and no blocking validation of unknown tool
names. This note covers what to update when upgrading.

## Tool-name replacement

| Removed legacy tool | Native replacement |
|---|---|
| `job_output` (poll a background job's output) | `batch_status` (root and task `node_id`s) and `batch_output` (paged output by cursor) |
| `job_kill` (stop a background job) | `batch_stop` (whole root, or one `node_id`) |

No compatibility aliases exist: a call to `job_output` or `job_kill`
resolves nothing at all. Unknown tool names are never an error at startup
or configuration load; they are simply inert.

## Three kinds of tool-name lists

Configuration carries tool names in three places with different
semantics. After the legacy-tool removal, each kind fails differently —
and only one of the three fails loudly anywhere:

1. **Availability in the SDK/memory (`AllowedTools`).** The resolved
   tool list a session is built from. A name here that no build
   produces anymore simply contributes nothing: the tool is absent, so
   the model cannot call it. A stale legacy name here is silently
   ineffective — the intended capability disappears rather than
   erroring.
2. **`permissions.allowed_tools` (permission bypass).** An exact
   `tool` or `tool:action` match grants the tool without a permission
   prompt. A stale legacy name never matches any executed tool call, so
   the grant becomes silently ineffective: no prompt is bypassed, and
   — depending on the run's permission mode — calls to the native
   replacement may start prompting (or being denied in
   non-interactive runs) where the legacy grant used to admit them.
3. **`options.disabled_tools` (filter membership, not validated).**
   Names here are removed from the resolved tool list; membership is a
   plain filter and is NOT validated against the build's tool set. A
   stale legacy name disables nothing, but unlike the two above it can
   leave a gap in the opposite direction: if your intent was "disable
   background-job control", the legacy `job_kill` entry no longer
   stops anything while the native replacements (`batch_stop`,
   `batch_output`, `batch_status`) remain enabled.

### What to do

Replace every `job_output` / `job_kill` occurrence in all three lists
with the native names from the table above. There is no blocking
validation that would remind you at startup: stale grants become
silently ineffective, and stale `disabled_tools` entries leave the
native replacements enabled. Targeted deprecation warnings for known
stale names are desirable and may be added later, but they are and
remain non-blocking diagnostics — never an error and never a behavior
change.

## Data and rollout

For the store migration, import/rollback semantics and the operational
steps around upgrade, see [jobbatch.md](jobbatch.md) and the
[README batch section](../README.md#batch-runtime-rollout-and-data).
