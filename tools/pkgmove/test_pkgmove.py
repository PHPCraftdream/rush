"""Tests for pkgmove."""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import gitops
import mapping
import rewrite

TOOLS_DIR = Path(__file__).resolve().parent.parent

GO_MOD = "module example.com/repo\n\ngo 1.21\n"

AGENT_GO = (
    "package agent\n\n"
    'import "example.com/repo/internal/util"\n\n'
    "func Hello() string { return util.Name() }\n"
)
AGENT_TEST_GO = (
    "package agent\n\n"
    "import (\n"
    '\t"testing"\n'
    '\t"example.com/repo/internal/util"\n'
    ")\n\n"
    "func TestHello(t *testing.T) { _ = util.Name() }\n"
)
CHILD_GO = "package child\n\nfunc C() int { return 1 }\n"
RESULT_GO = "package agentresult\n\nfunc R() int { return 2 }\n"
UTIL_GO = "package util\n\nfunc Name() string { return \"util\" }\n"
MISC_GO = "package miscx\n\nfunc M() int { return 3 }\n"
STABLE_GO = "package other\n\nfunc S() int { return 4 }\n"
APP_GO = (
    "package main\n\n"
    "import (\n"
    '\tag "example.com/repo/internal/agent"\n'
    '\t"example.com/repo/internal/util"\n'
    ")\n\n"
    "func main() { _ = ag.Hello(); _ = util.Name() }\n"
)
GOLANGCI = "run:\n  skip-dirs:\n    - internal/agent\n    - internal/agentresult\n"
DOCS_NOTE = "# note\n\nsee internal/agent and internal/agentresult\n"

FILES = {
    "go.mod": GO_MOD,
    "internal/agent/agent.go": AGENT_GO,
    "internal/agent/agent_test.go": AGENT_TEST_GO,
    "internal/agent/child/child.go": CHILD_GO,
    "internal/agentresult/result.go": RESULT_GO,
    "internal/util/util.go": UTIL_GO,
    "internal/misc/misc.go": MISC_GO,
    "internal/stable/stable.go": STABLE_GO,
    "cmd/app.go": APP_GO,
    ".golangci.yml": GOLANGCI,
    "docs/note.md": DOCS_NOTE,
}


def run_cli(args, cwd):
    return subprocess.run(
        [sys.executable, "-m", "pkgmove"] + args,
        cwd=str(cwd), capture_output=True, text=True,
    )


def snapshot(root):
    out = {}
    for p in Path(root).rglob("*"):
        if ".git" in p.parts:
            continue
        if p.is_file():
            out[str(p.relative_to(root)).replace("\\", "/")] = p.read_bytes()
    return out


class PkgmoveTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        for rel, content in FILES.items():
            f = self.root / rel
            f.parent.mkdir(parents=True, exist_ok=True)
            f.write_text(content, encoding="utf-8", newline="\n")
        for cmd in (
            ["git", "init", "-q", "-b", "main"],
            ["git", "config", "user.email", "t@example.com"],
            ["git", "config", "user.name", "t"],
            ["git", "config", "core.autocrlf", "false"],
            ["git", "add", "-A"],
            ["git", "commit", "-q", "-m", "init"],
        ):
            subprocess.run(cmd, cwd=str(self.root), check=True, capture_output=True)
        self.mapping_file = self.root / "map.json"

    def write_mapping(self, m):
        self.mapping_file.write_text(json.dumps(m), encoding="utf-8")

    def cli(self, args):
        return run_cli(args, TOOLS_DIR)

    def full(self, extra):
        return ["--mapping", str(self.mapping_file), "--repo", str(self.root)] + extra

    # 1
    def test_plan_report(self):
        self.write_mapping({"internal/agent": "internal/core/agent",
                            "internal/misc": "internal/core/misckit",
                            "internal/stable": "internal/core/stable"})
        before = snapshot(self.root)
        r = self.cli(self.full(["plan"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Moves (3):", r.stdout)
        self.assertIn("internal/agent -> internal/core/agent", r.stdout)
        self.assertIn("internal/misc -> internal/core/misckit", r.stdout)
        self.assertIn("import lines", r.stdout)
        self.assertIn("docs/note.md", r.stdout)
        self.assertIn("package miscx", r.stdout)
        self.assertNotIn("internal/stable", r.stdout.split("Warnings:")[-1])
        self.assertEqual(snapshot(self.root), before)

    # 2
    def test_conflicts_rejected(self):
        cases = [
            ({"internal/agent": "internal/util"}, False),
            ({"internal/agent": "x/y", "internal/util": "x/y"}, False),
            # ordering cycle: each target strictly inside the other's source
            ({"internal/agent": "internal/util/sub",
              "internal/util": "internal/agent/sub2"}, True),
            ({"internal/agent": "internal/util/sub",
              "internal/util": "internal/agentresult/sub",
              "internal/agentresult": "internal/agent/sub2"}, True),
        ]
        for m, cycle in cases:
            self.write_mapping(m)
            before = snapshot(self.root)
            for cmd in ("plan", "apply"):
                r = self.cli(self.full([cmd]))
                self.assertEqual(r.returncode, 2, (m, cmd, r.stdout, r.stderr))
                self.assertIn("error:", r.stderr)
                if cycle:
                    self.assertIn("ordering cycle", r.stderr)
            self.assertEqual(snapshot(self.root), before, m)

    # 2b
    def test_self_nested_rename(self):
        self.write_mapping({"internal/agent": "internal/agent/base"})
        r = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.root / "internal/agent/base/agent.go").exists())
        self.assertFalse((self.root / "internal/agent/agent.go").exists())
        self.assertFalse((self.root / "internal/agent/agent_test.go").exists())
        app = (self.root / "cmd/app.go").read_text(encoding="utf-8")
        self.assertIn('ag "example.com/repo/internal/agent/base"', app)
        r2 = self.cli(self.full(["plan"]))
        self.assertEqual(r2.returncode, 0, r2.stderr)
        self.assertIn("already", r2.stdout)

    # 2c
    def test_escape_nesting(self):
        self.write_mapping({"internal/agent": "internal/core/agent",
                            "internal/agent/child": "internal/core/child"})
        f = self.root / "other/other.go"
        f.parent.mkdir(parents=True)
        f.write_text(
            'package other\n\nimport "example.com/repo/internal/agent/child"\n'
            '\nvar _ = child.C\n',
            encoding="utf-8", newline="\n")
        subprocess.run(["git", "add", "-A"], cwd=str(self.root), check=True,
                       capture_output=True)
        subprocess.run(["git", "commit", "-q", "-m", "other"], cwd=str(self.root),
                       check=True, capture_output=True)
        r = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.root / "internal/core/child/child.go").exists())
        self.assertFalse((self.root / "internal/core/agent/child").exists())
        self.assertTrue((self.root / "internal/core/agent/agent.go").exists())
        out = (self.root / "other/other.go").read_text(encoding="utf-8")
        self.assertIn('"example.com/repo/internal/core/child"', out)

    # 2d
    def test_rides_along(self):
        self.write_mapping({"internal/agent": "internal/core/agent",
                            "internal/agent/child": "internal/core/agent/child"})
        r = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((self.root / "internal/core/agent/child/child.go").exists())
        moved_lines = [l for l in r.stdout.splitlines() if l.startswith("moved ")]
        self.assertEqual(len(moved_lines), 1, r.stdout)
        self.assertIn("internal/agent -> internal/core/agent", moved_lines[0])

    # 3
    def test_apply(self):
        self.write_mapping({"internal/agent": "internal/core/agent"})
        # FIX 1: apply without --manifest must not write a manifest in cwd
        stray = TOOLS_DIR / "pkgmove_manifest.json"
        if stray.exists():
            stray.unlink()
        r = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(stray.exists(), "apply without --manifest wrote %s" % stray)
        if stray.exists():
            stray.unlink()
        self.assertFalse((self.root / "internal/agent").exists())
        self.assertTrue((self.root / "internal/core/agent/child/child.go").exists())
        app = (self.root / "cmd/app.go").read_text(encoding="utf-8")
        self.assertIn('ag "example.com/repo/internal/core/agent"', app)
        self.assertIn('"example.com/repo/internal/util"', app)
        self.assertNotIn("example.com/repo/internal/agent\"", app)
        agent_go = (self.root / "internal/core/agent/agent.go").read_text(encoding="utf-8")
        self.assertIn('"example.com/repo/internal/util"', agent_go)
        golangci = (self.root / ".golangci.yml").read_text(encoding="utf-8")
        self.assertIn("- internal/core/agent", golangci)
        self.assertIn("internal/agentresult", golangci)
        docs = (self.root / "docs/note.md").read_text(encoding="utf-8")
        self.assertIn("internal/agent", docs)
        table = (self.root / "docs/package-moves.md").read_text(encoding="utf-8")
        self.assertIn("# Package moves", table)
        self.assertIn("| internal/agent | internal/core/agent | 2026-11-15 |", table)

    # 4
    def test_segment_boundary(self):
        moves = mapping.Moves({"internal/agent": "internal/core/agent"})
        text = "dirs: internal/agent, internal/agentresult/other\n"
        spans = rewrite.scan_nongo(text, moves, "example.com/repo")
        self.assertEqual(len(spans), 1)
        s, e, old, new = spans[0]
        self.assertEqual(old, "internal/agent")
        self.assertEqual(new, "internal/core/agent")
        out, n = rewrite.rewrite_with_spans(text, [(s, e, new)])
        self.assertEqual(n, 1)
        self.assertIn("internal/core/agent", out)
        self.assertIn("internal/agentresult/other", out)
        src = (
            "import (\n"
            '\t"a" "example.com/repo/internal/agent"\n'
            '\t"example.com/repo/internal/agentresult"\n'
            ")\n"
        )
        new_src, changed = rewrite.rewrite_go_source(src, "example.com/repo", moves)
        self.assertEqual(len(changed), 1)
        self.assertEqual(changed[0][0], "example.com/repo/internal/agent")
        self.assertIn('"a" "example.com/repo/internal/core/agent"', new_src)
        self.assertIn('"example.com/repo/internal/agentresult"', new_src)

    # 5
    def test_child_moves_by_prefix(self):
        self.write_mapping({"internal/agent": "internal/core/agent"})
        f = self.root / "other/other.go"
        f.parent.mkdir(parents=True)
        f.write_text(
            'package other\n\nimport "example.com/repo/internal/agent/child"\n'
            '\nvar _ = child.C\n',
            encoding="utf-8", newline="\n")
        subprocess.run(["git", "add", "-A"], cwd=str(self.root), check=True,
                       capture_output=True)
        subprocess.run(["git", "commit", "-q", "-m", "other"], cwd=str(self.root),
                       check=True, capture_output=True)
        r = self.cli(self.full(["apply"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        out = (self.root / "other/other.go").read_text(encoding="utf-8")
        self.assertIn('"example.com/repo/internal/core/agent/child"', out)

    # 6
    def test_idempotency(self):
        self.write_mapping({"internal/agent": "internal/core/agent"})
        r1 = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r1.returncode, 0, r1.stderr)
        snap1 = snapshot(self.root)
        r2 = self.cli(self.full(["apply", "--date", "2026-11-15"]))
        self.assertEqual(r2.returncode, 0, r2.stderr)
        self.assertIn("0 directories moved", r2.stdout)
        self.assertEqual(snapshot(self.root), snap1)
        table = (self.root / "docs/package-moves.md").read_text(encoding="utf-8")
        rows = [l for l in table.splitlines()
                if l.startswith("| internal/agent | internal/core/agent |")]
        self.assertEqual(len(rows), 1)

    # 7
    def test_manifest_verify_success(self):
        self.write_mapping({"internal/agent": "internal/core/agent"})
        out = (self.root / "pkgmove_manifest.json").as_posix()
        r = self.cli(self.full(["manifest", "--out", out, "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = self.cli(self.full(["apply", "--manifest", out, "--date", "2026-11-15"]))
        self.assertEqual(r.returncode, 0, r.stderr)
        r = self.cli(self.full(["verify", "--manifest", out]))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("verify OK", r.stdout)

    # 8
    def test_verify_failure(self):
        self.write_mapping({"internal/agent": "internal/core/agent"})
        out = (self.root / "pkgmove_manifest.json").as_posix()
        self.assertEqual(self.cli(self.full(["manifest", "--out", out])).returncode, 0)
        self.assertEqual(self.cli(self.full(["apply", "--manifest", out])).returncode, 0)
        f = self.root / "internal/core/agent/agent.go"
        text = f.read_text(encoding="utf-8")
        f.write_text(text.replace("func Hello() string { return util.Name() }\n", ""),
                     encoding="utf-8", newline="\n")
        r = self.cli(self.full(["verify", "--manifest", out]))
        self.assertEqual(r.returncode, 1)
        self.assertIn("declarations differ", r.stdout)
        self.assertIn("func Hello() string { return util.Name() }", r.stdout)

    # 9
    def test_moves_longest_prefix(self):
        moves = mapping.Moves({
            "internal/agent": "core/agent",
            "internal/agent/child": "core/kid",
        })
        self.assertEqual(moves.resolve("internal/agent/child/x.go"), "core/kid/x.go")
        self.assertEqual(moves.resolve("internal/agent"), "core/agent")
        self.assertEqual(moves.resolve("internal/other"), None)
        self.assertEqual(moves.reverse("core/kid"), "internal/agent/child")
        self.assertEqual(moves.reverse("core/agent"), "internal/agent")
        self.assertEqual(moves.reverse("core"), None)


if __name__ == "__main__":
    unittest.main()
