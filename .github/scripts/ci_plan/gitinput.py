"""Git input collection: merge-base, name-status diffs, base tree listing."""

from __future__ import annotations

import subprocess
from dataclasses import dataclass, field


class GitError(Exception):
    pass


def _git(root: str, *args: str) -> str:
    try:
        proc = subprocess.run(
            ["git", *args],
            cwd=root,
            capture_output=True,
            text=False,
            check=False,
        )
    except OSError as exc:
        raise GitError(f"failed to run git: {exc}") from exc
    if proc.returncode != 0:
        raise GitError(
            "git " + " ".join(args) + " failed: "
            + proc.stderr.decode("utf-8", "replace").strip()
        )
    return proc.stdout.decode("utf-8", "surrogateescape")


@dataclass
class Change:
    status: str  # A, M, D, R, C, T, U...
    path: str  # path at head (post path for R/C)
    old_path: str | None = None  # pre path for R/C


@dataclass
class RepoInput:
    base: str
    head: str
    merge_base: str
    changes: list[Change] = field(default_factory=list)
    base_tree_files: list[str] = field(default_factory=list)


def resolve_merge_base(root: str, base: str, head: str) -> str:
    out = _git(root, "merge-base", base, head).strip()
    if not out:
        raise GitError(f"no merge base between {base} and {head}")
    return out


def diff_name_status(root: str, frm: str, to: str) -> list[Change]:
    """NUL-delimited name-status diff including both rename/copy paths."""
    out = _git(root, "diff", "--name-status", "-z", f"{frm}..{to}")
    parts = out.split("\0")
    changes: list[Change] = []
    i = 0
    while i < len(parts):
        token = parts[i]
        if not token:
            i += 1
            continue
        status = token
        # Scored statuses like R100/C75: first char is the kind.
        kind = status[0]
        if kind in ("R", "C"):
            if i + 2 >= len(parts):
                raise GitError("truncated rename/copy entry in git diff output")
            changes.append(Change(kind, parts[i + 2], parts[i + 1]))
            i += 3
        else:
            if i + 1 >= len(parts):
                raise GitError("truncated entry in git diff output")
            changes.append(Change(kind, parts[i + 1]))
            i += 2
    return changes


def ls_tree_files(root: str, ref: str) -> list[str]:
    out = _git(root, "ls-tree", "-r", "-z", "--name-only", ref)
    return [p for p in out.split("\0") if p]


def collect(root: str, base: str, head: str) -> RepoInput:
    _ = _git(root, "rev-parse", "--verify", base + "^{commit}")
    _ = _git(root, "rev-parse", "--verify", head + "^{commit}")
    mb = resolve_merge_base(root, base, head)
    changes = diff_name_status(root, mb, head)
    if mb != base:
        # Union the merge-candidate side: base..merge_base changes are part of
        # what the PR merge candidate will contain beyond the raw head diff.
        changes.extend(diff_name_status(root, base, mb))
    base_tree = ls_tree_files(root, mb)
    return RepoInput(
        base=base,
        head=head,
        merge_base=mb,
        changes=changes,
        base_tree_files=base_tree,
    )
