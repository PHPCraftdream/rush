#!/usr/bin/env python3
"""License compatibility gate for the `licenses` CI job.

Reads the dependency licenses reported by the ecosystem tools and checks
every one against the policy in .github/license-policy.toml:

  * Go:  `go-licenses report ./...` CSV (columns: package, license URL,
         license name), one file per GOOS; pass each with --go-csv.
  * npm: `pnpm licenses list --prod --json` (a map license -> packages);
         pass it with --pnpm-json.

A license passes when its SPDX expression is satisfied by the policy's
`allowed` list (`A OR B`: one side is enough, `A AND B`: both are needed,
`A WITH exception` is judged by A). An unknown, missing or unparseable
license fails, as does anything not in `allowed`. A dependency can be waved
through only by a reviewed `[[exceptions]]` entry that carries a reason.

Exit code: 0 if every dependency is acceptable, 1 on any violation, 2 on a
usage, policy or input error. Python 3.11+ (tomllib), standard library only.
"""

import argparse
import csv
import json
import os
import re
import sys
import tomllib
from collections import Counter, namedtuple

EXIT_OK = 0
EXIT_VIOLATION = 1
EXIT_USAGE = 2

Dependency = namedtuple("Dependency", "ecosystem name license")
Exception_ = namedtuple("Exception_", "ecosystem name reason")
Policy = namedtuple("Policy", "allowed allowed_display prefixes exceptions")
Violation = namedtuple("Violation", "dep reason")
Report = namedtuple("Report", "violations excepted licenses count")

_TOKEN = re.compile(r"\(|\)|[^\s()]+")
_KEYWORDS = ("AND", "OR", "WITH")


class PolicyError(ValueError):
    """The policy file is missing, malformed or incomplete."""


class ExpressionError(ValueError):
    """A license string is not a valid SPDX expression."""


class _Parser:
    """Recursive-descent SPDX expression evaluator (AND binds tighter than OR)."""

    def __init__(self, tokens, allowed):
        self.tokens = tokens
        self.allowed = allowed
        self.pos = 0

    def peek(self):
        if self.pos < len(self.tokens):
            return self.tokens[self.pos]
        return None

    def peek_keyword(self, word):
        tok = self.peek()
        return tok is not None and tok.upper() == word

    def take(self):
        tok = self.peek()
        if tok is not None:
            self.pos += 1
        return tok

    def parse(self):
        ok, offenders = self.expression()
        if self.pos != len(self.tokens):
            raise ExpressionError("unexpected token %r" % self.tokens[self.pos])
        return ok, offenders

    def expression(self):
        ok, offenders = self.term()
        while self.peek_keyword("OR"):
            self.take()
            right_ok, right_offenders = self.term()
            ok = ok or right_ok
            offenders = offenders + right_offenders
        return ok, offenders

    def term(self):
        ok, offenders = self.factor()
        while self.peek_keyword("AND"):
            self.take()
            right_ok, right_offenders = self.factor()
            ok = ok and right_ok
            offenders = offenders + right_offenders
        return ok, offenders

    def factor(self):
        tok = self.peek()
        if tok is None:
            raise ExpressionError("unexpected end of expression")
        if tok == "(":
            self.take()
            ok, offenders = self.expression()
            if self.peek() != ")":
                raise ExpressionError("missing closing parenthesis")
            self.take()
            return ok, offenders
        if tok == ")" or tok.upper() in _KEYWORDS:
            raise ExpressionError("unexpected token %r" % tok)
        self.take()
        if self.peek_keyword("WITH"):
            self.take()
            exception_id = self.take()
            if exception_id is None or exception_id in ("(", ")"):
                raise ExpressionError("missing license exception after WITH")
        if tok.casefold() in self.allowed:
            return True, []
        return False, [tok]


def evaluate(expression, allowed):
    """Return (ok, offending ids) for an SPDX expression against `allowed`
    (a set of case-folded identifiers). Raises ExpressionError."""
    tokens = _TOKEN.findall(expression or "")
    if not tokens:
        raise ExpressionError("empty license expression")
    return _Parser(tokens, allowed).parse()


