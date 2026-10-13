#!/usr/bin/env python3
"""Stdlib-only mutation proof runner; selftest never invokes Go."""
from __future__ import annotations
import argparse
import hashlib
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Optional, Sequence
# Constants
MUTATION_KEYS = (
    "id",
    "file",
    "file_sha256",
    "needle",
    "replacement",
    "package",
    "test",
    "purpose",
    "expected_assertion",
)
TEST_NAME_RE = re.compile(r"Test\w+")
PACKAGE_RE = re.compile(r"internal/[A-Za-z0-9_./-]+")
ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}")
SUBPROCESS_TIMEOUT_S = 900
EXCERPT_LEN = 4000
SCRATCH_PREFIX = "mutation-shard-"
LOGS_REL = Path("tools") / "mutation-logs"
# Data model
@dataclass(frozen=True)
class Mutation:
    id: str
    file: str
    file_sha256: str
    needle: str
    replacement: str
    package: str
    test: str
    purpose: str
    expected_assertion: str
@dataclass
class Outcome:
    """Result of running one mutation (or its baseline)."""
    baseline: str = "NOT_RUN"
    outcome: str = "NOT_RUN"
    duration_s: float = 0.0
    detail: str = ""
@dataclass
class RunEvent:
    """Decoded facts from a ``go test -json`` stream for one named test."""
    saw_run: bool = False
    final: Optional[str] = None
    invalid: bool = False
    output: str = ""
# Validation
def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()
def is_regular_file_inside(root: Path, rel: str) -> Optional[str]:
    """Return a rejection reason for ``rel``, or None if acceptable."""
    if ":" in rel or "\\" in rel or not rel.endswith(".go"):
        return "file must be a repo-relative non-test .go path"
    parts = rel.split("/")
    if any(p in ("", ".", "..") for p in parts):
        return "file path must be a clean repo-relative POSIX path"
    if any(p.endswith("_test.go") for p in parts):
        return "file must not be a test file (*_test.go)"
    if "testdata" in parts:
        return "file must not live under testdata/"
    candidate = root
    for p in parts:
        candidate = candidate / p
        if candidate.is_symlink():
            return f"file has a symlink component: {rel}"
    if not candidate.exists():
        return f"file does not exist under root: {rel}"
    if candidate.is_symlink():
        return f"file is a symlink, regular files only: {rel}"
    if not candidate.is_file():
        return f"file is not a regular file: {rel}"
    resolved_root = root.resolve()
    resolved = candidate.resolve()
    if resolved_root != resolved and resolved_root not in resolved.parents:
        return f"file resolves outside root: {rel}"
    return None
def tracked_files(root: Path) -> set[str]:
    proc = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        capture_output=True, timeout=30,
    )
    if proc.returncode:
        raise RuntimeError("git ls-files failed: " + proc.stderr.decode("utf-8", "replace"))
    return {name.decode("utf-8", "surrogateescape")
            for name in proc.stdout.split(b"\0") if name}
def validation_bytes(root: Path, rel: str, data: bytes, windows: bool) -> bytes:
    if not windows:
        return data
    proc = subprocess.run(
        ["git", "-C", str(root), "show", f"HEAD:{rel}"],
        capture_output=True, timeout=30,
    )
    if proc.returncode:
        raise RuntimeError(f"cannot read HEAD blob for {rel}")
    normalized = data.replace(b"\r\n", b"\n")
    if normalized != proc.stdout:
        raise RuntimeError(f"LF-normalized working bytes differ from HEAD blob for {rel}")
    return normalized
def module_path(root: Path) -> str:
    text = (root / "go.mod").read_text(encoding="utf-8")
    match = re.search(r'^module\s+("[^"\n]+"|[^\s]+)\s*(?://[^\n]*)?$', text, re.MULTILINE)
    if not match:
        raise ValueError("go.mod has no module directive")
    return match.group(1).strip('"')
