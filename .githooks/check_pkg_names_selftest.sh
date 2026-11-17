#!/usr/bin/env bash
# check_pkg_names_selftest.sh: self-test for .githooks/check_pkg_names.sh
# (the unique-package-name ratchet).
#
# Guard cases build throwaway git repos under the system temp directory
# and run the real guard inside them, asserting:
#   1. two directories with distinct package names pass;
#   2. two directories with the same name fail (both are named);
#   3. package main directories never collide;
#   4. a directory with only test files contributes nothing;
#   5. testdata is invisible to the guard;
#   6. an allowlisted name that grows fails (the ratchet);
#   7. an allowlisted name back to one declaring directory fails with
#      the "remove it from" message (the list must shrink);
#   8. the real repository passes with its seeded allowlist.
#
# mktemp -d is deliberately NOT used: this repository's Windows shells
# hand out a virtual /tmp path that child processes cannot resolve, so
# the temp root is built from the standard environment variables, and
# every path below is used in ways (shell redirects, git -C, awk file
# arguments) that survive Windows-style separators. No Windows path is
# ever passed through `awk -v`, whose values undergo backslash-escape
# processing.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="$script_dir/check_pkg_names.sh"
allowlist_file="$script_dir/pkg_names_allowlist.txt"

for required in "$guard" "$allowlist_file"; do
	if [ ! -f "$required" ]; then
		printf 'check_pkg_names_selftest.sh: cannot find %s\n' "$required" >&2
		exit 1
	fi
done

tmp_base="${TMPDIR:-${TEMP:-${TMP:-/tmp}}}/check_pkg_names_selftest.$$"
if ! mkdir -p "$tmp_base"; then
	printf 'check_pkg_names_selftest.sh: cannot create %s\n' "$tmp_base" >&2
	exit 1
fi
cleanup() {
	rm -rf -- "$tmp_base" || :
}
trap cleanup EXIT

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

expect_output_contains() {
	desc="$1"
	got="$2"
	want="$3"
	case "$got" in
	*"$want"*)
		printf 'PASS: %s\n' "$desc"
		;;
	*)
		printf 'FAIL: %s -- output does not contain [%s]\n' "$desc" "$want"
		failures=$((failures + 1))
		;;
	esac
}

# new_repo <n>: fresh throwaway git repo with an empty allowlist; its
# path on stdout, or empty on failure.
new_repo() {
	repo="$tmp_base/repo$1"
	rm -rf -- "$repo" || return 1
	if ! mkdir -p "$repo/.githooks"; then
		return 1
	fi
	if ! : >"$repo/.githooks/pkg_names_allowlist.txt"; then
		return 1
	fi
	if ! git -C "$repo" init -q >/dev/null 2>&1; then
		return 1
	fi
	printf '%s' "$repo"
}

# write_pkg <repo> <path> <package>: a one-clause .go file at a
# possibly nested path under <repo>. The path is a shell redirect
# target only -- never an awk -v value (escape processing).
write_pkg() {
	w_path="$1/$2"
	mkdir -p -- "$(dirname "$w_path")" || return 1
	printf 'package %s\n' "$3" >"$w_path"
}

# run_guard <repo>: runs the guard inside <repo>; leaves combined
# output in $out and the exit status in $status.
run_guard() {
	out="$(cd "$1" && bash "$guard" 2>&1)"
	status=$?
}

# Case 1: unique names pass.
repo="$(new_repo 1)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 1: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" alpha
	write_pkg "$repo" "b/y.go" beta
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 1: unique package names pass" 0 "$status"
fi

# Case 2: duplicate names fail, both directories are named.
repo="$(new_repo 2)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 2: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" dup
	write_pkg "$repo" "b/y.go" dup
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 2: duplicate package names fail" 1 "$status"
	expect_output_contains "case 2: names the duplicate" "$out" 'DUPLICATE PACKAGE NAME: "dup" is declared by 2 directories'
	expect_output_contains "case 2: lists directory a" "$out" " a "
	expect_output_contains "case 2: lists directory b" "$out" " b"
fi

# Case 3: main packages do not collide.
repo="$(new_repo 3)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 3: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" main
	write_pkg "$repo" "b/y.go" main
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 3: main packages do not collide" 0 "$status"
fi

# Case 4: a directory whose only .go files are test files contributes
# nothing.
repo="$(new_repo 4)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 4: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" dup
	write_pkg "$repo" "b/y_test.go" dup
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 4: test-file-only directories are ignored" 0 "$status"
fi

# Case 5: testdata is invisible.
repo="$(new_repo 5)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 5: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" dup
	write_pkg "$repo" "a/testdata/z.go" dup
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 5: testdata packages are ignored" 0 "$status"
fi

# Case 6: the ratchet -- an allowlisted name that grows fails; at its
# recorded count it passes.
repo="$(new_repo 6)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 6: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" dup
	write_pkg "$repo" "b/y.go" dup
	printf 'dup 2\n' >"$repo/.githooks/pkg_names_allowlist.txt"
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 6: allowlisted collision at its recorded count passes" 0 "$status"
	write_pkg "$repo" "c/z.go" dup
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 6: allowlisted collision grown by one dir fails" 1 "$status"
	expect_output_contains "case 6: reports the growth against the recorded count" "$out" '"dup" is now declared by 3 directories (allowlist records 2)'
fi

# Case 7: a name back to one declaring directory must be removed from
# the allowlist; keeping the line would silently turn the ratchet into
# an exemption list.
repo="$(new_repo 7)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 7: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_pkg "$repo" "a/x.go" dup
	printf 'dup 2\n' >"$repo/.githooks/pkg_names_allowlist.txt"
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 7: allowlisted name now unique fails" 1 "$status"
	expect_output_contains "case 7: names the cleanup" "$out" 'RATCHET CLEANUP: "dup" is now unique'
	expect_output_contains "case 7: asks for the allowlist line to be removed" "$out" "remove it from"
fi

# Case 8: the real repository passes with its seeded allowlist.
out="$(bash "$guard" 2>&1)"
status=$?
expect_status "case 8: unmocked guard run on the real repository exits 0" 0 "$status"
expect_output_contains "case 8: summary reports the seeded allowlist size" "$out" "3 allowlisted collision(s)"

if [ "$failures" -gt 0 ]; then
	printf 'check_pkg_names_selftest: %d failure(s)\n' "$failures"
	exit 1
fi
printf 'check_pkg_names_selftest: all cases passed\n'
exit 0
