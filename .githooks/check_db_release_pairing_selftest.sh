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
# Mechanism: a mock `git` executable is prepended to PATH. On `grep`
# it exits with a forced status while delegating every other
# subcommand (rev-parse, and anything else the guard uses) to the real
# git, so the guard runs for real until the candidate listing.
set -uo pipefail

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

mock_dir="$(mktemp -d)"
trap 'rm -rf "$mock_dir"' EXIT

cat >"$mock_dir/git" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = "grep" ]; then
	if [ "${CHECK_DB_SELFTEST_GREP_EXIT:-1}" -ne 1 ]; then
		printf 'check_db_release_pairing_selftest: forced git grep exit %s\n' "$CHECK_DB_SELFTEST_GREP_EXIT" >&2
	fi
	exit "$CHECK_DB_SELFTEST_GREP_EXIT"
fi
exec "$CHECK_DB_SELFTEST_REAL_GIT" "$@"
EOF
chmod +x "$mock_dir/git"

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
err="$(CHECK_DB_SELFTEST_GREP_EXIT=128 CHECK_DB_SELFTEST_REAL_GIT="$real_git" PATH="$mock_dir:$PATH" bash "$target" 2>&1 >/dev/null)"
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
CHECK_DB_SELFTEST_GREP_EXIT=1 CHECK_DB_SELFTEST_REAL_GIT="$real_git" PATH="$mock_dir:$PATH" bash "$target" >/dev/null 2>&1
expect_status "git grep exit 1 (zero matches) stays a pass (exit 0)" 0 "$?"

if [ "$failures" -gt 0 ]; then
	printf 'check_db_release_pairing self-test: %d failure(s)\n' "$failures"
	exit 1
fi
printf 'check_db_release_pairing self-test: all cases passed\n'
exit 0
