"""Mapping load, normalize, resolve, and validation for pkgmove."""

import json

try:
    from . import gitops
except ImportError:
    import gitops


class MappingError(ValueError):
    pass


def normalize_dir(p):
    s = p.replace("\\", "/")
    while s.startswith("./"):
        s = s[2:]
    while s.endswith("/"):
        s = s[:-1]
    while "//" in s:
        s = s.replace("//", "/")
    if not s:
        raise MappingError("empty path")
    if s.startswith("/") or any(seg == ".." for seg in s.split("/")):
        raise MappingError("invalid path: %r" % p)
    return s


def load_mapping(path):
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, dict):
        raise MappingError("mapping must be a JSON object")
    out = {}
    for k, v in data.items():
        if not isinstance(k, str) or not isinstance(v, str):
            raise MappingError("mapping keys and values must be strings")
        out[normalize_dir(k)] = normalize_dir(v)
    return out


class Moves:
    """Sorted longest-old-prefix-first resolver."""

    def __init__(self, mapping):
        self.mapping = dict(mapping)
        self.entries = sorted(mapping.items(), key=lambda kv: len(kv[0]), reverse=True)

    def _apply(self, path, pairs):
        for old, new in pairs:
            if path == old:
                return new
            if path.startswith(old + "/"):
                return new + path[len(old):]
        return None

    def resolve(self, path):
        return self._apply(path, self.entries)

    def reverse(self, path):
        pairs = [(new, old) for old, new in self.entries]
        pairs.sort(key=lambda kv: len(kv[0]), reverse=True)
        return self._apply(path, pairs)


def _inside(a, b):
    return a == b or a.startswith(b + "/")


def _strictly_inside(a, b):
    return a.startswith(b + "/")


def validate_mapping(mapping, repo, apply_mode=False):
    """Return (moves, already, errors) for a repo (gitops.Repo).

    Ordering-aware: nested/overlapping entries are allowed when a consistent
    execution order exists. apply_mode=True treats old-missing+new-exists as
    "already"; it additionally fails on old-missing+new-missing.
    """
    errors = []
    already = []
    pairs = sorted(mapping.items())

    # exact duplicates of normalized target between distinct sources
    target_of = {}
    for old, new in mapping.items():
        if old == new:
            errors.append("identity mapping: %s -> %s" % (old, new))
        target_of.setdefault(new, []).append(old)
    for new, olds in sorted(target_of.items()):
        if len(olds) > 1:
            errors.append(
                "two sources map to one target %s: %s" % (new, ", ".join(sorted(olds)))
            )

    status = {}
    for old, new in pairs:
        if old == new:
            continue
        # target-exists check is skipped when a move vacates the target:
        # another entry's move, or the entry's own two-step self-nesting
        vacated = any(
            o != old and (_inside(new, o) or new == o) for o, _n in pairs
        ) or _strictly_inside(new, old)
        old_exists = repo.tracked_path_exists(old) or repo.dir_exists(old)
        new_exists = repo.tracked_path_exists(new) or repo.dir_exists(new)
        if _strictly_inside(new, old) and repo.tracked_path_exists(new):
            status[(old, new)] = "already"
            already.append((old, new))
        elif old_exists and new_exists and not vacated:
            errors.append("target exists: %s -> %s" % (old, new))
            status[(old, new)] = "error"
        elif not old_exists and not new_exists:
            errors.append("source not found: %s" % old)
            status[(old, new)] = "error"
        elif not old_exists and new_exists:
            status[(old, new)] = "already"
            already.append((old, new))
        else:
            status[(old, new)] = "move"

    moves = Moves(mapping)
    # ride-along: nested inside another source and the parent's move lands it exactly
    ride_along = set()
    two_step = set()
    top_level = {}
    for old_x, new_x in pairs:
        if status.get((old_x, new_x)) == "error":
            continue
        if _strictly_inside(new_x, old_x):
            two_step.add((old_x, new_x))
        parent_move = Moves({o: n for o, n in pairs if o != old_x})
        nested = any(o != old_x and _strictly_inside(old_x, o) for o, _n in pairs)
        if nested and parent_move.resolve(old_x) == new_x:
            ride_along.add((old_x, new_x))
        top_level[(old_x, new_x)] = (old_x, new_x) not in ride_along

    # ordering edges between non-error entries
    active = [(o, n) for o, n in pairs if status.get((o, n)) != "error"]
    preds = {e: set() for e in active}  # e -> entries that must run before e
    succs = {e: set() for e in active}
    for a in active:
        oa, na = a
        for b in active:
            if a == b:
                continue
            ob, nb = b
            # E1: A's target sits in B's old subtree -> B before A
            if _strictly_inside(na, ob) or na == ob:
                preds[a].add(b)
                succs[b].add(a)
            # E2: A escapes B's move -> A before B
            elif (
                _strictly_inside(oa, ob)
                and a not in ride_along
            ):
                preds[b].add(a)
                succs[a].add(b)

    # Kahn's algorithm, sorted ready nodes for determinism
    indeg = {e: len(preds[e]) for e in active}
    ready = sorted(e for e in active if indeg[e] == 0)
    order = []
    while ready:
        e = ready.pop(0)
        order.append(e)
        for s in sorted(succs[e]):
            indeg[s] -= 1
            if indeg[s] == 0:
                # keep ready sorted
                lo, hi = 0, len(ready)
                while lo < hi:
                    mid = (lo + hi) // 2
                    if ready[mid] < s:
                        lo = mid + 1
                    else:
                        hi = mid
                ready.insert(lo, s)
    if len(order) != len(active):
        cyclic = sorted(o for o, _n in active if (o, mapping[o]) not in set(order))
        errors.append(
            "overlapping inconsistent entries (ordering cycle): " + ", ".join(cyclic)
        )
        for o, n in active:
            if (o, n) not in set(order):
                status[(o, n)] = "error"
        order = [e for e in order if status.get(e) != "error"]

    # prune error entries from already list
    already = [(o, n) for (o, n) in already if status.get((o, n)) == "already"]

    moves.status = {k: v for k, v in status.items() if v != "error"}
    moves.top_level = top_level
    moves.order = [e for e in order if e not in ride_along]
    moves.two_step = two_step
    moves.ride_along = ride_along
    return moves, already, errors


def top_level(entry_moves, old):
    return entry_moves.top_level.get((old, entry_moves.mapping[old]), True)
