"""Tests for tools/license_check.py.

Runnable under `python -I -m unittest discover -s tools -p
'test_license_check.py'` from the repo root: isolated mode strips the script
dir from sys.path, so we insert the directory containing this file (which also
holds license_check.py) explicitly.
"""

import contextlib
import io
import json
import os
import sys
import tempfile
import unittest

TOOLS_DIR = os.path.dirname(os.path.abspath(__file__))
if TOOLS_DIR not in sys.path:
    sys.path.insert(0, TOOLS_DIR)

import license_check  # noqa: E402

POLICY = """
allowed = ["MIT", "Apache-2.0", "BSD-3-Clause", "MPL-2.0"]
incompatible_prefixes = ["GPL", "AGPL", "LGPL"]

[[exceptions]]
ecosystem = "go"
name = "example.com/dual"
reason = "dual licensed, MIT selected"

[[exceptions]]
ecosystem = "npm"
name = "odd-package"
reason = "reviewed: custom permissive text"
"""

ALLOWED = frozenset(["mit", "apache-2.0", "bsd-3-clause", "mpl-2.0"])


def write(directory, name, text):
    path = os.path.join(directory, name)
    with open(path, "w", encoding="utf-8", newline="") as fh:
        fh.write(text)
    return path


def run_main(argv):
    out = io.StringIO()
    err = io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = license_check.main(argv)
    return code, out.getvalue(), err.getvalue()


class EvaluateTest(unittest.TestCase):
    def test_single_identifier(self):
        self.assertEqual(license_check.evaluate("MIT", ALLOWED), (True, []))
        self.assertEqual(license_check.evaluate("mit", ALLOWED), (True, []))
        self.assertEqual(license_check.evaluate("GPL-3.0-only", ALLOWED), (False, ["GPL-3.0-only"]))

    def test_or_needs_one_side(self):
        ok, offenders = license_check.evaluate("GPL-3.0-only OR MIT", ALLOWED)
        self.assertTrue(ok)
        self.assertEqual(offenders, ["GPL-3.0-only"])

    def test_and_needs_both_sides(self):
        ok, offenders = license_check.evaluate("MIT AND GPL-2.0-only", ALLOWED)
        self.assertFalse(ok)
        self.assertEqual(offenders, ["GPL-2.0-only"])
        self.assertTrue(license_check.evaluate("MIT AND Apache-2.0", ALLOWED)[0])

    def test_and_binds_tighter_than_or(self):
        # GPL OR (MIT AND Apache) -> true; (GPL OR MIT) AND LGPL would be false.
        self.assertTrue(license_check.evaluate("GPL-3.0-only OR MIT AND Apache-2.0", ALLOWED)[0])
        self.assertFalse(license_check.evaluate("(GPL-3.0-only OR MIT) AND LGPL-2.1-only", ALLOWED)[0])

    def test_with_exception_is_judged_by_the_license(self):
        self.assertTrue(license_check.evaluate("Apache-2.0 WITH LLVM-exception", ALLOWED)[0])
        self.assertFalse(license_check.evaluate("GPL-2.0-only WITH Classpath-exception-2.0", ALLOWED)[0])

    def test_unparseable_expressions(self):
        for bad in ["", "   ", "MIT AND", "OR MIT", "(MIT", "MIT)", "MIT MIT", "MIT WITH"]:
            with self.subTest(expression=bad):
                with self.assertRaises(license_check.ExpressionError):
                    license_check.evaluate(bad, ALLOWED)


