#!/usr/bin/env bash
# check_layers_selftest.sh: self-test for .githooks/check_layers.sh
# (the internal layer-direction ratchet, kit < store < session <
# config < agent < app < serve).
#
# Guard cases build throwaway git repos under the system temp
# directory, copy the real guard into them with fresh exception
# lists, and assert:
#   1. a clean module passes;
#   2. a production import of a strictly higher group fails;
#   3. a same-group import passes;
#   4. internal/app/itest is exempt as an importer;
#   5. a test file importing a higher group fails;
#   6. recording the test file in the ratchet list makes it pass;
#   7. a test file growing past its recorded count fails (the
#      ratchet);
#   8. a stale production exception FAILS (stricter than the sibling
#      guards, which only warn);
#   9. a stale test exception FAILS;
#  10. a live production exception passes;
#  11. an unknown internal group is a broken guard input (exit 3);
#  12. the real repository passes with its seeded exception lists.
#
# mktemp -d is deliberately NOT used: this repository's Windows shells
# hand out a virtual /tmp path that child processes cannot resolve, so
# the temp root is built from the standard environment variables, and
# every path below is used in ways (shell redirects, git -C) that
# survive Windows-style separators.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="$script_dir/check_layers.sh"
prodlist="$script_dir/layer_exceptions.txt"
testlist="$script_dir/layer_test_exceptions.txt"

for required in "$guard" "$prodlist" "$testlist"; do
	if [ ! -f "$required" ]; then
		printf 'check_layers_selftest.sh: cannot find %s\n' "$required" >&2
		exit 1
	fi
done

tmp_base="${TMPDIR:-${TEMP:-${TMP:-/tmp}}}/check_layers_selftest.$$"
if ! mkdir -p "$tmp_base"; then
	printf 'check_layers_selftest.sh: cannot create %s\n' "$tmp_base" >&2
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

# new_repo <n>: fresh throwaway git repo with go.mod, the guard copied
# in and fresh empty exception lists; its path on stdout, or empty on
# failure.
new_repo() {
	repo="$tmp_base/repo$1"
	rm -rf -- "$repo" || return 1
	if ! mkdir -p "$repo/.githooks"; then
		return 1
	fi
	if ! : >"$repo/.githooks/layer_exceptions.txt"; then
		return 1
	fi
	if ! : >"$repo/.githooks/layer_test_exceptions.txt"; then
		return 1
	fi
	if ! printf 'module fixture\n\ngo 1.21\n' >"$repo/go.mod"; then
		return 1
	fi
	if ! cp "$guard" "$repo/.githooks/check_layers.sh"; then
		return 1
	fi
	if ! git -C "$repo" init -q >/dev/null 2>&1; then
		return 1
	fi
	printf '%s' "$repo"
}

# put_file <repo> <relpath> <content>: writes a file at <repo>/<relpath>.
put_file() {
	p_dir="${2%/*}"
	[ "$p_dir" = "$2" ] && p_dir=""
	if [ -n "$p_dir" ]; then
		mkdir -p -- "$1/$p_dir" || return 1
	fi
	printf '%s\n' "$3" >"$1/$2"
}

# put_pkg <repo> <dir> <pkgname> [imports...]: one tiny package file.
put_pkg() {
	p_repo="$1"
	p_dir="$2"
	p_name="$3"
	shift 3
	p_body="package $p_name"
	for p_imp in "$@"; do
		p_body="$p_body
import \"$p_imp\""
	done
	put_file "$p_repo" "$p_dir/pkg.go" "$p_body"
}

# run_guard <repo>: runs the guard inside <repo>; leaves combined
# output in $out and the exit status in $status.
run_guard() {
	repo="$1"
	out="$(cd "$repo" && bash .githooks/check_layers.sh 2>&1)"
	status=$?
}

# Case 1: a clean module passes; non-internal importers are exempt.
repo="$(new_repo 1)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 1: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/kit/a a
	put_pkg "$repo" internal/agent/b b fixture/internal/kit/a
	put_pkg "$repo" internal/app/d d
	put_pkg "$repo" internal/serve c fixture/internal/app/d
	put_pkg "$repo" internal/serve/c c
	put_file "$repo" main.go 'package main

import "fixture/internal/serve"

func main() {
	_ = serve.Pkg
}'
	run_guard "$repo"
	expect_status "case 1: a clean module passes" 0 "$status"
fi

# Case 2: a production import of a strictly higher group fails.
repo="$(new_repo 2)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 2: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/kit/a a fixture/internal/agent/b
	run_guard "$repo"
	expect_status "case 2: kit importing agent fails" 1 "$status"
	expect_output_contains "case 2: names the importer" "$out" "LAYER VIOLATION: fixture/internal/kit/a"
	expect_output_contains "case 2: names the imported package" "$out" "imports fixture/internal/agent/b"
fi

