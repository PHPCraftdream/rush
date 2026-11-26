#!/usr/bin/env bash
# check_db_release_pairing_selftest.sh: self-test for the git grep
# failure handling in check_db_release_pairing.sh.
#
# The guard's candidate listing must tell apart "git grep found zero
# matches" (exit exactly 1 -- the legitimate empty case: empty
# candidate list, no-op loop, still a pass) from "git grep itself
# failed" (exit > 1 -- broken repository, mangled paths in mixed
# Windows/WSL setups, etc.). The latter must fail closed with a
# message on stderr instead of masquerading as an empty result and
# reporting a false pass.
#
# Mechanism: a mock `git` (a shell FUNCTION injected into the guard
# subprocess via BASH_ENV -- see "Hermeticity" below) answers `git grep`
# with a forced status while delegating every other subcommand
# (rev-parse, and anything else the guard uses) to the real git, so the
# guard runs for real until the candidate listing.
#
# Hermeticity (why BASH_ENV instead of prepending a mock dir to PATH,
# and why the GIT_* unsets): the fixture mock must work identically in
# (a) a normal checkout, (b) a LINKED git worktree (`.git` is a FILE
# pointing at the real gitdir), and (c) shells whose temp-dir mapping
# is non-POSIX (e.g. on Windows, where `mktemp -d` can return a path
# like `/tmp\NNNN` that the MSYS root maps to `<cwd>/tmp/NNNN` -- and
# PATH lookup may refuse extensionless executables entirely). A
# PATH-prepended mock script therefore silently disappears in those
# environments: the real `git grep` runs instead of the forced-exit
# stub, hard-failure and unpaired-fixture cases exit 0 and the
# selftest reports a false pass of its own. A function defined in a
# BASH_ENV file shadows the PATH lookup in every bash, everywhere.
# Additionally, a linked worktree session can inherit GIT_DIR /
# GIT_WORK_TREE / GIT_COMMON_DIR / GIT_INDEX_FILE into the guard
# subprocess, where the guard's `cd "$(git rev-parse --show-toplevel)"`
# (check_db_release_pairing.sh:89) would then resolve against the
# wrong tree; the fixture repository and guard runs must see none of
# those, so they are unset once here and the guard inherits the clean
# environment.
set -uo pipefail

# Hermetic git environment: never let a linked-worktree session (or any
# parent harness) leak repo-selection into the fixtures or the guard.
# Without this, `git rev-parse --show-toplevel` inside the guard could
# resolve to the wrong tree (see check_db_release_pairing.sh:89).
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE \
      GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES 2>/dev/null || true

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
target="$script_dir/check_db_release_pairing.sh"
if [ ! -f "$target" ]; then
	printf 'check_db_release_pairing_selftest.sh: cannot find %s\n' "$target" >&2
	exit 1
fi

real_git="$(command -v git)"
if [ -z "$real_git" ]; then
	printf 'check_db_release_pairing_selftest.sh: git not found on PATH\n' >&2
	exit 1
fi

# The guard cds to the repository toplevel itself; it only needs to be
# started from inside the work tree.
cd "$script_dir/.." || exit 1

# Throwaway scratch dir, derived from the repo checkout rather than
# `mktemp -d`: mktemp output is not portable across shells/hosts (on
# Windows it can yield `/tmp\\NNNN`, which resolves as `<cwd>/tmp/NNNN`
# inside the MSYS root -- and the child guard bash, which starts in the
# same cwd, cannot always re-read that path when BASH_ENV points into
# it). An absolute path under $script_dir/.. is usable both as a
# directory and as a BASH_ENV file everywhere; the EXIT trap removes it.
mock_dir="$script_dir/.check_db_release_pairing_selftest.$$"
rm -rf "$mock_dir"
mkdir -p "$mock_dir" || {
	printf 'check_db_release_pairing_selftest.sh: cannot create scratch dir %s\n' "$mock_dir" >&2
	exit 1
}
trap 'rm -rf "$mock_dir"' EXIT

