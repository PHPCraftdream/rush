#!/usr/bin/env bash
# check_frozen_modules_selftest.sh: self-test for
# .githooks/check_frozen_modules.sh (the frozen-module guard).
#
# Each case builds a throwaway git repository under the system temp
# directory (never the real repository, never its history, no network),
# writes a minimal docs/modules.md registry fixture, creates a base
# commit, a branch with test commits, and a plain refs/heads/origin/main
# ref at the base, then runs the real guard inside the throwaway repo,
# asserting:
#   a. a change under a frozen module without an Unfreeze line fails;
#   b. the same change with `Unfreeze: <module> - reason` passes, in
#      both the hyphen and the em dash form;
#   c. a change under a stable module exits 0 with a warning;
#   d. an unknown status in the registry fails loudly;
#   e. a malformed registry row fails loudly;
#   f. a missing docs/modules.md fails with a clear message;
#   g. a nested path attributes to the LONGEST matching prefix
#      (internal/store stable, internal/store/db frozen).
#
# mktemp -d is used with an explicit directory template built from the
# standard environment variables: this repository's Windows shells hand
# out a virtual /tmp path that child processes cannot resolve. No
# Windows path is ever passed through `awk -v` (no awk is used here at
# all). Deliberately NOT set -e: see check_stale_model_slots.sh.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
guard="$script_dir/check_frozen_modules.sh"

if [ ! -f "$guard" ]; then
	printf 'check_frozen_modules_selftest.sh: cannot find %s\n' "$guard" >&2
	exit 1
fi

