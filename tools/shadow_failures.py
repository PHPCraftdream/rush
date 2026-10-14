#!/usr/bin/env python3
"""Collect a complete failure inventory from tee'd `go test` logs.

Approach for associating tests with packages (see the parsing rules below):
`go test` prints each package's `--- FAIL:` lines BEFORE that package's
trailing summary line (`FAIL <pkg> ...` or `ok <pkg> ...`), so per-test
failures are buffered as "pending" and attached to the NEXT package line
seen (`ok` or `FAIL`); the package itself is also emitted as its own row on
a `FAIL` line (package + per-test rows are distinct; a package row is never
dropped because its tests are already listed). Tests with no following
package line stay attributed to "(unknown)". De-duplication of (package,
test) pairs happens at output time, so reruns/retries in the log collapse
to one row.

Exit code: 0 if no failures found, 1 if any found, 2 on usage/IO errors.
"""

import argparse
import os
import re
import sys

RE_OK = re.compile(r"^ok\s+(\S+)")
RE_FAIL = re.compile(r"^FAIL\s+(\S+)(?:\s+\[build failed\]|\s+[0-9])")
RE_TEST_FAIL = re.compile(r"^--- FAIL:\s+(\S+)")
RE_PANIC = re.compile(r"^panic:\s*(.*)")

UNKNOWN = "(unknown)"


def _top_level(name):
    return name.split("/", 1)[0]


def parse_log(text, rows):
    """Feed one log's lines into the `rows` set of (package, test) pairs."""
    current_pkg = UNKNOWN
    pending = []
    for raw in text.splitlines():
        line = raw.rstrip()
        m = RE_OK.match(line)
        if m:
            current_pkg = m.group(1)
            for test in pending:
                rows.add((current_pkg, test))
            pending = []
            continue
        m = RE_FAIL.match(line)
        if m:
            current_pkg = m.group(1)
            rows.add((current_pkg, ""))
            for test in pending:
                rows.add((current_pkg, test))
            pending = []
            continue
        m = RE_TEST_FAIL.match(line)
        if m:
            pending.append(_top_level(m.group(1)))
            continue
        m = RE_PANIC.match(line)
        if m:
            rows.add((current_pkg, "panic"))
            continue
    for test in pending:
        rows.add((UNKNOWN, test))


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Extract a de-duplicated go-test failure inventory."
    )
    parser.add_argument("logs", nargs="+", help="go test log files")
    parser.add_argument(
        "-o", "--out", required=True, help="path of the markdown report"
    )
    args = parser.parse_args(argv)

    rows = set()
    for path in args.logs:
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as fh:
                parse_log(fh.read(), rows)
        except OSError as exc:
            print(
                "shadow_failures: cannot read %s: %s" % (path, exc),
                file=sys.stderr,
            )
            return 2

    ordered = sorted(rows)
    out_md = args.out
    out_txt = os.path.splitext(out_md)[0] + ".txt"

    md = ["| package | test |", "| --- | --- |"]
    for pkg, test in ordered:
        md.append("| %s | %s |" % (pkg, test))
    md_text = "\n".join(md) + "\n"
    txt_text = "".join("%s\t%s\n" % (pkg, test) for pkg, test in ordered)

    with open(out_md, "w", encoding="utf-8") as fh:
        fh.write(md_text)
    with open(out_txt, "w", encoding="utf-8") as fh:
        fh.write(txt_text)

    print("shadow_failures: %d failure row(s)" % len(ordered), file=sys.stderr)
    return 1 if ordered else 0


if __name__ == "__main__":
    sys.exit(main())