def validate_manifest(
    data: object, root: Path, tracked: Optional[set[str]] = None,
    windows: Optional[bool] = None, invalid_records: Optional[set[int]] = None,
) -> tuple[list[Mutation], list[str]]:
    """Validate a decoded manifest against ``root``.
    Returns (mutations, problems). ``problems`` is a list of human-readable
    strings, each prefixed with ``[<id>]`` (or ``[manifest]``) and a reason.
    """
    problems: list[str] = []
    mutations: list[Mutation] = []
    record_index: Optional[int] = None
    def bad(where: str, reason: str) -> None:
        problems.append(f"[{where}] {reason}")
        if invalid_records is not None and record_index is not None:
            invalid_records.add(record_index)
    if not isinstance(data, dict):
        return [], ["[manifest] manifest must be a JSON object"]
    schema_version = data.get("schema_version")
    if not isinstance(schema_version, int) or isinstance(schema_version, bool):
        bad("manifest", "schema_version must be an integer")
    if not isinstance(data.get("task"), str):
        bad("manifest", "task must be a string")
    raw_mutations = data.get("mutations")
    if not isinstance(raw_mutations, list):
        bad("manifest", "mutations must be a list")
        return [], problems
    try:
        tracked = tracked_files(root) if tracked is None else tracked
    except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
        return [], [f"[manifest] cannot enumerate tracked files: {exc}"]
    windows = os.name == "nt" if windows is None else windows
    seen_ids: set[str] = set()
    for idx, raw in enumerate(raw_mutations):
        record_index = idx
        problems_before = len(problems)
        if not isinstance(raw, dict):
            bad(f"mutations[{idx}]", "mutation must be an object")
            continue
        keys = set(raw.keys())
        expected = set(MUTATION_KEYS)
        if keys != expected:
            missing = sorted(expected - keys)
            extra = sorted(keys - expected)
            detail = []
            if missing:
                detail.append(f"missing keys {missing}")
            if extra:
                detail.append(f"unexpected keys {extra}")
            bad(f"mutations[{idx}]", "; ".join(detail))
            continue
        mid = raw["id"]
        where = mid if isinstance(mid, str) and mid else f"mutations[{idx}]"
        values: dict[str, str] = {}
        ok = True
        for key in MUTATION_KEYS:
            value = raw[key]
            if not isinstance(value, str):
                bad(where, f"{key} must be a string")
                ok = False
            else:
                values[key] = value
        if not ok:
            continue
        if not ID_RE.fullmatch(values["id"]) or values["id"].upper() in {
            "CON", "PRN", "AUX", "NUL", *(f"COM{i}" for i in range(1, 10)),
            *(f"LPT{i}" for i in range(1, 10)),
        }:
            bad(where, "id must be a safe log filename identifier")
        if not values["expected_assertion"].strip():
            bad(where, "expected_assertion must be non-empty")
        if values["id"] in seen_ids:
            bad(where, f"duplicate id {values['id']!r}")
        seen_ids.add(values["id"])
        if not values["needle"]:
            bad(where, "needle must be non-empty")
        if values["replacement"] == values["needle"]:
            bad(where, "replacement must differ from needle")
        if not TEST_NAME_RE.fullmatch(values["test"]):
            bad(where, f"test name {values['test']!r} must match ^Test\\w+$")
        pkg = values["package"]
        if not pkg.startswith("internal/"):
            bad(where, f"package {pkg!r} must start with internal/")
        elif ("..." in pkg or not PACKAGE_RE.fullmatch(pkg)
              or any(p in ("", ".", "..") for p in pkg.split("/"))):
            bad(where, f"package {pkg!r} must be a concrete package path")
        rel = values["file"]
        if rel.startswith("/") or "\\" in rel:
            bad(where, f"file {rel!r} must be a repo-relative POSIX path")
            continue
        reason = is_regular_file_inside(root, rel)
        if reason is not None:
            bad(where, reason)
            continue
        if rel not in tracked:
            bad(where, f"file must be tracked: {rel}")
            continue
        try:
            content = validation_bytes(root, rel, (root / rel).read_bytes(), windows)
        except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
            bad(where, f"file integrity read failed: {exc}")
            continue
        count = content.count(values["needle"].encode("utf-8"))
        if count != 1:
            bad(where, f"needle must occur exactly once ({count} occurrences)")
        actual = sha256_bytes(content)
        if actual != values["file_sha256"]:
            bad(
                where,
                f"file_sha256 mismatch for {rel}: "
                f"manifest={values['file_sha256']} actual={actual}",
            )
            continue
        if len(problems) != problems_before:
            continue
        mutations.append(Mutation(**values))
    return mutations, problems
def load_manifest(path: Path, root: Path, invalid_records: Optional[set[int]] = None) -> tuple[list[Mutation], list[str]]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        return [], [f"[manifest] cannot read manifest: {exc}"]
    except (json.JSONDecodeError, UnicodeError) as exc:
        return [], [f"[manifest] manifest is not valid JSON: {exc}"]
    try:
        return validate_manifest(data, root, invalid_records=invalid_records)
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as exc:
        return [], [f"[manifest] validation failed: {exc}"]
# Sharding
def assign_shard(mutations: Sequence[Mutation], i: int, n: int) -> list[Mutation]:
    """Deterministic shard assignment.
    Mutations are grouped by package (preserving first-appearance order in the
    manifest) and whole packages are dealt round-robin across the ``n``
    shards, so every mutation lands in exactly one shard and package members
    stay together.
    """
    order: list[str] = []
    groups: dict[str, list[Mutation]] = {}
    for m in mutations:
        if m.package not in groups:
            groups[m.package] = []
            order.append(m.package)
        groups[m.package].append(m)
    if len(order) < n:
        return [m for idx, m in enumerate(mutations) if idx % n == i - 1]
    shards: list[list[Mutation]] = [[] for _ in range(n)]
    for idx, pkg in enumerate(order):
        shards[idx % n].extend(groups[pkg])
    return shards[i - 1]