class PolicyTest(unittest.TestCase):
    def load(self, text):
        with tempfile.TemporaryDirectory() as tmp:
            return license_check.load_policy(write(tmp, "policy.toml", text))

    def test_valid_policy(self):
        policy = self.load(POLICY)
        self.assertIn("mit", policy.allowed)
        self.assertEqual(len(policy.exceptions), 2)
        self.assertEqual(policy.prefixes, ("gpl", "agpl", "lgpl"))

    def test_rejects_empty_allowed(self):
        with self.assertRaises(license_check.PolicyError):
            self.load("allowed = []\n")

    def test_rejects_exception_without_reason(self):
        text = 'allowed = ["MIT"]\n[[exceptions]]\necosystem = "go"\nname = "x"\nreason = "  "\n'
        with self.assertRaises(license_check.PolicyError):
            self.load(text)

    def test_rejects_unknown_ecosystem(self):
        text = 'allowed = ["MIT"]\n[[exceptions]]\necosystem = "pip"\nname = "x"\nreason = "r"\n'
        with self.assertRaises(license_check.PolicyError):
            self.load(text)

    def test_rejects_invalid_toml(self):
        with self.assertRaises(license_check.PolicyError):
            self.load("allowed = [")

    def test_missing_file(self):
        with self.assertRaises(license_check.PolicyError):
            license_check.load_policy(os.path.join(tempfile.gettempdir(), "no-such-license-policy.toml"))


