"""pkgmove: plan, apply, manifest and verify Go package moves in a git repo.

usage: python -m pkgmove <command> --mapping MAP.json [--repo ROOT] [options]

commands:
  plan       (default) validate mapping against the repo, print a report
  apply      execute the plan: git mv, rewrite Go imports and non-Go refs,
             append rows to the moves table
  manifest   write a JSON manifest for the packages being moved
  verify     compare a manifest against the post-move repository

options:
  --mapping PATH    JSON {"old/dir": "new/dir"} of repo-relative dirs (required)
  --repo ROOT       repository root (default: current directory)
  --also PATH       also rewrite path refs in this repo-relative file (repeatable)
  --out PATH        manifest output path (default pkgmove_manifest.json)
  --manifest PATH   apply/verify manifest path (default: none on apply,
                    pkgmove_manifest.json on verify)
  --moves-doc PATH  moves table path (default docs/package-moves.md)
  --date YYYY-MM-DD date written to the moves table (default: today)
  --verbose         print full listings instead of capped lists

exit codes: 0 ok, 1 verify mismatch, 2 usage/validation error
"""

import argparse
import datetime
import os
import sys

try:
    from . import gitops, mapping, report, rewrite, verify
except ImportError:
    import gitops, mapping, report, rewrite, verify


def build_parser():
    p = argparse.ArgumentParser(
        prog="pkgmove", add_help=True,
        usage="python -m pkgmove <command> --mapping MAP.json [--repo ROOT] [options]",
    )
    p.add_argument("command", nargs="?", default="plan",
                   choices=["plan", "apply", "manifest", "verify"])
    p.add_argument("--mapping", required=False)
    p.add_argument("--repo", default=".")
    p.add_argument("--also", action="append", default=[])
    p.add_argument("--out", default="pkgmove_manifest.json")
    p.add_argument("--manifest", default=None)
    p.add_argument("--moves-doc", default="docs/package-moves.md", dest="moves_doc")
    p.add_argument("--date", default=None)
    p.add_argument("--verbose", action="store_true")
    return p


def _fail(msg):
    print("error: %s" % msg, file=sys.stderr)
    return 2


def _write_bytes(repo, path, data):
    full = repo.root / path
    parent = full.parent
    if str(parent):
        os.makedirs(parent, exist_ok=True)
    with open(full, "wb") as f:
        f.write(data)


def _read_bytes(repo, path):
    with open(repo.root / path, "rb") as f:
        return f.read()


def _append_moves_table(repo, moves_doc, moves, date_iso):
    path = repo.root / moves_doc
    rows = []
    existing_pairs = set()
    if path.exists():
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    else:
        lines = ["# Package moves", "", "| Old path | New path | Date |", "|---|---|---|"]
    # find header row index; if the file exists but lacks the table, create it
    if not any(l.startswith("| Old path") for l in lines):
        if lines and lines[0].startswith("# Package moves"):
            body = lines[1:]
        else:
            body = lines
        lines = ["# Package moves", "", "| Old path | New path | Date |", "|---|---|---|"] + body
    for l in lines:
        rows.append(l)
        if l.startswith("| ") and not l.endswith("|---") and "---" not in l.split("|")[1]:
            cells = [c.strip() for c in l.strip().strip("|").split("|")]
            if len(cells) >= 2:
                existing_pairs.add((cells[0], cells[1]))
    appended = 0
    for old, new in sorted(moves.mapping.items()):
        if (old, new) in existing_pairs:
            continue
        rows.append("| %s | %s | %s |" % (old, new, date_iso))
        appended += 1
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(rows) + "\n", encoding="utf-8", newline="\n")
    return appended


def _nongo_target(path, also_targets):
    if path.endswith(".go"):
        return False
    if path in also_targets:
        return True
    prefixes = (".github/", ".githooks/", "tools/", "npm/", "scripts/")
    return path.startswith(prefixes) or path in (".golangci.yml", "sqlc.yaml")


def cmd_plan(repo, mapping_dict, args):
    module = rewrite.read_module(repo)
    plan = report.build_plan(repo, mapping_dict, set(args.also), module)
    if plan["errors"]:
        for e in plan["errors"]:
            print("error: %s" % e, file=sys.stderr)
        return 2
    report.print_plan(plan, verbose=args.verbose)
    return 0


