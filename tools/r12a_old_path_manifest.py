#!/usr/bin/env python3
"""R12 old-path cutover: uncapped tracked-file inventory manifest generator.

Deterministic, stdlib-only, no network, NOT run in CI.
Regenerates docs/research/2026-10-14-r12-old-path-manifest.md from the
current working tree. Do not hand-edit the generated report.
"""
import collections
import os
import re
import subprocess
import sys
from datetime import date

FIXED_DATE = "2026-10-14"
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_PATH = os.path.join(
    REPO_ROOT, "docs", "research", f"{FIXED_DATE}-r12-old-path-manifest.md"
)
TOP_DIRS = ["internal", "sdk", "web", "docs", ".github", ".githooks"]
PATTERN = (
    r"async_jobs|AsyncJob|asyncJob|work_ledger|workLedger|WorkLedger|awaitSync|"
    r"job_kill|job_output|stallDetach|stallDetacher|stall-|RUSH_BATCH_RUNTIME"
)
GREP_CMD = (
    "git grep -n -I -E "
    "'async_jobs|AsyncJob|asyncJob|work_ledger|workLedger|WorkLedger|awaitSync|"
    "job_kill|job_output|stallDetach|stallDetacher|stall-|RUSH_BATCH_RUNTIME' "
    "-- internal sdk web docs .github .githooks"
)

# (filename regex (anchored at end of path or basename) or full-path regex ->
#   family). A file may match several families; all matches are recorded.
# Order matters only for `unknown` fallback: if no rule matches, unknown.
RULES = [
    # ledger_terminal_authority: delete
    (r"(^|/)work_ledger[^/]*\.go$", "ledger_terminal_authority"),
    (r"(^|/)work_job\.go$", "ledger_terminal_authority"),
    (r"(^|/)async_tool[^/]*\.go$", "ledger_terminal_authority"),
    (r"(^|/)bgshell_claim\.go$", "ledger_terminal_authority"),
    (r"(^|/)delegation_question\.go$", "ledger_terminal_authority"),
    # ledger_consumers: port
    (r"(^|/)coordinator_[^/]*\.go$", "ledger_consumers"),
    (r"(^|/)turn_arbiter\.go$", "ledger_consumers"),
    (r"(^|/)drain_attempt\.go$", "ledger_consumers"),
    (r"(^|/)supervision\.go$", "ledger_consumers"),
    (r"(^|/)agent_notice_pull\.go$", "ledger_consumers"),
    (r"(^|/)agent\.go$", "ledger_consumers"),
    (r"(^|/)agent_options\.go$", "ledger_consumers"),
    (r"(^|/)agent_turn[^/]*\.go$", "ledger_consumers"),
    # sql_sqlc: port (split hosts first)
    (r"^internal/db/sql/async_jobs", "sql_sqlc"),
    (r"^internal/db/async_jobs\.sql\.go$", "sql_sqlc"),
    # other generated internal/db/*.sql.go files (messages.sql.go, db.go, ...)
    # intentionally NOT matched here: they fall through to unknown/review.
    (r"(^|/)models\.go$", "sql_sqlc"),
    (r"(^|/)querier\.go$", "sql_sqlc"),
    # session_old_store: delete
    (r"^internal/session/async_job_[^/]*\.go$", "session_old_store"),
    # retained_infrastructure: retain
    (r"(^|/)notice_pull\.go$", "retained_infrastructure"),
    (r"(^|/)notice_row_ids\.go$", "retained_infrastructure"),
    (r"(^|/)session_driver\.go$", "retained_infrastructure"),
    (r"(^|/)session_child_question\.go$", "retained_infrastructure"),
    (r"(^|/)host_lock\.go$", "retained_infrastructure"),
    (r"(^|/)wake_schedule_[^/]*\.go$", "retained_infrastructure"),
    (r"(^|/)wake_scheduler\.go$", "retained_infrastructure"),
    # stall_jobs: delete (stall clock/watchdog policy retained separately)
    (r"(^|/)turn_stall_tool_detach\.go$", "stall_jobs"),
    (r"(^|/)turn_stall_policy\.go$", "stall_jobs"),
    (r"(^|/)turn_stall_[^/]*test[^/]*\.go$", "stall_jobs"),
    (r"(^|/)stall_detach_controller\.go$", "stall_jobs"),
    # old_model_tools: delete
    (r"^internal/agent/tools/job_[^/]*\.go$", "old_model_tools"),
    (r"(^|/)job_kill\.md$", "old_model_tools"),
    (r"(^|/)job_output\.md$", "old_model_tools"),
    (r"(^|/)restricted_stall_detach_test\.go$", "old_model_tools"),
    (r"(^|/)job_run_command_control_test\.go$", "old_model_tools"),
    (r"(^|/)r16_background_ownership_test\.go$", "old_model_tools"),
    # prompts_tool_inventories: port
    (r"^internal/config/config\.go$", "prompts_tool_inventories"),
    (r"(^|/)batch_allowed_controls\.go$", "prompts_tool_inventories"),
    (r"(^|/)templates/[^/]*\.md\.tpl$", "prompts_tool_inventories"),
    (r"(^|/)prompt/prompt\.go$", "prompts_tool_inventories"),
    (r"(^|/)tools/bash[^/]*$", "prompts_tool_inventories"),
    (r"(^|/)run_command\.md\.tpl$", "prompts_tool_inventories"),
    (r"(^|/)await_tasks\.md$", "prompts_tool_inventories"),
    # app_lifecycle: port
    (r"^internal/app/[^/]*\.go$", "app_lifecycle"),
    (r"(^|/)async_phase[^/]*\.go$", "app_lifecycle"),
    (r"(^|/)async_scenarios[^/]*\.go$", "app_lifecycle"),
    (r"(^|/)app_run[^/]*test[^/]*\.go$", "app_lifecycle"),
    # cli_sessions: port
    (r"^internal/cmd/sessions_[^/]*\.go$", "cli_sessions"),
    # server_rerun: port
    (r"^internal/server/handlers_[^/]*\.go$", "server_rerun"),
    # web_completions: port
    (r"^web/src/", "web_completions"),
    # sdk_overrides: retain
    (r"^sdk/sdk\.go$", "sdk_overrides"),
    # switch: delete
    (r"(^|/)batch_runtime_flag[^/]*\.go$", "switch"),
    (r"(^|/)batch_cli\.go$", "switch"),
    # migration_and_testdata: retain
    (r"^internal/db/migrate_[^/]*\.go$", "migration_and_testdata"),
    (r"^internal/db/testdata/", "migration_and_testdata"),
    (r"(^|/)migrations/", "migration_and_testdata"),
    # historical_docs: historical
    (r"^docs/plans/", "historical_docs"),
    (r"^docs/research/", "historical_docs"),
    (r"^docs/[^/]*\.md$", "historical_docs"),
    # filelock_probe: retain
    (r"^internal/filelock/host_probe", "filelock_probe"),
    (r"^internal/db/migrate_live_hosts", "filelock_probe"),
]

