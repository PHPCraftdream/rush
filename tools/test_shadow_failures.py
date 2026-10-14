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


def run_cli(log_text):
    """Run the CLI on a temp log; return (returncode, md_text, txt_text)."""
    with tempfile.TemporaryDirectory() as tmp:
        log = os.path.join(tmp, "log.txt")
        out_md = os.path.join(tmp, "out.md")
        with open(log, "w", encoding="utf-8") as fh:
            fh.write(log_text)
        proc = subprocess.run(
            [sys.executable, "-I", os.path.join(TOOLS_DIR, "shadow_failures.py"),
             log, "-o", out_md],
            capture_output=True, text=True,
        )
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
        log = (
            "ok  github.com/x/before 0.2s\n"
            "panic: runtime index out of range\n"
            "FAIL\tpkg2 0.3s\n"
        )
        rc, md, _ = run_cli(log)
        self.assertEqual(rc, 1)
        self.assertIn("| github.com/x/before | panic |", md)
        self.assertIn("| pkg2 |  |", md)

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

    def test_missing_file_exit_two(self):
        proc = subprocess.run(
            [sys.executable, "-I", os.path.join(TOOLS_DIR, "shadow_failures.py"),
             os.path.join(TOOLS_DIR, "definitely-missing.log"), "-o",
             os.path.join(tempfile.gettempdir(), "sf-out.md")],
            capture_output=True, text=True,
        )
        self.assertEqual(proc.returncode, 2)
        self.assertIn("cannot read", proc.stderr)


if __name__ == "__main__":
    unittest.main()