def cmd_apply(repo, mapping_dict, args):
    module = rewrite.read_module(repo)
    moves, already, errors = mapping.validate_mapping(mapping_dict, repo, apply_mode=True)
    if errors:
        for e in errors:
            print("error: %s" % e, file=sys.stderr)
        return 2
    date_iso = args.date or datetime.date.today().isoformat()
    if args.manifest:
        manifest = verify.build_manifest(repo, moves, module, date_iso)
        verify.write_manifest(args.manifest, manifest)
    snapshot = repo.tracked_files()

    moved = 0
    for old, new in moves.order:
        if moves.status.get((old, new)) != "move":
            continue
        if not moves.top_level.get((old, new), True):
            continue
        repo.git_mv(old, new)
        print("moved %s -> %s" % (old, new))
        moved += 1

    go_files = [f for f in snapshot if f.endswith(".go")]
    go_rewritten = 0
    import_lines = 0
    for f in go_files:
        cur = moves.resolve(f) or f
        data = _read_bytes(repo, cur)
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            print("warning: skipping non-UTF-8 file %s" % cur, file=sys.stderr)
            continue
        new_text, changed = rewrite.rewrite_go_source(text, module, moves)
        if changed:
            _write_bytes(repo, cur, new_text.encode("utf-8"))
            go_rewritten += 1
            import_lines += len(changed)

    nongo_files = [f for f in snapshot if _nongo_target(f, set(args.also))]
    nongo_rewritten = 0
    nongo_refs = 0
    for f in nongo_files:
        cur = moves.resolve(f) or f
        data = _read_bytes(repo, cur)
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        spans = rewrite.scan_nongo(text, moves, module)
        new_text, n = rewrite.rewrite_with_spans(text, [(s, e, n2) for s, e, _o, n2 in spans])
        if n:
            _write_bytes(repo, cur, new_text.encode("utf-8"))
            nongo_rewritten += 1
            nongo_refs += n

    rows = _append_moves_table(repo, args.moves_doc, moves, date_iso)

    print(
        "summary: %d directories moved, %d already moved, "
        "%d Go files rewritten (%d import lines), "
        "%d non-Go files rewritten (%d references), %d table rows appended"
        % (moved, len(already), go_rewritten, import_lines,
           nongo_rewritten, nongo_refs, rows)
    )
    return 0


def cmd_manifest(repo, mapping_dict, args):
    module = rewrite.read_module(repo)
    moves, already, errors = mapping.validate_mapping(mapping_dict, repo)
    if errors:
        for e in errors:
            print("error: %s" % e, file=sys.stderr)
        return 2
    date_iso = args.date or datetime.date.today().isoformat()
    manifest = verify.build_manifest(repo, moves, module, date_iso)
    verify.write_manifest(args.out, manifest)
    print("manifest written to %s (%d packages)" % (args.out, len(manifest["packages"])))
    return 0


def cmd_verify(repo, args):
    if args.manifest is None:
        args.manifest = "pkgmove_manifest.json"
    manifest = verify.read_manifest(args.manifest)
    module = manifest.get("module") or rewrite.read_module(repo)
    problems = verify.verify(repo, manifest, module)
    if problems:
        for i, p in enumerate(problems, 1):
            print("%d. %s" % (i, p))
        return 1
    print("verify OK: %d packages match the manifest" % len(manifest.get("packages", {})))
    return 0


def main(argv):
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.command != "verify" and args.mapping is None:
        return _fail("--mapping is required for plan/apply/manifest")
    try:
        mapping_dict = mapping.load_mapping(args.mapping)
    except (OSError, ValueError) as exc:
        return _fail("cannot load mapping %s: %s" % (args.mapping, exc))
    repo = gitops.Repo(args.repo)
    if args.command == "verify":
        return cmd_verify(repo, args)
    try:
        module = rewrite.read_module(repo)
    except (OSError, RuntimeError) as exc:
        return _fail(str(exc))
    if args.command == "plan":
        return cmd_plan(repo, mapping_dict, args)
    if args.command == "apply":
        return cmd_apply(repo, mapping_dict, args)
    if args.command == "manifest":
        return cmd_manifest(repo, mapping_dict, args)
    return _fail("unknown command")


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
