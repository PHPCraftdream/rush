"""Plan computation and printing for pkgmove."""

try:
    from . import mapping as mapping_mod
    from . import rewrite
    from . import verify as verify_mod
except ImportError:
    import mapping as mapping_mod
    import rewrite
    import verify as verify_mod

NONGO_PREFIXES = (".github/", ".githooks/", "tools/", "npm/", "scripts/")
NONGO_EXACT = (".golangci.yml", "sqlc.yaml")
CAP = 40


def _is_nongo_target(path, also_targets):
    if path.endswith(".go"):
        return False
    if path in also_targets:
        return True
    return path.startswith(NONGO_PREFIXES) or path in NONGO_EXACT


def _read(repo, path):
    try:
        return (repo.root / path).read_text(encoding="utf-8")
    except (UnicodeDecodeError, OSError):
        return None


def build_plan(repo, mapping_path_dict, also_targets, module):
    moves, already, errors = mapping_mod.validate_mapping(mapping_path_dict, repo)
    plan = {
        "repo": str(repo.root),
        "module": module,
        "errors": errors,
        "moves": [],
        "already": sorted(already),
    }
    if errors:
        return plan
    for old, new in mapping_path_dict.items():
        status = moves.status.get((old, new), "move")
        plan["moves"].append((old, new, status))
    plan["moves"].sort()

    tracked = repo.tracked_files()
    go_files = [f for f in tracked if f.endswith(".go")]

    affected_files = 0
    import_lines = 0
    unaliased = {}
    for f in go_files:
        text = _read(repo, f)
        if text is None:
            continue
        specs = rewrite.scan_import_specs(text)
        n = 0
        for alias, p in specs:
            new = rewrite.resolve_import(p, module, moves)
            if new is not None:
                n += 1
                if alias is None:
                    # key by the module-relative package dir of the import
                    tail = p[len(module) + 1:] if p.startswith(module + "/") else (
                        "." if p == module else p)
                    d = moves.resolve(tail) or tail
                    unaliased[d] = unaliased.get(d, 0) + 1
        if n:
            affected_files += 1
            import_lines += n
    plan["go_affected_files"] = affected_files
    plan["import_lines"] = import_lines
    plan["go_total"] = len(go_files)
    plan["unaliased"] = unaliased

    nongo = []
    for f in tracked:
        if not _is_nongo_target(f, also_targets):
            continue
        text = _read(repo, f)
        if text is None:
            continue
        spans = rewrite.scan_nongo(text, moves, module)
        if spans:
            nongo.append((f, len(spans)))
    plan["nongo"] = sorted(nongo)

    docs = []
    total_docs_refs = 0
    for f in tracked:
        if not f.startswith("docs/") or f.endswith(".go"):
            continue
        text = _read(repo, f)
        if text is None:
            continue
        spans = rewrite.scan_nongo(text, moves, module)
        if spans:
            docs.append((f, len(spans)))
            total_docs_refs += len(spans)
    docs.sort()
    plan["docs"] = docs
    plan["docs_refs"] = total_docs_refs

    warnings = []
    embed_files = 0
    for f in go_files:
        if moves.resolve(f.rsplit("/", 1)[0]) is None and moves.resolve(f) is None:
            continue
        text = _read(repo, f)
        if text and "go:embed" in text:
            embed_files += 1
    if embed_files:
        warnings.append(
            "%d Go files under moved directories use go:embed; embed paths are "
            "relative and move with their package (no rewrite needed)" % embed_files
        )
    tdata = set()
    for f in tracked:
        d = f.rsplit("/", 1)[0] if "/" in f else "."
        while "/" in d:
            if d.rsplit("/", 1)[-1] == "testdata" and (
                moves.resolve(d) is not None or moves.resolve(f) is not None
            ):
                tdata.add(d)
            d = d.rsplit("/", 1)[0]
        if d.rsplit("/", 1)[-1] == "testdata" and (
            moves.resolve(d) is not None or moves.resolve(f) is not None
        ):
            tdata.add(d)
    if tdata:
        warnings.append(
            "%d testdata directories move along with their packages" % len(tdata)
        )
    pkg_warnings = []
    pkg_dirs = sorted(
        {f.rsplit("/", 1)[0] for f in go_files
         if moves.resolve(f.rsplit("/", 1)[0]) is not None}
    )
    for d in pkg_dirs:
        target = moves.resolve(d)
        old_base = d.rsplit("/", 1)[-1]
        new_base = target.rsplit("/", 1)[-1]
        if old_base == new_base:
            continue
        base = target.rsplit("/", 1)[-1]
        decl = None
        non_test = sorted(
            f for f in go_files
            if f.rsplit("/", 1)[0] == d and not f.endswith("_test.go")
        )
        test_files = sorted(
            f for f in go_files
            if f.rsplit("/", 1)[0] == d and f.endswith("_test.go")
        )
        for rel in non_test + test_files:
            text = _read(repo, rel)
            if text:
                decl = rewrite.package_name(text)
                if decl:
                    break
        if decl and base != decl:
            n = plan["unaliased"].get(target, 0)
            pkg_warnings.append(
                "%s -> %s declares `package %s` (target basename %s); "
                "%d unaliased import(s) still resolve to the declared name, "
                "no rewrite needed" % (d, target, decl, base, n)
            )
    warnings.extend(pkg_warnings)
    plan["warnings"] = warnings
    return plan


def print_plan(plan, verbose=False):
    print("== pkgmove plan ==")
    print("repo: %s" % plan["repo"])
    print("module: %s" % plan["module"])
    line = "mapping entries: %d" % len(plan["moves"])
    if plan["already"]:
        line += " (already applied: %d)" % len(plan["already"])
    print(line)
    if plan["moves"]:
        print()
        print("Moves (%d):" % len(plan["moves"]))
        for old, new, _status in plan["moves"]:
            print("  %s -> %s" % (old, new))
    print()
    if plan["import_lines"]:
        print(
            "Go imports to rewrite: %d import lines in %d files (of %d tracked Go files)"
            % (plan["import_lines"], plan["go_affected_files"], plan["go_total"])
        )
    else:
        print("Go imports to rewrite: none")
    print()
    if plan["nongo"]:
        print("Non-Go references to rewrite (by file):")
        for path, count in plan["nongo"]:
            print("  %s: %d" % (path, count))
    else:
        print("Non-Go references to rewrite (by file):")
        print("  none")
    print()
    if plan["docs"]:
        print(
            "Docs references (warning: listed, not rewritten by default): "
            "%d references in %d files" % (plan["docs_refs"], len(plan["docs"]))
        )
        shown = plan["docs"] if verbose else plan["docs"][:CAP]
        for path, count in shown:
            print("  %s: %d" % (path, count))
        if len(plan["docs"]) > len(shown):
            print("... and %d more files (use --verbose)" % (len(plan["docs"]) - len(shown)))
    print()
    if plan["warnings"]:
        print("Warnings:")
        for w in plan["warnings"]:
            print("  - %s" % w)
