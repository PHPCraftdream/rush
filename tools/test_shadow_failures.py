"""Tests for tools/shadow_failures.py.

Runnable under `python -I -m unittest discover -s tools -p
'test_shadow_failures.py'` from the repo root: isolated mode strips the
script dir from sys.path, so we insert the directory containing this file
(which also holds shadow_failures.py) explicitly.
"""

import os
import subprocess
import sys
import tempfile
import unittest

TOOLS_DIR = os.path.dirname(os.path.abspath(__file__))
if TOOLS_DIR not in sys.path:
    sys.path.insert(0, TOOLS_DIR)

import shadow_failures  # noqa: E402


def run_cli(log_text, step_failures=None):
    """Run the CLI on a temp log; return (returncode, md_text, txt_text)."""
    with tempfile.TemporaryDirectory() as tmp:
        log = os.path.join(tmp, "log.txt")
        out_md = os.path.join(tmp, "out.md")
        with open(log, "w", encoding="utf-8") as fh:
            fh.write(log_text)
        cmd = [sys.executable, "-I", os.path.join(TOOLS_DIR, "shadow_failures.py"),
               log, "-o", out_md]
        if step_failures is not None:
            tsv = os.path.join(tmp, "step-failures.tsv")
            with open(tsv, "w", encoding="utf-8", newline="") as fh:
                fh.write(step_failures)
            cmd += ["--step-failures", tsv]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        with open(out_md, encoding="utf-8") as fh:
            md = fh.read()
        with open(os.path.splitext(out_md)[0] + ".txt", encoding="utf-8") as fh:
            txt = fh.read()
    return proc.returncode, md, txt


class ShadowFailuresTests(unittest.TestCase):
    def test_nested_fail_and_package_row(self):
        log = (
            "=== RUN   TestFoo\n"
            "--- FAIL: TestFoo (0.01s)\n"
            "    x_test.go:12: boom\n"
            "=== RUN   TestFoo/sub_y\n"
            "--- FAIL: TestFoo/sub_y (0.00s)\n"
            "--- FAIL: TestFoo/sub_y/sub_z (0.00s)\n"
            "FAIL\tpkg1\t0.1s\n"
        )
        rc, md, txt = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertIn("| pkg1 | TestFoo |", md)
        self.assertIn("| pkg1 |  |", md)  # package row kept
        self.assertNotIn("sub_y", md)  # top-level only
        self.assertEqual(txt, "pkg1\t\npkg1\tTestFoo\n")

    def test_panic_row(self):
        # NEW attribution: the panic belongs to the NEXT package summary
        # line (pkg2), not to the preceding `ok github.com/x/before`.
        log = (
            "ok  github.com/x/before 0.2s\n"
            "panic: runtime index out of range\n"
            "FAIL\tpkg2 0.3s\n"
        )
        rc, md, _ = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertIn("| pkg2 | panic |", md)
        self.assertIn("| pkg2 |  |", md)
        self.assertNotIn("| github.com/x/before | panic |", md)

    def test_build_failed(self):
        log = "FAIL\tpkg2 [build failed]\n"
        rc, md, _ = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertIn("| pkg2 |  |", md)

    def test_no_failures_exit_zero(self):
        log = "ok  \tpkg3\t0.5s\n"
        rc, md, txt = run_cli(log)
        self.assertEqual(rc, 0)
        self.assertIn("| --- |", md)
        self.assertNotIn("| pkg3", md)
        self.assertEqual(txt, "")

    def test_dedup_reruns(self):
        log = (
            "--- FAIL: TestA\n"
            "FAIL\tpkgR 0.1s\n"
            "--- FAIL: TestA\n"
            "FAIL\tpkgR 0.1s\n"
        )
        rc, md, txt = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertEqual(md.count("TestA"), 1)  # one data row despite rerun
        self.assertEqual(txt.count("pkgR\tTestA"), 1)
        self.assertEqual(txt.count("pkgR\t\n"), 1)

    def test_sorted_by_package_then_test(self):
        log = (
            "--- FAIL: TestA\n"
            "ok  a/pkg 0.1s\n"
            "--- FAIL: TestZ\n"
            "FAIL  z/pkg 0.1s\n"
        )
        _, md, _ = run_cli(log)
        data_rows = [
            line for line in md.splitlines()
            if line.startswith("| ") and "package" not in line and "---" not in line
        ]
        self.assertEqual(
            data_rows,
            ["| a/pkg | TestA |", "| z/pkg |  |", "| z/pkg | TestZ |"],
        )

    def test_step_failures_tsv_merged_and_deduped(self):
        # Duplicate internal/app rows collapse to one; merged with parsed
        # rows; sorted; exit 1.
        tsv = "internal/app\t1\r\ninternal/app\t1\nsdk\t2\n"
        rc, md, txt = run_cli(
            "--- FAIL: TestA\nFAIL  a/pkg 0.1s\n", step_failures=tsv
        )
        self.assertEqual(rc, 1)
        self.assertEqual(md.count("(step failed, exit 1)"), 1)  # deduped
        self.assertIn("| internal/app | (step failed, exit 1) |", md)
        self.assertIn("| sdk | (step failed, exit 2) |", md)
        self.assertIn("| a/pkg | TestA |", md)  # parsed rows still merged
        self.assertEqual(txt.count("internal/app\t(step failed, exit 1)"), 1)

    def test_step_failures_empty_tsv_clean_log_exit_zero(self):
        rc, md, txt = run_cli("ok  a/pkg 0.1s\n", step_failures="\n")
        self.assertEqual(rc, 0)
        self.assertEqual(txt, "")

    def test_indented_subtest_fail_captured(self):
        log = "    --- FAIL: TestFoo/sub_y (0.00s)\nFAIL\tpkgS 0.1s\n"
        rc, md, _ = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertIn("| pkgS | TestFoo |", md)  # collapsed to top level
        self.assertNotIn("sub_y", md)

    def test_missing_log_is_a_row_and_keeps_step_failures(self):
        # A shard that failed before creating its log must still produce an
        # inventory: the step-failure rows survive and the missing log is
        # itself listed; exit 1.
        with tempfile.TemporaryDirectory() as tmp:
            out_md = os.path.join(tmp, "out.md")
            tsv = os.path.join(tmp, "step.tsv")
            with open(tsv, "w", encoding="utf-8", newline="") as fh:
                fh.write("agent-shards\t1\n")
            proc = subprocess.run(
                [sys.executable, "-I",
                 os.path.join(TOOLS_DIR, "shadow_failures.py"),
                 os.path.join(tmp, "definitely-missing.log"),
                 "--step-failures", tsv, "-o", out_md],
                capture_output=True, text=True,
            )
            with open(out_md, encoding="utf-8") as fh:
                md = fh.read()
        self.assertEqual(proc.returncode, 1)
        self.assertIn("cannot read", proc.stderr)
        self.assertIn("| agent-shards | (step failed, exit 1) |", md)
        self.assertIn("| definitely-missing.log | (log missing or unreadable) |", md)


if __name__ == "__main__":
    unittest.main()