class ParsingTest(unittest.TestCase):
    def test_go_csv(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write(
                tmp,
                "go.csv",
                "github.com/a/b,https://x/LICENSE,MIT\n\ngolang.org/x/sys/unix,https://y/LICENSE,BSD-3-Clause\n",
            )
            deps = license_check.read_go_csv(path)
        self.assertEqual(
            deps,
            [
                license_check.Dependency("go", "github.com/a/b", "MIT"),
                license_check.Dependency("go", "golang.org/x/sys/unix", "BSD-3-Clause"),
            ],
        )

    def test_go_csv_wrong_columns(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write(tmp, "go.csv", "only,two\n")
            with self.assertRaises(ValueError):
                license_check.read_go_csv(path)

    def test_pnpm_json(self):
        data = {"MIT": [{"name": "react", "versions": ["19.0.0"]}], "(MIT OR CC0-1.0)": [{"name": "lodash.merge"}]}
        with tempfile.TemporaryDirectory() as tmp:
            deps = license_check.read_pnpm_json(write(tmp, "pnpm.json", json.dumps(data)))
        self.assertEqual(
            sorted(deps),
            [
                license_check.Dependency("npm", "lodash.merge", "(MIT OR CC0-1.0)"),
                license_check.Dependency("npm", "react", "MIT"),
            ],
        )

    def test_pnpm_json_empty_and_malformed(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(license_check.read_pnpm_json(write(tmp, "empty.json", " \n")), [])
            with self.assertRaises(ValueError):
                license_check.read_pnpm_json(write(tmp, "list.json", "[]"))
            with self.assertRaises(ValueError):
                license_check.read_pnpm_json(write(tmp, "noname.json", '{"MIT": [{"versions": []}]}'))


class CheckTest(unittest.TestCase):
    def setUp(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.policy = license_check.load_policy(write(tmp, "policy.toml", POLICY))

    def test_clean(self):
        deps = [
            license_check.Dependency("go", "github.com/a/b", "MIT"),
            license_check.Dependency("npm", "react", "MIT"),
        ]
        report = license_check.check(deps, self.policy)
        self.assertEqual(report.violations, [])
        self.assertEqual(report.count, 2)
        self.assertEqual(report.licenses["MIT"], 2)

    def test_duplicates_across_goos_collapse(self):
        dep = license_check.Dependency("go", "golang.org/x/sys/unix", "BSD-3-Clause")
        report = license_check.check([dep, dep, dep], self.policy)
        self.assertEqual(report.count, 1)

    def test_violation_messages(self):
        deps = [
            license_check.Dependency("go", "github.com/copyleft/lib", "GPL-3.0-only"),
            license_check.Dependency("go", "github.com/mystery/lib", "Unknown"),
            license_check.Dependency("npm", "weird", "SEE LICENSE IN LICENSE.txt"),
            license_check.Dependency("npm", "agpl-thing", "AGPL-3.0-or-later"),
        ]
        report = license_check.check(deps, self.policy)
        reasons = {v.dep.name: v.reason for v in report.violations}
        self.assertEqual(len(reasons), 4)
        self.assertIn("incompatible license GPL-3.0-only", reasons["github.com/copyleft/lib"])
        self.assertIn("not in the allowed list: Unknown", reasons["github.com/mystery/lib"])
        self.assertIn("unparseable license", reasons["weird"])
        self.assertIn("incompatible license AGPL-3.0-or-later", reasons["agpl-thing"])

    def test_unparseable_license_is_a_violation(self):
        report = license_check.check([license_check.Dependency("npm", "x", "MIT AND")], self.policy)
        self.assertEqual(len(report.violations), 1)
        self.assertIn("unparseable", report.violations[0].reason)

    def test_exception_by_prefix_and_exact_name(self):
        deps = [
            license_check.Dependency("go", "example.com/dual", "GPL-3.0-only"),
            license_check.Dependency("go", "example.com/dual/sub/pkg", "GPL-3.0-only"),
            license_check.Dependency("go", "example.com/dualistic", "GPL-3.0-only"),
            license_check.Dependency("npm", "odd-package", "Unknown"),
            license_check.Dependency("npm", "odd-package-2", "Unknown"),
        ]
        report = license_check.check(deps, self.policy)
        self.assertEqual(
            sorted(v.dep.name for v in report.excepted),
            ["example.com/dual", "example.com/dual/sub/pkg", "odd-package"],
        )
        self.assertEqual(
            sorted(v.dep.name for v in report.violations),
            ["example.com/dualistic", "odd-package-2"],
        )

    def test_exception_does_not_cross_ecosystems(self):
        report = license_check.check([license_check.Dependency("npm", "example.com/dual", "GPL-3.0-only")], self.policy)
        self.assertEqual(len(report.violations), 1)


class MainTest(unittest.TestCase):
    def test_exit_codes_and_summary(self):
        with tempfile.TemporaryDirectory() as tmp:
            policy = write(tmp, "policy.toml", POLICY)
            good = write(tmp, "good.csv", "github.com/a/b,u,MIT\n")
            bad = write(tmp, "bad.csv", "github.com/a/b,u,MIT\ngithub.com/c/d,u,GPL-3.0-only\n")
            summary = os.path.join(tmp, "summary.md")

            code, out, _ = run_main(["--policy", policy, "--go-csv", good, "--summary", summary])
            self.assertEqual(code, license_check.EXIT_OK)
            self.assertIn("result: ok", out)

            code, out, _ = run_main(["--policy", policy, "--go-csv", good, "--go-csv", bad, "--summary", summary])
            self.assertEqual(code, license_check.EXIT_VIOLATION)
            self.assertIn("VIOLATION go github.com/c/d (GPL-3.0-only)", out)
            with open(summary, encoding="utf-8") as fh:
                text = fh.read()
            self.assertIn("1 violation(s)", text)
            self.assertIn("All 1 dependencies are compatible", text)

    def test_usage_errors(self):
        with tempfile.TemporaryDirectory() as tmp:
            policy = write(tmp, "policy.toml", POLICY)
            self.assertEqual(run_main(["--policy", policy])[0], license_check.EXIT_USAGE)
            missing = os.path.join(tmp, "missing.csv")
            self.assertEqual(run_main(["--policy", policy, "--go-csv", missing])[0], license_check.EXIT_USAGE)
            self.assertEqual(
                run_main(["--policy", os.path.join(tmp, "nope.toml"), "--go-csv", missing])[0],
                license_check.EXIT_USAGE,
            )

    def test_github_annotation(self):
        old = os.environ.get("GITHUB_ACTIONS")
        os.environ["GITHUB_ACTIONS"] = "true"
        try:
            with tempfile.TemporaryDirectory() as tmp:
                policy = write(tmp, "policy.toml", POLICY)
                bad = write(tmp, "bad.csv", "github.com/c/d,u,GPL-3.0-only\n")
                code, out, _ = run_main(["--policy", policy, "--go-csv", bad])
        finally:
            if old is None:
                del os.environ["GITHUB_ACTIONS"]
            else:
                os.environ["GITHUB_ACTIONS"] = old
        self.assertEqual(code, license_check.EXIT_VIOLATION)
        self.assertIn("::error title=Incompatible dependency license::go github.com/c/d", out)


if __name__ == "__main__":
    unittest.main()