def load_policy(path):
    """Parse and validate the TOML policy. Raises PolicyError."""
    try:
        with open(path, "rb") as fh:
            raw = tomllib.load(fh)
    except OSError as exc:
        raise PolicyError("cannot read policy %s: %s" % (path, exc)) from exc
    except tomllib.TOMLDecodeError as exc:
        raise PolicyError("invalid policy %s: %s" % (path, exc)) from exc

    allowed = raw.get("allowed")
    if (
        not isinstance(allowed, list)
        or not allowed
        or not all(isinstance(item, str) and item.strip() for item in allowed)
    ):
        raise PolicyError("policy `allowed` must be a non-empty list of SPDX ids")
    prefixes = raw.get("incompatible_prefixes", [])
    if not isinstance(prefixes, list) or not all(isinstance(p, str) for p in prefixes):
        raise PolicyError("policy `incompatible_prefixes` must be a list of strings")

    exceptions = []
    for idx, entry in enumerate(raw.get("exceptions", [])):
        where = "exceptions[%d]" % idx
        if not isinstance(entry, dict):
            raise PolicyError("%s must be a table" % where)
        ecosystem = entry.get("ecosystem")
        name = entry.get("name")
        reason = entry.get("reason")
        if ecosystem not in ("go", "npm"):
            raise PolicyError('%s.ecosystem must be "go" or "npm"' % where)
        if not isinstance(name, str) or not name.strip():
            raise PolicyError("%s.name must be a non-empty string" % where)
        if not isinstance(reason, str) or not reason.strip():
            raise PolicyError("%s.reason is mandatory" % where)
        exceptions.append(Exception_(ecosystem, name.strip(), reason.strip()))

    return Policy(
        allowed=frozenset(item.strip().casefold() for item in allowed),
        allowed_display=tuple(sorted(item.strip() for item in allowed)),
        prefixes=tuple(p.casefold() for p in prefixes if p),
        exceptions=tuple(exceptions),
    )


def read_go_csv(path):
    """Parse a go-licenses CSV: package, license URL, license name."""
    deps = []
    with open(path, newline="", encoding="utf-8") as fh:
        for record in csv.reader(fh):
            if not record:
                continue
            if len(record) != 3:
                raise ValueError(
                    "%s: expected 3 columns, got %d: %r" % (path, len(record), record)
                )
            deps.append(Dependency("go", record[0].strip(), record[2].strip()))
    return deps


def read_pnpm_json(path):
    """Parse `pnpm licenses list --json`: {license: [{name, versions, ...}]}."""
    with open(path, encoding="utf-8") as fh:
        text = fh.read().strip()
    if not text:
        return []
    data = json.loads(text)
    if not isinstance(data, dict):
        raise ValueError("%s: expected a JSON object keyed by license" % path)
    deps = []
    for license_name, packages in data.items():
        if not isinstance(packages, list):
            raise ValueError("%s: licenses[%r] is not a list" % (path, license_name))
        for pkg in packages:
            name = pkg.get("name") if isinstance(pkg, dict) else None
            if not isinstance(name, str) or not name:
                raise ValueError("%s: package without a name under %r" % (path, license_name))
            deps.append(Dependency("npm", name, license_name.strip()))
    return deps


def _find_exception(dep, exceptions):
    for exc in exceptions:
        if exc.ecosystem != dep.ecosystem:
            continue
        if dep.name == exc.name:
            return exc
        if dep.ecosystem == "go" and dep.name.startswith(exc.name.rstrip("/") + "/"):
            return exc
    return None


def _reason(dep, policy):
    try:
        ok, offenders = evaluate(dep.license, policy.allowed)
    except ExpressionError as exc:
        return "unparseable license %r (%s)" % (dep.license, exc)
    if ok:
        return None
    for offender in offenders:
        folded = offender.casefold()
        if any(folded.startswith(prefix) for prefix in policy.prefixes):
            return "incompatible license %s" % ", ".join(offenders)
    return "license not in the allowed list: %s" % ", ".join(offenders)