# go test -json stream handling
def parse_go_test_events(raw: bytes) -> list[dict]:
    events: list[dict] = []
    text = raw.decode("utf-8", "strict") if isinstance(raw, bytes) else raw
    for line in text.splitlines():
        if not line.strip():
            continue
        obj = json.loads(line)
        if not isinstance(obj, dict) or not isinstance(obj.get("Action"), str):
            raise ValueError("malformed go test JSON event")
        events.append(obj)
    return events
def extract_named_test(events: Iterable[dict], pkg: str, test: str) -> RunEvent:
    result = RunEvent()
    chunks: list[str] = []
    for ev in events:
        name = ev.get("Test")
        if ev.get("Package") != pkg or not isinstance(name, str):
            continue
        action = ev.get("Action")
        if name != test:
            if (name.startswith(test + "/") and action == "output"
                    and result.saw_run and result.final is None):
                out = ev.get("Output")
                if isinstance(out, str):
                    chunks.append(out)
                else:
                    result.invalid = True
            continue
        if action == "run":
            if result.saw_run or result.final is not None:
                result.invalid = True
            result.saw_run = True
        elif action == "output":
            out = ev.get("Output")
            if not isinstance(out, str):
                result.invalid = True
            elif result.saw_run and result.final is None:
                chunks.append(out)
        elif action in ("pass", "fail", "skip"):
            if not result.saw_run or result.final is not None:
                result.invalid = True
            result.final = action
    result.output = "".join(chunks)
    return result
def named_event(raw: bytes, pkg: str, test: str) -> RunEvent:
    try:
        return extract_named_test(parse_go_test_events(raw), pkg, test)
    except (ValueError, UnicodeError):
        return RunEvent(invalid=True)
def classify_result(rc: Optional[int], event: RunEvent) -> str:
    """Classify one mutated run.
    KILLED           nonzero exit AND the named test ran and failed.
    SURVIVED         zero exit AND the named test ran and passed.
    EXECUTION_ERROR  everything else: build/vet failure, timeout, panic before
                     the test ran, missing events, ... (never a kill).
    """
    if rc is None or event.invalid:
        return "EXECUTION_ERROR"
    if rc != 0 and event.saw_run and event.final == "fail":
        return "KILLED"
    if rc == 0 and event.saw_run and event.final == "pass":
        return "SURVIVED"
    return "EXECUTION_ERROR"
def baseline_ok(rc: Optional[int], event: RunEvent) -> bool:
    """A baseline pass is: exit 0, run event then pass event, named test."""
    return rc == 0 and not event.invalid and event.saw_run and event.final == "pass"
# Subprocess plumbing
def build_test_cmd(package: str, test: str) -> list[str]:
    return [
        "go",
        "test",
        "-race",
        "-json",
        "-count=1",
        "-p=1",
        "-parallel=1",
        "-timeout=5m",
        "-run",
        f"^{test}$",
        "./" + package,
    ]
def child_env() -> dict[str, str]:
    env = dict(os.environ)
    env["CGO_ENABLED"] = "1"
    return env
def kill_process_tree(proc: subprocess.Popen) -> None:
    """Best-effort hard kill of a child; process-group aware on POSIX."""
    if proc.poll() is not None and os.name != "posix":
        return
    if os.name == "posix":
        try:
            os.killpg(proc.pid, signal.SIGKILL)
            return
        except (ProcessLookupError, PermissionError, OSError):
            pass
    try:
        proc.kill()
    except OSError:
        pass
def run_command(
    cmd: Sequence[str], cwd: Path, timeout_s: float
) -> tuple[Optional[int], bytes]:
    """Run a command, capturing combined stdout+stderr.
    Returns (returncode, output). ``returncode`` is None on timeout (after a
    process-group kill) — the caller must treat that as EXECUTION_ERROR.
    """
    proc = subprocess.Popen(  # noqa: S603 — fixed argv, no shell
        list(cmd),
        cwd=str(cwd),
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=child_env(),
        start_new_session=(os.name == "posix"),
    )
    try:
        out, _ = proc.communicate(timeout=timeout_s)
    except subprocess.TimeoutExpired:
        kill_process_tree(proc)
        try:
            out, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            out = b""
        return None, out or b""
    except BaseException:
        kill_process_tree(proc)
        try:
            proc.communicate(timeout=10)
        except (OSError, subprocess.SubprocessError):
            pass
        raise
    return proc.returncode, out or b""
