"""Manifest building and verification for pkgmove."""

import json
import os

try:
    from . import mapping as mapping_mod
    from . import rewrite
except ImportError:
    import mapping as mapping_mod
    import rewrite


def _module_tail(path, module):
    if path == module:
        return "."
    if path.startswith(module + "/"):
        return path[len(module) + 1:]
    return None


def package_data(repo, dirpath, tracked_go_files, module):
    decls = []
    test_funcs = 0
    imports = set()
    test_imports = set()
    prefix = dirpath + "/"
    for f in tracked_go_files:
        if not (f == dirpath or f.startswith(prefix)):
            continue
        text = (repo.root / f).read_text(encoding="utf-8", errors="replace")
        if f.endswith("_test.go"):
            test_funcs += rewrite.count_test_functions(text)
            for _, p in rewrite.scan_import_specs(text):
                tail = _module_tail(p, module)
                if tail is not None:
                    test_imports.add(tail)
        else:
            decls.extend(rewrite.top_level_decls(text))
            for _, p in rewrite.scan_import_specs(text):
                tail = _module_tail(p, module)
                if tail is not None:
                    imports.add(tail)
    return {
        "declarations": sorted(decls),
        "test_functions": test_funcs,
        "imports": sorted(imports),
        "test_imports": sorted(test_imports),
    }


def _moved_dirs(repo, moves):
    go_files = repo.read_go_files()
    dirs = set()
    for f in go_files:
        d = f.rsplit("/", 1)[0] if "/" in f else "."
        if moves.resolve(d) is not None and d != ".":
            dirs.add(d)
    return go_files, sorted(dirs)


def build_manifest(repo, moves, module, date_iso):
    go_files, dirs = _moved_dirs(repo, moves)
    packages = {}
    for d in dirs:
        packages[d] = package_data(repo, d, go_files, module)
    return {
        "tool": "pkgmove",
        "manifest_version": 1,
        "module": module,
        "date": date_iso,
        "mapping": dict(moves.mapping),
        "packages": packages,
    }


def write_manifest(path, manifest):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(manifest, f, indent=2, sort_keys=True)
        f.write("\n")


def read_manifest(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def verify(repo, manifest, module):
    problems = []
    moves = mapping_mod.Moves(manifest["mapping"])
    go_files = repo.read_go_files()
    for old_dir in sorted(manifest.get("packages", {})):
        new_dir = moves.resolve(old_dir)
        if new_dir is None:
            new_dir = old_dir
        expected = manifest["packages"][old_dir]
        if not any(f.startswith(new_dir + "/") or f == new_dir for f in go_files):
            if not (repo.root / new_dir).is_dir():
                problems.append("package %s not found at %s" % (old_dir, new_dir))
                continue
        actual = package_data(repo, new_dir, go_files, module)
        if actual["declarations"] != expected["declarations"]:
            problems.append(
                "package %s (now %s): declarations differ" % (old_dir, new_dir)
            )
            exp = expected["declarations"]
            act = actual["declarations"]
            for line in sorted(set(exp) - set(act)):
                problems.append("  missing (in new): %s" % line)
            for line in sorted(set(act) - set(exp)):
                problems.append("  added (in new): %s" % line)
        if actual["test_functions"] != expected["test_functions"]:
            problems.append("test functions %s -> %s" % (old_dir, new_dir))
        for key in ("imports", "test_imports"):
            old_edges = sorted(
                (moves.resolve(e) or e) for e in expected.get(key, [])
            )
            new_edges = actual[key]
            missing = sorted(set(old_edges) - set(new_edges))
            extra = sorted(set(new_edges) - set(old_edges))
            if missing or extra:
                problems.append(
                    "package %s (now %s): %s differ" % (old_dir, new_dir, key)
                )
                for e in missing:
                    problems.append("  missing %s edge: %s" % (key, e))
                for e in extra:
                    problems.append("  extra %s edge: %s" % (key, e))
    # unexpected moved packages
    current_dirs = set()
    for f in go_files:
        d = f.rsplit("/", 1)[0] if "/" in f else "."
        if d != ".":
            current_dirs.add(d)
    for d in sorted(current_dirs):
        old = moves.reverse(d)
        if old is not None and old != d and old not in manifest.get("packages", {}):
            problems.append(
                "unexpected moved package %s (old %s) not in manifest" % (d, old)
            )
    return problems