def check(deps, policy):
    """Evaluate every distinct dependency. Returns a Report."""
    seen = set()
    violations = []
    excepted = []
    licenses = Counter()
    for dep in sorted(set(deps)):
        if dep in seen:
            continue
        seen.add(dep)
        licenses[dep.license] += 1
        reason = _reason(dep, policy)
        if reason is None:
            continue
        exc = _find_exception(dep, policy.exceptions)
        if exc is not None:
            excepted.append(Violation(dep, "excepted: %s" % exc.reason))
        else:
            violations.append(Violation(dep, reason))
    return Report(violations, excepted, licenses, len(seen))


def render_text(report):
    lines = ["license check: %d dependencies, %d distinct licenses" % (report.count, len(report.licenses))]
    for license_name, count in sorted(report.licenses.items()):
        lines.append("  %5d  %s" % (count, license_name))
    for item in report.excepted:
        lines.append("excepted  %s %s (%s): %s" % (item.dep.ecosystem, item.dep.name, item.dep.license, item.reason))
    for item in report.violations:
        lines.append("VIOLATION %s %s (%s): %s" % (item.dep.ecosystem, item.dep.name, item.dep.license, item.reason))
    lines.append("result: %s" % ("FAIL, %d violation(s)" % len(report.violations) if report.violations else "ok"))
    return "\n".join(lines)


def render_markdown(report, policy):
    lines = ["### Dependency license check", ""]
    if report.violations:
        lines += ["**%d violation(s)**" % len(report.violations), "", "| ecosystem | dependency | license | problem |", "|---|---|---|---|"]
        for item in report.violations:
            lines.append("| %s | `%s` | %s | %s |" % (item.dep.ecosystem, item.dep.name, item.dep.license, item.reason))
        lines.append("")
    else:
        lines += ["All %d dependencies are compatible with the policy." % report.count, ""]
    if report.excepted:
        lines += ["Reviewed exceptions:", ""]
        for item in report.excepted:
            lines.append("- `%s` (%s): %s" % (item.dep.name, item.dep.license, item.reason))
        lines.append("")
    lines += ["| license | dependencies |", "|---|---|"]
    for license_name, count in sorted(report.licenses.items()):
        lines.append("| %s | %d |" % (license_name, count))
    lines += ["", "Allowed: " + ", ".join(policy.allowed_display), ""]
    return "\n".join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description="License compatibility gate.")
    parser.add_argument("--policy", required=True, help="path to license-policy.toml")
    parser.add_argument("--go-csv", action="append", default=[], help="go-licenses CSV (repeatable)")
    parser.add_argument("--pnpm-json", action="append", default=[], help="pnpm licenses JSON (repeatable)")
    parser.add_argument("--summary", help="append a Markdown summary to this file")
    args = parser.parse_args(argv)
    if not args.go_csv and not args.pnpm_json:
        print("license_check: no input given (--go-csv / --pnpm-json)", file=sys.stderr)
        return EXIT_USAGE

    try:
        policy = load_policy(args.policy)
        deps = []
        for path in args.go_csv:
            deps.extend(read_go_csv(path))
        for path in args.pnpm_json:
            deps.extend(read_pnpm_json(path))
    except (PolicyError, OSError, ValueError) as exc:
        print("license_check: %s" % exc, file=sys.stderr)
        return EXIT_USAGE

    report = check(deps, policy)
    print(render_text(report))
    if os.environ.get("GITHUB_ACTIONS") == "true":
        for item in report.violations:
            print(
                "::error title=Incompatible dependency license::%s %s (%s): %s"
                % (item.dep.ecosystem, item.dep.name, item.dep.license, item.reason)
            )
    if args.summary:
        with open(args.summary, "a", encoding="utf-8") as fh:
            fh.write(render_markdown(report, policy) + "\n")
    return EXIT_VIOLATION if report.violations else EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
