#!/usr/bin/env python3
"""Tiered CI shadow-mode selector entry point.

Prints (and optionally writes) a plan of which packages would run per OS.
Shadow mode: gates nothing. Exit 0 whenever a plan is produced; nonzero only
on planning failure (missing SHAs, malformed metadata, failed enumeration).
"""

from __future__ import annotations

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from ci_plan import gitinput, golist, policy as pol, report, selector  # noqa: E402

TARGET_OSES = golist.TARGET_OSES


def _parse_args(argv: list[str] | None) -> argparse.Namespace:
    ap = argparse.ArgumentParser(prog="ci_plan.py", description=__doc__)
    ap.add_argument("--base", required=True, help="base commit SHA")
    ap.add_argument("--head", required=True, help="head commit SHA")
    ap.add_argument("--root", default=".", help="repository root (default: .)")
    ap.add_argument(
        "--policy", default=".github/ci-policy.toml", help="policy file path"
    )
    ap.add_argument("--out", default=None, help="write plan JSON here")
    ap.add_argument("--summary", default=None, help="write markdown summary here")
    ap.add_argument(
        "--go-list-json",
        action="append",
        default=[],
        metavar="OS=FILE",
        help=(
            "pre-extracted `go list -deps -test -json ./...` output, one per OS, "
            "as os=path (e.g. linux=/tmp/linux.json). Missing OSes are "
            "enumerated with one `go list` process each."
        ),
    )
    ap.add_argument(
        "--dry-run", action="store_true", help="print to stdout, write nothing"
    )
    return ap.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    try:
        polobj = pol.load_policy(args.policy)
    except pol.PolicyError as exc:
        print(f"ci_plan: {exc}", file=sys.stderr)
        return 2

    if not args.base or not args.head:
        print("ci_plan: --base and --head are required", file=sys.stderr)
        return 2

    try:
        repo = gitinput.collect(args.root, args.base, args.head)
    except gitinput.GitError as exc:
        print(f"ci_plan: planning failure: {exc}", file=sys.stderr)
        return 2

    go_files: dict[str, str | None] = {os_name: None for os_name in TARGET_OSES}
    for spec in args.go_list_json:
        if "=" not in spec:
            print(
                f"ci_plan: malformed --go-list-json {spec!r}, expected os=path",
                file=sys.stderr,
            )
            return 2
        os_name, _, path = spec.partition("=")
        if os_name not in TARGET_OSES:
            print(
                f"ci_plan: unknown OS {os_name!r} in --go-list-json "
                f"(expected one of {', '.join(TARGET_OSES)})",
                file=sys.stderr,
            )
            return 2
        go_files[os_name] = path
    try:
        graphs = golist.build_graphs(args.root, go_files)
    except golist.GraphError as exc:
        print(f"ci_plan: planning failure: {exc}", file=sys.stderr)
        return 2

    try:
        plan = selector.plan(args.root, repo, graphs, polobj)
    except selector.SelectionError as exc:
        print(f"ci_plan: planning failure: {exc}", file=sys.stderr)
        return 2

    js = report.plan_json(plan)
    md = report.summary_md(plan)
    if args.dry_run:
        print(js)
        print()
        print(md)
        return 0
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(js + "\n")
    else:
        print(js)
    if args.summary:
        with open(args.summary, "w", encoding="utf-8") as fh:
            fh.write(md)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
