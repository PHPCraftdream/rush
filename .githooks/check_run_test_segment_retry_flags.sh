#!/usr/bin/env bash
# check_run_test_segment_retry_flags.sh: self-test for the retry paths of
# run_test_segment in .githooks/pre-push (task #892).
#
# The invariant under test: EVERY go test invocation run_test_segment
# builds -- the first attempt and BOTH retry branches ("package not
# identified" and "package(s) identified") -- must carry the segment's
# throttling flags (-parallel 2 / -p 2). The identified-package retry once
# rebuilt its command from the failed package list alone, silently
# dropping the flags: after a flake, precisely when memory pressure from
# the first attempt was already at its peak, the retry re-armed the
# Windows commit-limit OOM (ERROR_COMMITMENT_LIMIT, errno=1455) the
# throttling exists to prevent.
#
# Mechanism: only the run_test_segment function is extracted from
# pre-push (awk: the first column-0 "run_test_segment() {" line through
# the first column-0 "}") and sourced into this shell -- sourcing the
# whole file would execute its top-level steps (go build, golangci-lint,
# the real test segments). The helper functions the extracted body calls
# (step, fail, run_capped) are stubbed here; run_capped is a pure argv
# passthrough so the assertions see exactly the argv run_test_segment
# hands to go test, independent of the machine-local safego.ps1 wrapper.
# `go` itself is a plain shell function rather than a PATH shim -- no
# `export -f` needed, because the sourced body runs in this same shell
# process -- that fails its FIRST invocation with a realistic
# "FAIL<TAB>pkg" line (or flag-less noise, for case 2) and records every
# invocation's full argv to a file.
#
# RUSH_PRE_PUSH_UNDER_TEST overrides which pre-push file is checked;
# the default is the sibling pre-push, which is also how pre-push
# invokes this script as one of its own steps. The override exists to
# let this script prove it FAILS against the pre-fix function body
# without mutating the working tree.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
target="${RUSH_PRE_PUSH_UNDER_TEST:-$script_dir/pre-push}"
if [ ! -f "$target" ]; then
	printf 'check_run_test_segment_retry_flags.sh: cannot find %s\n' "$target" >&2
	exit 1
fi

extracted="$(mktemp)"
argv_log="$(mktemp)"
trap 'rm -f "$extracted" "$argv_log" "$count_file"' EXIT
# Pipeline subshells lose variable updates, so the invocation counter
# lives in a file: the first-attempt go call runs on the left side of a
# pipe and its writes must survive into the retry branch.
count_file="$(mktemp)"
printf '0' >"$count_file"

# Extract the function only: from the line that opens it at column 0
# through the first column-0 closing brace (the body itself is
# tab-indented, so the first such brace is the function's own).
awk '/^run_test_segment\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "$target" >"$extracted"
if [ ! -s "$extracted" ] || ! grep -q '^run_test_segment() {' "$extracted"; then
	printf 'check_run_test_segment_retry_flags.sh: could not extract run_test_segment from %s\n' "$target" >&2
	exit 1
fi

# Stubs for the helpers the extracted body calls. fail() must abort the
# test: a retry failing twice is a state this test must never reach.
step() { printf 'step: %s\n' "$1"; }
fail() {
	printf 'FAIL: fail() reached (a retry failed twice): %s\n' "$1" >&2
	exit 1
}
# Passthrough wrapper: the machine-local safego.ps1 memory cap is out of
# scope here; what is under test is the argv, not the wrapper.
run_capped() {
	local size="$1" timeout="$2"
	shift 2
	"$@"
}

# Mock go: first invocation fails like a real single-package failure
# (tab-separated ^FAIL line when first_attempt_fail_line=1, otherwise
# failure noise with no parsable FAIL line); every invocation's full
# argv is recorded verbatim for the assertions below.
first_attempt_fail_line=1
go() {
	local n
	n=$(( $(cat "$count_file") + 1 ))
	printf '%s' "$n" >"$count_file"
	printf '%s\n' "$*" >>"$argv_log"
	if [ "$n" -eq 1 ]; then
		if [ "$first_attempt_fail_line" -eq 1 ]; then
			printf 'FAIL\tgithub.com/example/pkg\t1.23s\n'
		else
			printf 'fork/exec: no such file or directory\n'
		fi
		return 1
	fi
	return 0
}

# shellcheck source=/dev/null
. "$extracted"

failures=0

expect_status() {
	desc="$1"
	want="$2"
	got="$3"
	if [ "$got" -eq "$want" ]; then
		printf 'PASS: %s (exit %s)\n' "$desc" "$got"
	else
		printf 'FAIL: %s -- expected exit %s, got %s\n' "$desc" "$want" "$got"
		failures=$((failures + 1))
	fi
}

expect_argv_line_contains() {
	desc="$1"
	line_no="$2"
	want="$3"
	got="$(sed -n "${line_no}p" "$argv_log")"
	case "$got" in
	*"$want"*)
		printf 'PASS: %s\n' "$desc"
		;;
	*)
		printf 'FAIL: %s -- argv line %s is [%s], expected it to contain [%s]\n' "$desc" "$line_no" "$got" "$want"
		failures=$((failures + 1))
		;;
	esac
}

# Case 1 -- THE regression: the first attempt fails with an identifiable
# package, so the retry takes the identified-package branch. Before the
# fix, that branch rebuilt the command from the package list alone and
# the second argv line lost "-parallel 2".
: >"$argv_log"
printf '0' >"$count_file"
first_attempt_fail_line=1
run_test_segment "selftest case 1 (identified-package retry)" 1g 30 "-parallel 2" ./some/pkg/ >/dev/null
expect_status "case 1: segment passes via retry" 0 "$?"
expect_argv_line_contains "case 1: first attempt carries the throttling flags" 1 "-parallel 2"
expect_argv_line_contains "case 1: first attempt tests the requested package" 1 "./some/pkg/"
expect_argv_line_contains "case 1: RETRY carries the throttling flags (the task #892 regression)" 2 "-parallel 2"

# Case 2 -- refactor guard: with no parsable FAIL line the retry re-runs
# the whole original segment. This branch preserved the flags even
# before the fix; the assertion pins that the restructured function
# keeps it so.
: >"$argv_log"
printf '0' >"$count_file"
first_attempt_fail_line=0
run_test_segment "selftest case 2 (whole-segment retry)" 1g 30 "-parallel 2" ./case2/pkg/ >/dev/null
expect_status "case 2: segment passes via retry" 0 "$?"
expect_argv_line_contains "case 2: RETRY carries the throttling flags" 2 "-parallel 2"
expect_argv_line_contains "case 2: RETRY re-runs the original package pattern" 2 "./case2/pkg/"

if [ "$failures" -gt 0 ]; then
	printf 'check_run_test_segment_retry_flags: %d failure(s) against %s\n' "$failures" "$target"
	exit 1
fi
printf 'check_run_test_segment_retry_flags: all cases passed against %s\n' "$target"
exit 0