# The mock `git` is a function sourced into the guard's bash via
# BASH_ENV, not a PATH-prepended executable: PATH lookup of an
# extensionless script is refused on Windows and the mktemp dir may not
# resolve there at all, which made the real git answer `git grep` and
# every mocked case silently pass (see the Hermeticity comment above).
cat >"$mock_dir/git_mock.env" <<'EOF'
git() {
	if [ "${1:-}" = "grep" ]; then
		if [ -n "${CHECK_DB_SELFTEST_FIXTURE:-}" ]; then
			printf '%s\n' "$CHECK_DB_SELFTEST_FIXTURE"
			exit 0
		fi
		if [ "${CHECK_DB_SELFTEST_GREP_EXIT:-1}" -ne 1 ]; then
			printf 'check_db_release_pairing_selftest: forced git grep exit %s\n' "$CHECK_DB_SELFTEST_GREP_EXIT" >&2
		fi
		exit "$CHECK_DB_SELFTEST_GREP_EXIT"
	fi
	# No `exec` on purpose: delegating as a plain command keeps the
	# guard's process intact across shells whose `exec`-in-function
	# semantics differ (replacing the interpreter mid-script silently
	# truncates the guard under some bash emulations).
	"$CHECK_DB_SELFTEST_REAL_GIT" "$@"
}
EOF

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

# Case 1: unmocked run against the real repository must stay a pass.
bash "$target" >/dev/null 2>&1
expect_status "unmocked guard run on the real repository exits 0" 0 "$?"

# Case 2: git grep failing hard (exit 128) must fail closed with a
# message on stderr, never exit 0.
err="$(CHECK_DB_SELFTEST_GREP_EXIT=128 CHECK_DB_SELFTEST_REAL_GIT="$real_git" BASH_ENV="$mock_dir/git_mock.env" bash "$target" 2>&1 >/dev/null)"
status=$?
expect_status "git grep hard failure (exit 128) makes the guard exit 1" 1 "$status"
case "$err" in
*"git grep failed"*)
	printf 'PASS: hard failure reports "git grep failed" on stderr\n'
	;;
*)
	printf 'FAIL: expected "git grep failed" on stderr, got: %s\n' "$err"
	failures=$((failures + 1))
	;;
esac

# Case 3: git grep exit 1 (zero matches, the legitimate empty case)
# must remain a pass, not be treated as an error.
CHECK_DB_SELFTEST_GREP_EXIT=1 CHECK_DB_SELFTEST_REAL_GIT="$real_git" BASH_ENV="$mock_dir/git_mock.env" bash "$target" >/dev/null 2>&1
expect_status "git grep exit 1 (zero matches) stays a pass (exit 0)" 0 "$?"

fixture="$mock_dir/pair_test.go"
printf '%s\n' 'package p' 'func f() { db.Connect(ctx, "fixture"); db.ReleaseConn(conn) }' >"$fixture"
CHECK_DB_SELFTEST_FIXTURE="$fixture" CHECK_DB_SELFTEST_REAL_GIT="$real_git" BASH_ENV="$mock_dir/git_mock.env" bash "$target" >/dev/null 2>&1
expect_status "ReleaseConn pairs one Connect" 0 "$?"

printf '%s\n' 'package p' 'func f() { db.Connect(ctx, "fixture"); db.ConnectRead(ctx, "fixture"); db.ReleaseConn(conn) }' >"$fixture"
CHECK_DB_SELFTEST_FIXTURE="$fixture" CHECK_DB_SELFTEST_REAL_GIT="$real_git" BASH_ENV="$mock_dir/git_mock.env" bash "$target" >/dev/null 2>&1
expect_status "one ReleaseConn cannot pair two connections" 1 "$?"

printf '%s\n' 'package p' 'func f() { db.Connect(ctx, "fixture") }' >"$fixture"
CHECK_DB_SELFTEST_FIXTURE="$fixture" CHECK_DB_SELFTEST_REAL_GIT="$real_git" BASH_ENV="$mock_dir/git_mock.env" bash "$target" >/dev/null 2>&1
expect_status "an unpaired connection still fails" 1 "$?"

if [ "$failures" -gt 0 ]; then
	printf 'check_db_release_pairing self-test: %d failure(s)\n' "$failures"
	exit 1
fi
printf 'check_db_release_pairing self-test: all cases passed\n'
exit 0