def git_out(root: Path, *args: str) -> Optional[str]:
    try:
        proc = subprocess.run(  # noqa: S603
            ["git", "-C", str(root), *args],
            capture_output=True,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if proc.returncode != 0:
        return None
    return proc.stdout.decode("utf-8", "replace").strip()
# Scratch tree
def temp_root_dir() -> Path:
    return Path(os.environ.get("RUNNER_TEMP") or tempfile.gettempdir())
def scratch_dir(root: Path, manifest_stem: str, shard_i: int) -> Path:
    return temp_root_dir() / f"{SCRATCH_PREFIX}{manifest_stem}-{shard_i}"
def copy_checkout(root: Path, dest: Path) -> None:
    """Copy every git-tracked file from ``root`` into ``dest``."""
    listing = subprocess.run(  # noqa: S603 — read-only git query
        ["git", "-C", str(root), "ls-files", "-z"],
        capture_output=True,
        timeout=120,
    )
    if listing.returncode != 0:
        raise RuntimeError(
            "git ls-files failed: " + listing.stderr.decode("utf-8", "replace")
        )
    if dest.exists():
        import shutil
        shutil.rmtree(dest)
    dest.mkdir(parents=True)
    count = 0
    for raw_name in listing.stdout.split(b"\0"):
        if not raw_name:
            continue
        rel = raw_name.decode("utf-8", "surrogateescape")
        src = root / rel
        if src.is_symlink() or not src.is_file():
            continue  # submodules, symlinks, oddities: skip
        target = dest / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        data = src.read_bytes()
        target.write_bytes(data)
        src_mode = src.stat().st_mode & 0o777
        os.chmod(target, src_mode)
        count += 1
    print(f"[shard] copied {count} tracked files into {dest}", flush=True)
# Runner
class ShardRunner:
    def __init__(
        self,
        root: Path,
        mutations: Sequence[Mutation],
        manifest_path: Path,
        shard_i: int,
        shard_n: int,
    ) -> None:
        self.root = root
        self.all_mutations = list(mutations)
        self.manifest_path = manifest_path
        self.shard_i = shard_i
        self.shard_n = shard_n
        self.manifest_stem = manifest_path.stem
        self.scratch = scratch_dir(root, self.manifest_stem, shard_i)
        self.logs_dir = root / LOGS_REL
        self.mutations = assign_shard(mutations, shard_i, shard_n)
        self.baselines: dict[tuple[str, str], Outcome] = {}
        self.results: dict[str, Outcome] = {m.id: Outcome() for m in self.mutations}
        self.module = ""
        self.fatal: Optional[str] = None
    def _write_log(self, name: str, data: bytes) -> None:
        try:
            self.logs_dir.mkdir(parents=True, exist_ok=True)
            (self.logs_dir / name).write_bytes(data)
        except OSError as exc:
            print(f"[warn] cannot write log {name}: {exc}", file=sys.stderr)
    @staticmethod
    def _excerpt(data: str, limit: int = EXCERPT_LEN) -> str:
        text = data.strip()
        if len(text) <= limit:
            return text
        return "..." + text[-limit:]
    def baseline_for(self, m: Mutation) -> Outcome:
        key = (m.package, m.test)
        cached = self.baselines.get(key)
        if cached is not None:
            return cached
        cmd = build_test_cmd(m.package, m.test)
        print(f"[baseline] {m.package} :: {m.test}", flush=True)
        started = time.monotonic()
        try:
            rc, out = run_command(cmd, self.scratch, SUBPROCESS_TIMEOUT_S)
        except (OSError, subprocess.SubprocessError) as exc:
            out = str(exc).encode("utf-8")
            outcome = Outcome("ERROR", "BASELINE_ERROR", time.monotonic() - started, str(exc))
            self._write_log(f"{m.id}.baseline.log", out)
            self.baselines[key] = outcome
            return outcome
        duration = time.monotonic() - started
        event = named_event(out, self.module + "/" + m.package, m.test)
        if baseline_ok(rc, event):
            outcome = Outcome("PASS", "NOT_RUN", duration, "")
        else:
            why = "timeout" if rc is None else f"exit={rc} final={event.final}"
            outcome = Outcome(
                "FAIL", "NOT_RUN", duration, f"baseline failed: {why}"
            )
            print(
                f"[baseline] FAILED ({why}) — mutations for "
                f"{m.package} :: {m.test} will be BASELINE_ERROR",
                flush=True,
            )
        self._write_log(f"{m.id}.baseline.log", out)
        self.baselines[key] = outcome
        return outcome
    def _read_and_check(self, m: Mutation) -> Optional[bytes]:
        target = self.scratch / m.file
        try:
            data = target.read_bytes()
        except OSError as exc:
            print(f"[integrity] cannot read {m.file}: {exc}", file=sys.stderr)
            return None
        if sha256_bytes(data) != m.file_sha256:
            print(
                f"[integrity] scratch file sha mismatch for {m.file} "
                f"(before mutation) — aborting shard",
                file=sys.stderr,
            )
            return None
        return data
    def _restore(self, m: Mutation, original: bytes) -> bool:
        target = self.scratch / m.file
        try:
            target.write_bytes(original)
            data = target.read_bytes()
        except OSError as exc:
            print(f"[restore] cannot rewrite/verify {m.file}: {exc}", file=sys.stderr)
            return False
        if data != original or sha256_bytes(data) != m.file_sha256:
            print(
                f"[restore] sha mismatch for {m.file} after restore "
                f"— aborting shard",
                file=sys.stderr,
            )
            return False
        return True
    def run(self) -> int:
        print(f"[shard] {self.shard_i}/{self.shard_n} — {len(self.mutations)} mutation(s)")
        try:
            self.module = module_path(self.root)
            copy_checkout(self.root, self.scratch)
            self._run_mutations()
        except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as exc:
            self.fatal = f"setup/execution failed: {exc}"
            for result in self.results.values():
                if result.outcome == "NOT_RUN":
                    result.outcome = "EXECUTION_ERROR"
                    result.detail = self.fatal
        except BaseException as exc:
            self.fatal = self.fatal or f"interrupted: {type(exc).__name__}"
            for result in self.results.values():
                if result.outcome == "NOT_RUN":
                    result.outcome = "EXECUTION_ERROR"
                    result.detail = self.fatal
        if self.fatal:
            print(f"[shard] FATAL: {self.fatal}", file=sys.stderr)
        return 0 if (not self.fatal and self._all_killed()) else 1
    def _run_mutations(self) -> None:
        integrity_broken = False
        for m in self.mutations:
            if self.fatal or integrity_broken:
                self.results[m.id] = Outcome(
                    detail="skipped: shard integrity failure"
                )
                continue
            outcome = Outcome()
            baseline = self.baseline_for(m)
            outcome.baseline = baseline.baseline
            if baseline.baseline != "PASS":
                outcome.outcome = "BASELINE_ERROR"
                outcome.detail = baseline.detail
                self.results[m.id] = outcome
                self._report(m, outcome)
                continue
            original = self._read_and_check(m)
            if original is None:
                integrity_broken = True
                self.fatal = "integrity failure before mutation"
                self.results[m.id] = Outcome(
                    outcome="EXECUTION_ERROR", detail="integrity failure"
                )
                self._report(m, self.results[m.id])
                continue
            count = original.count(m.needle.encode("utf-8"))
            if not m.needle or count != 1:
                outcome.outcome = "EXECUTION_ERROR"
                outcome.detail = f"needle not found exactly once ({count} occurrences)"
                integrity_broken = True
                self.fatal = outcome.detail
                self.results[m.id] = outcome
                self._report(m, outcome)
                continue
            mutated = original.replace(m.needle.encode("utf-8"), m.replacement.encode("utf-8"), 1)
            target = self.scratch / m.file
            self.results[m.id] = outcome
            try:
                # Recheck the actual bytes immediately before touching the target.
                current = target.read_bytes()
                if current != original or current.count(m.needle.encode("utf-8")) != 1:
                    raise RuntimeError("integrity/needle changed immediately before write")
                target.write_bytes(mutated)
                if target.read_bytes() != mutated:
                    raise RuntimeError("mutation write verification failed")
                self._run_mutated(m, outcome)
            except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
                outcome.outcome = "EXECUTION_ERROR"
                outcome.detail = str(exc)
                self._write_log(f"{m.id}.mutated.log", str(exc).encode("utf-8"))
                if isinstance(exc, RuntimeError):
                    self.fatal = str(exc)
            except BaseException:
                outcome.outcome = "EXECUTION_ERROR"
                outcome.detail = "interrupted"
                raise
            finally:
                if not self._restore(m, original):
                    integrity_broken = True
                    self.fatal = f"restore failure for {m.file}"
                    outcome.outcome = "EXECUTION_ERROR"
                    outcome.detail += "\n" + self.fatal
            self.results[m.id] = outcome
            self._report(m, outcome)
    def _run_mutated(self, m: Mutation, outcome: Outcome) -> None:
        cmd = build_test_cmd(m.package, m.test)
        print(f"[mutate] {m.id}: {m.file} :: {m.test}", flush=True)
        started = time.monotonic()
        rc, out = run_command(cmd, self.scratch, SUBPROCESS_TIMEOUT_S)
        outcome.duration_s = time.monotonic() - started
        self._write_log(f"{m.id}.mutated.log", out)
        event = named_event(out, self.module + "/" + m.package, m.test)
        verdict = classify_result(rc, event)
        if verdict == "KILLED":
            assertion = m.expected_assertion
            if assertion and assertion not in event.output:
                outcome.outcome = "KILLED_UNEXPECTED"
                outcome.detail = (
                    f"killed but expected_assertion {assertion!r} not found "
                    "in failing output (NOT proved)"
                )
            else:
                outcome.outcome = "KILLED"
                outcome.detail = "killed" + (" (assertion present)" if m.expected_assertion else "")
        else:
            outcome.outcome = verdict
            if rc is None:
                outcome.detail = "timeout (process group killed)"
            else:
                outcome.detail = (
                    f"exit={rc} saw_run={event.saw_run} final={event.final}"
                )
        text = out.decode("utf-8", "replace")
        if not event.output:
            event.output = text
        outcome.detail += "\n" + self._excerpt(event.output)
    def _all_killed(self) -> bool:
        return all(
            self.results[m.id].outcome == "KILLED" for m in self.mutations
        )
    def _report(self, m: Mutation, outcome: Outcome) -> None:
        print(
            f"[result] {m.id}: {outcome.outcome} "
            f"({outcome.duration_s:.1f}s)",
            flush=True,
        )
    def summary(self) -> tuple[str, list[dict]]:
        head = git_out(self.root, "rev-parse", "HEAD") or "<unknown HEAD>"
        try:
            manifest_sha = sha256_bytes(self.manifest_path.read_bytes())
        except OSError:
            manifest_sha = "<unreadable>"
        header = [
            f"Repo HEAD: {head}",
            f"Manifest: {self.manifest_path} (sha256 {manifest_sha})",
            f"Shard: {self.shard_i}/{self.shard_n} — "
            f"expected {len(self.mutations)}, "
            f"completed {sum(1 for m in self.mutations if self.results[m.id].outcome != 'NOT_RUN')}",
            f"Fatal: {self.fatal or 'none'}",
            "",
            "ID | Package | Test | Baseline | Outcome | Duration",
            "--- | --- | --- | --- | --- | ---",
        ]
        rows = []
        for m in self.mutations:
            r = self.results[m.id]
            rows.append(
                f"{m.id} | {m.package} | {m.test} | {r.baseline} | "
                f"{r.outcome} | {r.duration_s:.1f}s"
            )
        return "\n".join(header + rows) + "\n", [
            {
                "id": m.id,
                "package": m.package,
                "test": m.test,
                "baseline": self.results[m.id].baseline,
                "outcome": self.results[m.id].outcome,
                "duration_s": round(self.results[m.id].duration_s, 3),
                "detail": self.results[m.id].detail,
            }
            for m in self.mutations
        ]
def write_results(records: list[dict]) -> None:
    path = Path.cwd() / "results.json"
    path.write_text(
        json.dumps(records, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    print(f"[results] wrote {path}", flush=True)
def append_step_summary(table: str) -> None:
    target = os.environ.get("GITHUB_STEP_SUMMARY")
    if not target:
        return
    try:
        with open(target, "a", encoding="utf-8") as fh:
            fh.write("## Mutation shard results\n\n```\n" + table + "\n```\n")
    except OSError as exc:
        print(f"[warn] cannot append to GITHUB_STEP_SUMMARY: {exc}", file=sys.stderr)
# --validate
def run_validate(manifest_path: Path, root: Path) -> int:
    invalid_records: set[int] = set()
    mutations, problems = load_manifest(manifest_path, root, invalid_records)
    needle_bad = sum(1 for p in problems if "needle" in p or "replacement must differ" in p)
    sha_bad = sum(1 for p in problems if "file_sha256" in p or "integrity" in p)
    print(f"mutations ok: {len(mutations)}")
    print(f"needle-not-exactly-once: {needle_bad}")
    print(f"sha-mismatch: {sha_bad}")
    print(f"invalid records: {len(invalid_records)}")
    print(f"diagnostics: {len(problems)}")
    if problems:
        print()
        for p in problems:
            print(f"  - {p}")
    return 0 if not problems else 1
# Self test
SELFTEST_GOOD_FILE = "internal/example/store.go"
SELFTEST_GOOD_CONTENT = b"package store\n\nfunc Get() int { return 1 }\n"
def _write_selftest_root(tmp: Path) -> tuple[dict, Path, Path]:
    target = tmp / SELFTEST_GOOD_FILE
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(SELFTEST_GOOD_CONTENT)
    sha = sha256_bytes(SELFTEST_GOOD_CONTENT)
    good = {
        "schema_version": 1,
        "task": "selftest",
        "mutations": [
            {
                "id": "M001",
                "file": SELFTEST_GOOD_FILE,
                "file_sha256": sha,
                "needle": "return 1",
                "replacement": "return 2",
                "package": "internal/example",
                "test": "TestGet",
                "purpose": "selftest",
                "expected_assertion": "want 1",
            }
        ],
    }
    return good, target, tmp
def _selftest_validator(tmp: Path) -> list[tuple[str, bool]]:
    cases: list[tuple[str, bool]] = []
    good, target, root = _write_selftest_root(tmp)
    muts, problems = validate_manifest(good, root, {SELFTEST_GOOD_FILE}, windows=False)
    cases.append(("good manifest accepted", not problems and len(muts) == 1))
    def variant(name: str, mutate) -> None:
        import copy
        data = copy.deepcopy(good)
        mutate(data)
        muts, problems = validate_manifest(data, root, {SELFTEST_GOOD_FILE}, windows=False)
        cases.append((name, bool(problems) and len(muts) == (1 if name == "duplicate id rejected" else 0)))
    def dup_id(d: dict) -> None:
        clone = dict(d["mutations"][0])
        clone["id"] = "M001"
        d["mutations"].append(clone)
    variant("duplicate id rejected", dup_id)
    variant("empty needle rejected", lambda d: d["mutations"][0].__setitem__("needle", ""))
    variant(
        "replacement == needle rejected",
        lambda d: d["mutations"][0].__setitem__("replacement", d["mutations"][0]["needle"]),
    )
    variant(
        "bad test name rejected",
        lambda d: d["mutations"][0].__setitem__("test", "testGet"),
    )
    variant(
        "non-Test test name rejected",
        lambda d: d["mutations"][0].__setitem__("test", "BenchmarkGet"),
    )
    variant(
        "path traversal rejected",
        lambda d: d["mutations"][0].__setitem__("file", "../escape.go"),
    )
    variant(
        "package outside internal/ rejected",
        lambda d: d["mutations"][0].__setitem__("package", "cmd/example"),
    )
    # symlink target handling: symlink to the real file must be rejected
    link = root / "internal/example/link.go"
    try:
        link.symlink_to(target)
        data = json.loads(json.dumps(good))
        data["mutations"][0]["file"] = "internal/example/link.go"
        _, problems = validate_manifest(data, root, {SELFTEST_GOOD_FILE}, windows=False)
        cases.append(("symlink file rejected", bool(problems)))
    except OSError:
        cases.append(("symlink file rejected (skipped, no symlink support)", True))
    for key, value in [
        ("test", "TestGet\n"), ("package", "internal/./example"),
        ("package", "internal/../example"), ("package", "internal/C:drive"),
        ("file", "C:store.go"), ("file", "internal/example/store.txt"),
        ("file", "internal/example/store_test.go"),
        ("id", "../escape"), ("id", "C:drive"), ("id", "NUL"),
        ("expected_assertion", ""), ("needle", "missing"),
        ("needle", "r"),
    ]:
        variant(f"unsafe {key}={value!r} rejected",
                lambda d, k=key, v=value: d["mutations"][0].__setitem__(k, v))
    _, problems = validate_manifest(good, root, set(), windows=False)
    cases.append(("untracked target rejected", bool(problems)))
    ancestor = root / "alias"
    try:
        ancestor.symlink_to(target.parent, target_is_directory=True)
        cases.append(("symlink ancestor rejected",
                      "symlink" in (is_regular_file_inside(root, "alias/store.go") or "")))
    except OSError:
        cases.append(("symlink ancestor (skipped, no support)", True))
    # sha mismatch
    target.write_bytes(SELFTEST_GOOD_CONTENT + b"\n")
    data = json.loads(json.dumps(good))
    _, problems = validate_manifest(data, root, {SELFTEST_GOOD_FILE}, windows=False)
    cases.append(("sha256 mismatch rejected", any("file_sha256" in p for p in problems)))
    return cases
def _selftest_classifier() -> list[tuple[str, bool]]:
    pkg, test = "github.com/PHPCraftdream/rush/internal/agent", "TestGet"
    events = lambda *acts: "".join(  # noqa: E731
        json.dumps(
            {"Action": a, "Package": pkg, "Test": test, "Elapsed": 0.1},
        )
        + "\n"
        for a in acts
    )
    def ev_with_output(action: str, output: str) -> str:
        return (
            json.dumps(
                {"Action": "output", "Package": pkg, "Test": test, "Output": output}
            )
            + "\n"
            + json.dumps({"Action": action, "Package": pkg, "Test": test})
            + "\n"
        )
    cases: list[tuple[str, bool]] = []
    kill_stream = (events("run") + ev_with_output("fail", "store_test.go:12: want 1\n")).encode()
    e = extract_named_test(parse_go_test_events(kill_stream), pkg, test)
    cases.append(("kill case -> KILLED", classify_result(1, e) == "KILLED"))
    child = [{"Action": a, "Package": pkg, "Test": test + "/is_error", "Output": "child assertion"}
             for a in ("run", "output", "fail")]
    e = extract_named_test([{"Action": "run", "Package": pkg, "Test": test}, *child,
                           {"Action": "fail", "Package": pkg, "Test": test}], pkg, test)
    cases.append(("child assertion extracted; parent classifies kill",
                  e.output == "child assertion" and classify_result(1, e) == "KILLED"))
    e = extract_named_test(child, pkg, test)
    cases.append(("child events alone cannot classify parent", classify_result(1, e) == "EXECUTION_ERROR"))
    survive_stream = events("run", "pass").encode()
    e = extract_named_test(parse_go_test_events(survive_stream), pkg, test)
    cases.append(("survive case -> SURVIVED", classify_result(0, e) == "SURVIVED"))
    build_fail = b"# internal/example\n./store.go:3:2: undefined: x\nFAIL\n"
    e = named_event(build_fail, pkg, test)
    cases.append(
        ("build failure -> EXECUTION_ERROR", classify_result(2, e) == "EXECUTION_ERROR")
    )
    e = extract_named_test(parse_go_test_events(events("run", "fail")), pkg, test)
    cases.append(("timeout (rc None) -> EXECUTION_ERROR", classify_result(None, e) == "EXECUTION_ERROR"))
    panic = b"panic: boom [recovered]\n\ngoroutine 1 [running]:\nFAIL\tinternal/example\t0.5s\n"
    e = named_event(panic, pkg, test)
    cases.append(
        ("panic before test ran -> EXECUTION_ERROR", classify_result(2, e) == "EXECUTION_ERROR")
    )
    e = extract_named_test(parse_go_test_events(events("run", "pass")), pkg, test)
    cases.append(("baseline pass detected", baseline_ok(0, e)))
    cases.append(("baseline nonzero not ok", not baseline_ok(1, e)))
    for actions in [("pass", "run"), ("fail", "run"), ("run", "skip"),
                    ("run", "fail", "pass")]:
        e = named_event(events(*actions).encode(), pkg, test)
        cases.append((f"invalid order {actions} rejected",
                      classify_result(1, e) == "EXECUTION_ERROR" and not baseline_ok(0, e)))
    for suffix in [b"{broken\n", b"[]\n", b"not JSON\n", b"\xff\n"]:
        e = named_event(events("run", "fail").encode() + suffix, pkg, test)
        cases.append((f"malformed stream {suffix!r} fails closed",
                      classify_result(1, e) == "EXECUTION_ERROR"))
    e = named_event(events("run", "pass").encode(), "internal/agent", test)
    cases.append(("short JSON package does not match module package", not baseline_ok(0, e)))
    cases.append(("Go command uses ./internal/agent",
                  build_test_cmd("internal/agent", test)[-1] == "./internal/agent"))
    good = Mutation("M1", "internal/agent/store.go", "", "x", "y",
                    "internal/agent", test, "proof", "want")
    from dataclasses import replace
    muts = [replace(good, id=f"M{i}") for i in range(5)]
    shards = [assign_shard(muts, i, 2) for i in (1, 2)]
    cases.append(("single package index round-robin",
                  [[m.id for m in s] for s in shards] == [["M0", "M2", "M4"], ["M1", "M3"]]))
    return cases
def run_selftest() -> int:
    suites: list[tuple[str, list[tuple[str, bool]]]] = [
        ("manifest validator", _selftest_validator(Path(tempfile.mkdtemp(prefix="mutation-selftest-")))),
        ("go test -json classifier", _selftest_classifier()),
    ]
    failures = 0
    for suite_name, cases in suites:
        print(f"== {suite_name} ==")
        for name, ok in cases:
            print(f"  {'PASS' if ok else 'FAIL'}: {name}")
            if not ok:
                failures += 1
    print(f"== selftest: {'PASS' if failures == 0 else 'FAIL'} ({failures} failure(s)) ==")
    return 0 if failures == 0 else 1
# CLI
def parse_shard(value: str) -> tuple[int, int]:
    m = re.match(r"^(\d+)/(\d+)$", value.strip())
    if not m:
        raise argparse.ArgumentTypeError("--shard must look like i/n, e.g. 1/2")
    i, n = int(m.group(1)), int(m.group(2))
    if not (1 <= i <= n and n >= 1):
        raise argparse.ArgumentTypeError("--shard requires 1 <= i <= n")
    return i, n
def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="run.py", description="Stdlib-only mutation testing runner."
    )
    parser.add_argument("--manifest", type=Path, help="path to the mutation manifest JSON")
    parser.add_argument(
        "--shard",
        type=parse_shard,
        metavar="i/n",
        help="1-based shard index and total shard count",
    )
    parser.add_argument(
        "--root",
        type=Path,
        default=Path.cwd(),
        help="repo root of the checkout under test (default: cwd)",
    )
    parser.add_argument(
        "--validate",
        action="store_true",
        help="validate the manifest only; no go commands are run",
    )
    parser.add_argument(
        "--selftest",
        action="store_true",
        help="run the pure-python self test and exit",
    )
    return parser
def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    root: Path = args.root.resolve()
    if args.selftest:
        return run_selftest()
    if not args.manifest:
        parser.error("--manifest is required for --validate and for runs")
    if args.validate:
        return run_validate(args.manifest, root)
    if not args.shard:
        parser.error("--shard i/n is required for runs")
    mutations, problems = load_manifest(args.manifest, root)
    shard_i, shard_n = args.shard
    runner = ShardRunner(root, mutations, args.manifest, shard_i, shard_n)
    if problems:
        runner.fatal = "manifest validation failed: " + "; ".join(problems)
        for result in runner.results.values():
            result.outcome = "EXECUTION_ERROR"
            result.detail = runner.fatal
        exit_code = 2
    else:
        exit_code = runner.run()
    table, records = runner.summary()
    if runner.fatal and not records:
        records = [{"outcome": "EXECUTION_ERROR", "detail": runner.fatal}]
    print()
    print(table)
    try:
        write_results(records)
    finally:
        append_step_summary(table)
    return exit_code
if __name__ == "__main__":
    sys.exit(main())