tmp_base="${TMPDIR:-${TEMP:-${TMP:-/tmp}}}"
tmp_root="$(mktemp -d "$tmp_base/check_frozen_selftest.XXXXXX")" || {
	printf 'check_frozen_modules_selftest.sh: mktemp -d failed\n' >&2
	exit 1
}
cleanup() {
	rm -rf -- "$tmp_root" || :
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

# write_registry <repo> [<extra rows>...]: the standard fixture
# registry (one frozen, one stable, one active row) plus any extra
# rows given, appended verbatim.
write_registry() {
	w_repo="$1"
	shift
	mkdir -p "$w_repo/docs" || return 1
	{
		printf '| module | status | purpose | public API | layer |\n'
		printf '|---|---|---|---|---|\n'
		printf '`internal/frozenmod` | frozen | untouchable core | none | core\n'
		printf '`internal/stablemod` | stable | settling down | yes | mid\n'
		printf '`internal/activemod` | active | under construction | yes | mid\n'
		for row in "$@"; do
			printf '%s\n' "$row"
		done
	} >"$w_repo/docs/modules.md"
}

# new_repo <n>: fresh throwaway git repo with a base commit on main,
# a test branch, and refs/heads/origin/main at the base; echoes the
# repo path and sets $repo_base / $repo_head globally.
new_repo() {
	repo="$tmp_root/repo$1"
	rm -rf -- "$repo" || return 1
	mkdir -p "$repo" || return 1
	git -C "$repo" init -q -b main >/dev/null 2>&1 || return 1
	git -C "$repo" config user.email selftest@example.invalid >/dev/null 2>&1 || return 1
	git -C "$repo" config user.name selftest >/dev/null 2>&1 || return 1
	mkdir -p "$repo/internal/base" || return 1
	printf 'package base\n' >"$repo/internal/base/base.go"
	git -C "$repo" add -A >/dev/null 2>&1 || return 1
	git -C "$repo" commit -q -m "base commit" >/dev/null 2>&1 || return 1
	repo_base="$(git -C "$repo" rev-parse HEAD)" || return 1
	git -C "$repo" update-ref refs/heads/origin/main "$repo_base" >/dev/null 2>&1 || return 1
	git -C "$repo" checkout -q -b feature >/dev/null 2>&1 || return 1
	printf '%s' "$repo"
}

# add_commit <repo> <file> <message>: touch <file> and commit it.
add_commit() {
	a_path="$1/$2"
	mkdir -p -- "$(dirname "$a_path")" || return 1
	printf 'package x\n' >"$a_path"
	git -C "$1" add -A >/dev/null 2>&1 || return 1
	git -C "$1" commit -q -m "$3" >/dev/null 2>&1 || return 1
	repo_head="$(git -C "$1" rev-parse HEAD)" || return 1
}

# run_guard <repo> [args...]: runs the guard inside <repo>; leaves
# combined output in $out and the exit status in $status.
run_guard() {
	r_dir="$1"
	shift
	if [ $# -gt 0 ]; then
		out="$(cd "$r_dir" && bash "$guard" "$@" 2>&1)"
	else
		out="$(cd "$r_dir" && bash "$guard" 2>&1)"
	fi
	status=$?
}

# Case a: frozen module change WITHOUT an Unfreeze line fails.
repo="$(new_repo a)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case a: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_registry "$repo" || printf 'FAIL: case a: cannot write registry\n'
	add_commit "$repo" "internal/frozenmod/a.go" "add frozen file" || printf 'FAIL: case a: commit failed\n'
	run_guard "$repo"
	expect_status "case a: frozen change without Unfreeze line fails" 1 "$status"
	expect_output_contains "case a: names the owning module" "$out" "internal/frozenmod"
	expect_output_contains "case a: lists the offending file" "$out" "internal/frozenmod/a.go"
fi

# Case b: frozen change WITH the Unfreeze line passes; hyphen form and
# em dash form both accepted; the default merge-base base is exercised.
repo="$(new_repo b)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case b: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	repo_base="$(git -C "$repo" rev-parse refs/heads/origin/main)" || repo_base=""
	write_registry "$repo" || printf 'FAIL: case b: cannot write registry\n'
	add_commit "$repo" "internal/frozenmod/b.go" "add frozen file

Unfreeze: internal/frozenmod - hotfix leak in reader" || printf 'FAIL: case b: commit failed\n'
	run_guard "$repo"
	expect_status "case b: hyphen Unfreeze line passes (default merge-base base)" 0 "$status"
	expect_output_contains "case b: reports the covered frozen file" "$out" "1 frozen file(s) covered"
	add_commit "$repo" "internal/frozenmod/c.go" "add another frozen file

Unfreeze: internal/frozenmod — deliberate protocol change" || printf 'FAIL: case b: em dash commit failed\n'
	run_guard "$repo" "$repo_base"
	expect_status "case b: em dash Unfreeze line passes (explicit base)" 0 "$status"
fi

# Case c: stable module change exits 0 and warns on stderr.
repo="$(new_repo c)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case c: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_registry "$repo" || printf 'FAIL: case c: cannot write registry\n'
	add_commit "$repo" "internal/stablemod/c.go" "add stable file" || printf 'FAIL: case c: commit failed\n'
	run_guard "$repo"
	expect_status "case c: stable change exits 0" 0 "$status"
	expect_output_contains "case c: warns about the stable file" "$out" "warning: changed file internal/stablemod/c.go is under stable module internal/stablemod"
fi

# Case d: unknown status in the registry fails loudly.
repo="$(new_repo d)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case d: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_registry "$repo" '`internal/weirdmod` | thawed | bogus status row | no | top' || printf 'FAIL: case d: cannot write registry\n'
	add_commit "$repo" "internal/weirdmod/d.go" "add weird file" || printf 'FAIL: case d: commit failed\n'
	run_guard "$repo"
	expect_status "case d: unknown status fails" 1 "$status"
	expect_output_contains "case d: reports the invalid status" "$out" 'invalid status "thawed"'
	expect_output_contains "case d: echoes the offending line" "$out" '`internal/weirdmod` | thawed | bogus status row | no | top'
fi

# Case e: malformed registry row fails loudly.
repo="$(new_repo e)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case e: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_registry "$repo" '`internal/brokenmod` | frozen | missing cells row' || printf 'FAIL: case e: cannot write registry\n'
	add_commit "$repo" "internal/brokenmod/e.go" "add broken file" || printf 'FAIL: case e: commit failed\n'
	run_guard "$repo"
	expect_status "case e: malformed registry row fails" 1 "$status"
	expect_output_contains "case e: reports the malformed row" "$out" "malformed registry row"
fi

# Case f: missing docs/modules.md fails with a clear message.
repo="$(new_repo f)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case f: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	add_commit "$repo" "internal/frozenmod/f.go" "add file, no registry" || printf 'FAIL: case f: commit failed\n'
	run_guard "$repo"
	expect_status "case f: missing registry fails" 1 "$status"
	expect_output_contains "case f: clear message about the missing registry" "$out" "missing docs/modules.md"
fi

# Case g: longest matching prefix wins (internal/store stable,
# internal/store/db frozen).
repo="$(new_repo g)" || repo=""
if [ -z "$repo" ]; then
	printf 'FAIL: case g: could not create the throwaway repo\n'
	failures=$((failures + 1))
else
	write_registry "$repo" '`internal/store` | stable | storage layer | yes | mid' '`internal/store/db` | frozen | the database core | no | core' || printf 'FAIL: case g: cannot write registry\n'
	# Without an Unfreeze line for internal/store/db: fail, and the
	# owner must be the LONGER prefix, not internal/store.
	add_commit "$repo" "internal/store/db/x.go" "add db file" || printf 'FAIL: case g: commit failed\n'
	run_guard "$repo"
	expect_status "case g: nested frozen path fails without Unfreeze" 1 "$status"
	expect_output_contains "case g: owner is the longest prefix" "$out" "FROZEN module internal/store/db"
	# With the Unfreeze line naming internal/store/db: pass even though
	# internal/store (stable) also matches the path.
	add_commit "$repo" "internal/store/db/y.go" "add db file with unfreeze

Unfreeze: internal/store/db - storage engine migration" || printf 'FAIL: case g: commit failed\n'
	run_guard "$repo"
	expect_status "case g: nested frozen path passes with matching Unfreeze" 0 "$status"
	# An Unfreeze line naming the SHORTER prefix must NOT cover a file
	# owned by the longer one; a fresh repo keeps earlier commits'
	# valid Unfreeze lines out of the range.
	repo="$(new_repo g2)" || repo=""
	if [ -z "$repo" ]; then
		printf 'FAIL: case g2: could not create the throwaway repo\n'
		failures=$((failures + 1))
	else
		write_registry "$repo" '`internal/store` | stable | storage layer | yes | mid' '`internal/store/db` | frozen | the database core | no | core' || printf 'FAIL: case g2: cannot write registry\n'
		add_commit "$repo" "internal/store/db/z.go" "add db file with wrong unfreeze

Unfreeze: internal/store - vague reason" || printf 'FAIL: case g2: commit failed\n'
		run_guard "$repo"
		expect_status "case g: Unfreeze naming the shorter prefix does not cover the file" 1 "$status"
		expect_output_contains "case g: still names the real owner" "$out" "internal/store/db"
	fi
fi

if [ "$failures" -gt 0 ]; then
	printf 'check_frozen_modules_selftest: %d failure(s)\n' "$failures"
	printf 'SELTEST-RC=1\n'
	exit 1
fi
printf 'check_frozen_modules_selftest: all cases passed\n'
printf 'SELTEST-RC=0\n'
exit 0