# Case 3: a same-group import passes.
repo="$(new_repo 3)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 3: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/kit/a a
	put_pkg "$repo" internal/kit/a2 a2 fixture/internal/kit/a
	run_guard "$repo"
	expect_status "case 3: a same-group import passes" 0 "$status"
fi

# Case 4: internal/app/itest is exempt as an importer.
repo="$(new_repo 4)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 4: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/serve/c c
	put_pkg "$repo" internal/app/itest itest fixture/internal/serve/c
	run_guard "$repo"
	expect_status "case 4: internal/app/itest may import anything" 0 "$status"
fi

# Case 5: a test file importing a higher group fails.
repo="$(new_repo 5)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 5: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/serve/c c
	put_pkg "$repo" internal/kit a
	put_file "$repo" internal/kit/a_test.go 'package a

import "fixture/internal/agent/b"

var _ = b.Pkg'
	run_guard "$repo"
	expect_status "case 5: a test file importing a higher group fails" 1 "$status"
	expect_output_contains "case 5: reports the test violation" "$out" "TEST LAYER VIOLATION"
	expect_output_contains "case 5: names the test file" "$out" "internal/kit/a_test.go"
fi

# Case 6: recording the same file in the test ratchet list passes.
repo="$(new_repo 6)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 6: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/serve/c c
	put_pkg "$repo" internal/kit a
	put_file "$repo" internal/kit/a_test.go 'package a

import "fixture/internal/agent/b"

var _ = b.Pkg'
	printf 'internal/kit/a_test.go 1\n' >"$repo/.githooks/layer_test_exceptions.txt"
	run_guard "$repo"
	expect_status "case 6: a recorded test violation at its count passes" 0 "$status"
fi

# Case 7: the same test file growing past its recorded count fails.
repo="$(new_repo 7)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 7: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/serve/c c
	put_pkg "$repo" internal/kit a
	put_file "$repo" internal/kit/a_test.go 'package a

import (
	"fixture/internal/agent/b"
	"fixture/internal/serve/c"
)

var _ = b.Pkg'
	printf 'internal/kit/a_test.go 1\n' >"$repo/.githooks/layer_test_exceptions.txt"
	run_guard "$repo"
	expect_status "case 7: a test file grown past its recorded count fails" 1 "$status"
	expect_output_contains "case 7: reports the ratchet violation" "$out" "RATCHET VIOLATION: internal/kit/a_test.go now violates with 2 import(s) (recorded 1)"
fi

# Case 8: a stale production exception fails (the deliberate
# difference from the sibling guards, which only warn).
repo="$(new_repo 8)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 8: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/kit/a a
	printf 'fixture/internal/kit/a 1\n' >"$repo/.githooks/layer_exceptions.txt"
	run_guard "$repo"
	expect_status "case 8: a stale production exception fails" 1 "$status"
	expect_output_contains "case 8: asks for the stale entry to be removed" "$out" "remove it from"
fi

# Case 9: a stale test exception fails.
repo="$(new_repo 9)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 9: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/kit/a a
	printf 'internal/kit/a/gone_test.go 1\n' >"$repo/.githooks/layer_test_exceptions.txt"
	run_guard "$repo"
	expect_status "case 9: a stale test exception fails" 1 "$status"
	expect_output_contains "case 9: reports the stale exception" "$out" "STALE EXCEPTION: internal/kit/a/gone_test.go"
fi

# Case 10: a live production exception at its recorded count passes.
repo="$(new_repo 10)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 10: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/agent/b b
	put_pkg "$repo" internal/kit/a a fixture/internal/agent/b
	printf 'fixture/internal/kit/a 1\n' >"$repo/.githooks/layer_exceptions.txt"
	run_guard "$repo"
	expect_status "case 10: a live production exception passes" 0 "$status"
	expect_output_contains "case 10: the summary reports the kept edge" "$out" "1 production edge(s) allowlisted (kept)"
fi

# Case 11: an unknown internal group is a broken guard input.
repo="$(new_repo 11)"
if [ -z "$repo" ]; then
	printf 'FAIL: case 11: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	put_pkg "$repo" internal/foo/x x
	run_guard "$repo"
	expect_status "case 11: an unknown group is a broken guard input" 3 "$status"
	expect_output_contains "case 11: names the offending package" "$out" "fixture/internal/foo/x"
fi

# Case 12: the real repository passes with its seeded exception lists.
out="$(bash "$guard" 2>&1)"
status=$?
expect_status "case 12: unmocked guard run on the real repository exits 0" 0 "$status"
expect_output_contains "case 12: the summary shows the layer chain" "$out" "kit < store < session < config < agent < app < serve"

if [ "$failures" -gt 0 ]; then
	printf 'check_layers_selftest: %d failure(s)\n' "$failures"
	exit 1
fi
printf 'check_layers_selftest: all cases passed\n'
exit 0
