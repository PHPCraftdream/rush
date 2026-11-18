#!/usr/bin/env bash
# check_dir_files_selftest.sh: self-test for
# .githooks/check_dir_files.sh (the 21-code-files-per-directory
# ratchet, tests included).
#
# Guard cases build throwaway git repos under the system temp directory
# and run the real guard inside them, asserting:
#   1. a clean code directory passes;
#   2. a 22-file directory fails with TOO MANY FILES;
#   3. a directory with exactly 21 files passes;
#   4. test files ALWAYS count (the deliberate difference from
#      check_dir_entries.sh, where tests are excluded by default);
#   5. generated files are skipped;
#   6. testdata is invisible to the guard;
#   7. an allowlisted directory that grows fails (the ratchet);
#   8. an allowlisted directory at/below the limit fails with the
#      "remove it from" message (the list must shrink);
#   9. the repository root is exempt from the rule;
#  10. non-Go extensions (.ts, .tsx) count too;
#  11. the real repository passes with its seeded allowlist.
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
guard="$script_dir/check_dir_files.sh"
allowlist_file="$script_dir/dir_files_allowlist.txt"

for required in "$guard" "$allowlist_file"; do
	if [ ! -f "$required" ]; then
		printf 'check_dir_files_selftest.sh: cannot find %s\n' "$required" >&2
		exit 1
	fi
done

tmp_base="${TMPDIR:-${TEMP:-${TMP:-/tmp}}}/check_dir_files_selftest.$$"
if ! mkdir -p "$tmp_base"; then
	printf 'check_dir_files_selftest.sh: cannot create %s\n' "$tmp_base" >&2
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

expect_output_lacks() {
	desc="$1"
	got="$2"
	want="$3"
	case "$got" in
	*"$want"*)
		printf 'FAIL: %s -- output contains [%s]\n' "$desc" "$want"
		failures=$((failures + 1))
		;;
	*)
		printf 'PASS: %s\n' "$desc"
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
	if ! : >"$repo/.githooks/dir_files_allowlist.txt"; then
		return 1
	fi
	if ! git -C "$repo" init -q >/dev/null 2>&1; then
		return 1
	fi
	printf '%s' "$repo"
}

# write_filler_at <repo> <path> <lines> [first-line]: a file at a
# possibly nested path under <repo> with exactly <lines> lines,
# optionally starting with a marker line. The path is a shell redirect
# target only -- never an awk -v value (escape processing).
write_filler_at() {
	r_dir="$1"
	r_path="$r_dir/$2"
	mkdir -p -- "$(dirname "$r_path")" || return 1
	awk -v lines="$3" -v first="${4:-}" 'BEGIN {
		if (first != "") {
			print first
			lines--
		}
		for (i = 1; i <= lines; i++) {
			print "filler line " i
		}
	}' >"$r_path"
}

# write_filler_set <repo> <dir> <base> <ext> <count>: <count> filler
# files named <base><i><ext> under <repo>/<dir>, written by ONE awk
# process (a per-file spawn loop is minutes on Windows).
write_filler_set() {
	s_repo="$1"
	s_dir="$2"
	s_base="$3"
	s_ext="$4"
	s_count="$5"
	if ! mkdir -p -- "$s_repo/$s_dir"; then
		return 1
	fi
	(
		cd "$s_repo/$s_dir" || exit 1
		awk -v count="$s_count" -v base="$s_base" -v ext="$s_ext" 'BEGIN {
			for (i = 1; i <= count; i++) {
				print "filler line " i > (base i ext)
			}
		}'
	) || return 1
}

# run_guard <repo> [extra args...]: runs the guard inside <repo>;
# leaves combined output in $out and the exit status in $status.
run_guard() {
	repo="$1"
	shift
	out="$(cd "$repo" && bash "$guard" "$@" 2>&1)"
	status=$?
}

# Case 1: a clean code directory passes.
repo="$(new_repo 1)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 1: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 3
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 1: a clean code directory passes" 0 "$status"
fi

