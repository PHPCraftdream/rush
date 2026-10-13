"""Policy loading and path matching helpers (stdlib only)."""

from __future__ import annotations

import fnmatch
import tomllib
from dataclasses import dataclass, field


class PolicyError(Exception):
    pass


@dataclass
class NonGoMapping:
    patterns: list[str]
    owners: list[str]


@dataclass
class Policy:
    native_required_prefixes: list[str] = field(default_factory=list)
    core_prefixes: list[str] = field(default_factory=list)
    race_roots: list[str] = field(default_factory=list)
    fanout_max_packages: int = 25
    fanout_max_ratio: float = 0.40
    non_go_mappings: list[NonGoMapping] = field(default_factory=list)
    always_run: list[str] = field(default_factory=list)
    native_suffixes: list[str] = field(default_factory=list)


def load_policy(path: str) -> Policy:
    try:
        with open(path, "rb") as fh:
            raw = tomllib.load(fh)
    except FileNotFoundError as exc:
        raise PolicyError(f"policy file not found: {path}") from exc
    except tomllib.TOMLDecodeError as exc:
        raise PolicyError(f"malformed policy {path}: {exc}") from exc

    mappings = []
    for m in raw.get("non_go_mappings", []):
        try:
            mappings.append(
                NonGoMapping(list(m["patterns"]), list(m["owners"]))
            )
        except KeyError as exc:
            raise PolicyError(f"non_go_mappings entry missing {exc}") from exc

    try:
        return Policy(
            native_required_prefixes=list(raw["native_required_prefixes"]),
            core_prefixes=list(raw["core_prefixes"]),
            race_roots=list(raw["race_roots"]),
            fanout_max_packages=int(raw.get("fanout_max_packages", 25)),
            fanout_max_ratio=float(raw.get("fanout_max_ratio", 0.40)),
            non_go_mappings=mappings,
            always_run=list(raw.get("always_run", [])),
            native_suffixes=list(
                raw.get(
                    "native_suffixes",
                    ["_windows", "_darwin", "_linux", "_unix", "_amd64", "_arm64"],
                )
            ),
        )
    except KeyError as exc:
        raise PolicyError(f"policy missing required key {exc}") from exc


def under_prefix(path: str, prefixes: list[str]) -> str | None:
    """Return the first prefix that matches path (repo-relative, /-separated).

    A prefix without a trailing slash matches the directory itself or any
    descendant; a trailing slash requires descendants.
    """
    norm = path.replace("\\", "/")
    for p in prefixes:
        p = p.replace("\\", "/")
        if p.endswith("/"):
            if norm.startswith(p):
                return p
        else:
            if norm == p or norm.startswith(p + "/"):
                return p
    return None


def glob_match(path: str, pattern: str) -> bool:
    """fnmatch-style match where `**` crosses directory levels."""
    norm = path.replace("\\", "/")
    pat = pattern.replace("\\", "/")
    if "**" not in pat:
        return fnmatch.fnmatchcase(norm, pat)
    head, _, tail = pat.partition("**")
    if tail.startswith("/"):
        tail = tail[1:]
        # '**/rest' matches 'rest' at any depth, including zero.
        for i, _ in enumerate(norm + "/"):
            if fnmatch.fnmatchcase(norm[i:], tail):
                return True
        return False
    # '**' at the end: prefix must match.
    return norm.startswith(head.rstrip("*")) if head.endswith("/") else norm.startswith(head)


def map_non_go(path: str, mappings: list[NonGoMapping]) -> list[str]:
    owners: list[str] = []
    for m in mappings:
        if any(glob_match(path, p) for p in m.patterns):
            for o in m.owners:
                if o not in owners:
                    owners.append(o)
    return owners
