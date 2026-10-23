# .github/scripts — tiered CI selector (shadow mode)

`ci_plan.py` implements design section 2 of the tiered-CI spec: given
`--base`/`--head` SHAs it diffs merge-base..head, maps changed files to Go
packages (including synthetic test packages, embeds, `testdata/`, SQL/migrations
via `.github/ci-policy.toml` mappings, deleted files via base-tree ownership),
builds reverse-dependency closures over unioned per-OS `go list` graphs, and
prints a plan: selected packages per OS (linux/darwin/windows) with reasons,
race selection, `native_required`, full-fallback reasons, and an exhaustive
`not_run` list.

**Shadow mode: it gates nothing and never fails a PR on plan content.** It
exits nonzero only on planning failure (bad SHAs, malformed go-list metadata,
failed package enumeration) and never emits an empty plan.

## Files

- `ci_plan.py` — thin entry point (CLI).
- `ci_plan/` — stdlib-only implementation package (Python 3.11+; `tomllib` OK).
  - `gitinput.py` — merge-base, NUL-delimited name-status diffs (incl. R/C both
    paths), base-tree listing.
  - `golist.py` — streaming parse of concatenated `go list -deps -test -json`
    objects; one process per OS with GOOS/GOARCH/CGO_ENABLED env (never
    per-package, never `-e`); synthetic test-package normalization; reverse
    edges from Imports/TestImports/XTestImports.
  - `selector.py` — selection algorithm, fallbacks, race + native decisions.
  - `report.py` — versioned plan JSON and markdown summary.
- `../ci-policy.toml` — all thresholds and path lists (native prefixes, core
  prefixes, race roots, fan-out limits, non-Go input mappings, always_run).

## Run

```sh
python -I -m unittest discover -s .github/scripts
python .github/scripts/ci_plan.py --base <sha> --head <sha> --dry-run
# with pre-extracted metadata (recommended in CI):
go list -deps -test -json ./... > /tmp/linux.json   # per GOOS/GOARCH
python .github/scripts/ci_plan.py --base <sha> --head <sha> \
  --go-list-json linux=/tmp/linux.json --out plan.json --summary summary.md
```

The shadow workflow (`.github/workflows/ci-plan.yml`, added by a later chunk)
runs the selector on every PR and uploads/prints the plan for comparison
against current full CI. Rollback = delete `ci-plan.yml` (and these scripts).

## Phase plan

- Phase 1 (this): selector + policy + shadow planning only.
- Phase 2 (NOT here): tier jobs, Windows shards, `ci-gate` required check,
  `full-ci` label semantics, reusable test/security workflows.