# Case 2: a 22-file directory fails.
repo="$(new_repo 2)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 2: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 22
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 2: a 22-file code directory fails" 1 "$status"
	expect_output_contains "case 2: names the directory and its count" "$out" "TOO MANY FILES: src has 22 code files"
fi

# Case 3: a directory with exactly 21 files passes.
repo="$(new_repo 3)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 3: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 21
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 3: a directory with exactly 21 files passes" 0 "$status"
fi

# Case 4: test files ALWAYS count (unlike check_dir_entries.sh, where
# tests are excluded by default): 7 non-test plus 15 test files = 22
# fails, and the message reports 22.
repo="$(new_repo 4)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 4: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 7
	write_filler_set "$repo" src f _test.go 15
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 4: test files always count" 1 "$status"
	expect_output_contains "case 4: reports the test-inclusive count" "$out" "src has 22 code files"
fi

# Case 5: generated files are skipped.
repo="$(new_repo 5)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 5: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 21
	write_filler_at "$repo" "src/gen.go" 3 "// Code generated by tool. DO NOT EDIT."
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 5: a generated file is skipped" 0 "$status"
	expect_output_contains "case 5: reports the generated skip" "$out" "generated skipped"
fi

# Case 6: testdata is invisible -- 3 files in src plus 22 files under
# src/testdata passes, and src/testdata is never reported.
repo="$(new_repo 6)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 6: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .go 3
	write_filler_set "$repo" src/testdata t .go 22
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 6: testdata content is invisible" 0 "$status"
	expect_output_lacks "case 6: testdata is never reported" "$out" "TOO MANY FILES: src/testdata"
fi

# Case 7: the ratchet -- an allowlisted directory that grows fails; at
# its recorded count it passes.
repo="$(new_repo 7)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 7: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" big f .go 25
	printf 'big 25\n' >"$repo/.githooks/dir_files_allowlist.txt"
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 7: allowlisted directory at its recorded count passes" 0 "$status"
	write_filler_at "$repo" "big/f26.go" 3
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 7: allowlisted directory grown by one file fails" 1 "$status"
	expect_output_contains "case 7: reports the growth against the recorded count" "$out" "RATCHET VIOLATION: big grew to 26 code files (allowlist records 25)"
fi

# Case 8: a directory back at/below the limit must be removed from the
# allowlist; keeping the line would silently turn the ratchet into an
# exemption list.
repo="$(new_repo 8)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 8: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" big f .go 20
	printf 'big 25\n' >"$repo/.githooks/dir_files_allowlist.txt"
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 8: allowlisted directory now under the limit fails" 1 "$status"
	expect_output_contains "case 8: names the directory and its new count" "$out" "RATCHET CLEANUP: big is now 20 code files"
	expect_output_contains "case 8: asks for the allowlist line to be removed" "$out" "remove it from"
fi

# Case 9: the repository root is exempt from the rule entirely.
repo="$(new_repo 9)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 9: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" . root .go 30
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 9: root-level files are exempt" 0 "$status"
fi

# Case 10: non-Go extensions count -- 21 .ts plus 1 .tsx fails.
repo="$(new_repo 10)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 10: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_filler_set "$repo" src f .ts 21
	write_filler_at "$repo" "src/f22.tsx" 3
	(cd "$repo" && git add -A >/dev/null 2>&1)
	run_guard "$repo"
	expect_status "case 10: 21 .ts plus 1 .tsx fails" 1 "$status"
	expect_output_contains "case 10: reports the combined count" "$out" "src has 22 code files"
fi

# Case 11: the real repository passes with its seeded allowlist.
out="$(bash "$guard" 2>&1)"
status=$?
expect_status "case 11: unmocked guard run on the real repository exits 0" 0 "$status"
expect_output_contains "case 11: summary reports the seeded allowlist size" "$out" "22 allowlisted over 21"

if [ "$failures" -gt 0 ]; then
	printf 'check_dir_files_selftest: %d failure(s)\n' "$failures"
	exit 1
fi
printf 'check_dir_files_selftest: all cases passed\n'
exit 0