FAMILY_DISPOSITION = {
    "ledger_terminal_authority": "delete",
    "ledger_consumers": "port",
    "sql_sqlc": "port (split hosts first)",
    "session_old_store": "delete",
    "retained_infrastructure": "retain",
    "stall_jobs": "delete (stall clock/watchdog policy retained separately)",
    "old_model_tools": "delete",
    "prompts_tool_inventories": "port",
    "app_lifecycle": "port",
    "cli_sessions": "port",
    "server_rerun": "port",
    "web_completions": "port",
    "sdk_overrides": "retain",
    "switch": "delete",
    "migration_and_testdata": "retain (historical migrations / DDL fixtures allowlisted)",
    "historical_docs": "historical",
    "filelock_probe": "retain",
    "unknown": "review (disposition must be assigned by a human)",
}


def classify(path):
    fams = []
    for rx, fam in RULES:
        if re.search(rx, path):
            fams.append(fam)
    if not fams:
        fams = ["unknown"]
    # deterministic, rule order
    return fams


def git_grep(dirspec):
    out = subprocess.run(
        ["git", "grep", "-n", "-I", "-E", PATTERN, "--", dirspec],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if out.returncode not in (0, 1):  # 1 = no matches, which is fine
        out.check_returncode()
    return out.stdout.splitlines()


def tracked_files():
    out = subprocess.run(
        ["git", "ls-files"], cwd=REPO_ROOT, capture_output=True, text=True, check=True
    )
    return set(out.stdout.splitlines())


def main():
    os.chdir(REPO_ROOT)
    hits = collections.defaultdict(list)  # path -> [(lineno, text)]
    total = 0
    for d in TOP_DIRS:
        for line in git_grep(d):
            path, lineno, text = line.split(":", 2)
            hits[path].append((lineno, text))
            total += 1

    tracked = tracked_files()
    missing = sorted(set(hits) - tracked)

    files = []
    for path in sorted(hits):
        fams = classify(path)
        try:
            with open(os.path.join(REPO_ROOT, path), "rb") as f:
                nlines = sum(1 for _ in f)
        except OSError:
            nlines = -1
        flag = ""
        if nlines >= 900:
            flag = "NEAR-LIMIT" if nlines <= 1000 else "OVER-LIMIT"
        files.append(
            {
                "path": path,
                "lines": nlines,
                "hits": len(hits[path]),
                "fams": fams,
                "flag": flag,
            }
        )

    # summary per family
    fam_files = collections.defaultdict(int)
    fam_hits = collections.defaultdict(int)
    for fi in files:
        for fam in fi["fams"]:
            fam_files[fam] += 1
            fam_hits[fam] += fi["hits"]

    disp_counts = collections.Counter()
    for fi in files:
        ds = tuple(FAMILY_DISPOSITION[f] for f in fi["fams"])
        disp_counts["; ".join(ds)] += 1

    lines = []
    lines.append(f"# R12 Old-Path Inventory Manifest ({FIXED_DATE})")
    lines.append("")
    lines.append("## Provenance")
    lines.append("")
    lines.append("- Generator script: `tools/r12a_old_path_manifest.py` (stdlib only, deterministic, no network, not run in CI).")
    lines.append(f"- Generated date (fixed literal): {FIXED_DATE}.")
    lines.append("- Exact command equivalent (run once per top-level dir and concatenated, NO cap):")
    lines.append(f"  ```sh\n  {GREP_CMD}\n  ```")
    lines.append(f"- Total hits: {total} across {len(files)} files (sum of per-directory runs).")
    lines.append("- **Do not edit by hand; regenerate with the script.**")
    lines.append("")
    lines.append("## Summary by family")
    lines.append("")
    lines.append("| Family | Files | Hits | Disposition |")
    lines.append("|---|---:|---:|---|")
    for fam in sorted(fam_files):
        lines.append(
            f"| {fam} | {fam_files[fam]} | {fam_hits[fam]} | {FAMILY_DISPOSITION[fam]} |"
        )
    lines.append("")
    lines.append(f"Total dispositions across files: {sum(disp_counts.values())}")
    lines.append("")
    lines.append("| Disposition(s) | Files |")
    lines.append("|---|---:|")
    for ds in sorted(disp_counts):
        lines.append(f"| {ds} | {disp_counts[ds]} |")
    lines.append("")
    lines.append("## Files")
    lines.append("")
    lines.append("| Path | Lines | Hits | Families | Disposition | Flag |")
    lines.append("|---|---:|---:|---|---|---|")
    for fi in files:
        fams = ", ".join(fi["fams"])
        disp = "; ".join(FAMILY_DISPOSITION[f] for f in fi["fams"])
        lines.append(
            f"| `{fi['path']}` | {fi['lines']} | {fi['hits']} | {fams} | {disp} | {fi['flag']} |"
        )
    lines.append("")
    lines.append("## Full uncapped hit listing (grouped by file, with line numbers)")
    lines.append("")
    for fi in files:
        p = fi["path"]
        lines.append(f"### `{p}` ({fi['hits']} hits)")
        lines.append("")
        lines.append("```")
        for lineno, text in hits[p]:
            lines.append(f"{lineno}:{text}")
        lines.append("```")
        lines.append("")
    unknowns = [fi["path"] for fi in files if fi["fams"] == ["unknown"]]
    if unknowns:
        lines.append("## Files requiring human disposition (`unknown`)")
        lines.append("")
        for p in unknowns:
            lines.append(f"- `{p}`")
        lines.append("")
    if missing:
        lines.append("## WARNING: hit paths not in `git ls-files`")
        lines.append("")
        for p in missing:
            lines.append(f"- `{p}`")
        lines.append("")

    os.makedirs(os.path.dirname(OUT_PATH), exist_ok=True)
    with open(OUT_PATH, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines) + "\n")
    print(
        f"wrote {OUT_PATH}: {total} hits, {len(files)} files, "
        f"{len(unknowns)} unknown, {len(missing)} untracked-path warnings"
    )


if __name__ == "__main__":
    sys.exit(main())
