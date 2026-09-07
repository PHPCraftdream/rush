#!/usr/bin/env bash
# Self-test for the fail-closed package extraction used by pre-push.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
target="$script_dir/list_test_packages.sh"
if [ ! -f "$target" ]; then
	printf 'check_list_test_packages.sh: cannot find %s\n' "$target" >&2
	exit 1
fi

mock_dir=""
cleanup() {
	if [ -n "${mock_dir:-}" ]; then
		rm -rf -- "$mock_dir" || :
	fi
}
trap cleanup EXIT
if ! mock_dir="$(mktemp -d)"; then
	exit 1
fi
cat >"$mock_dir/go" <<'EOF'
#!/usr/bin/env bash
case "${CHECK_LIST_PACKAGES_MODE:-valid}" in
empty) exit 0 ;;
fail) exit 1 ;;
agent-only) printf '%s\n' github.com/PHPCraftdream/rush/internal/agent ;;
valid)
	printf '%s\n' github.com/PHPCraftdream/rush/internal/agent github.com/PHPCraftdream/rush/internal/app
	;;
esac
EOF
chmod +x "$mock_dir/go"

real_path="$mock_dir:$PATH"
failures=0
expect_status() {
	desc="$1"
	want="$2"
	got="$3"
	if [ "$want" -eq "$got" ]; then
		printf 'PASS: %s (exit %s)\n' "$desc" "$got"
	else
		printf 'FAIL: %s -- expected exit %s, got %s\n' "$desc" "$want" "$got"
		failures=$((failures + 1))
	fi
}

CHECK_LIST_PACKAGES_MODE=empty PATH="$real_path" bash "$target" >/dev/null 2>&1
expect_status "empty go list fails closed" 1 "$?"
CHECK_LIST_PACKAGES_MODE=fail PATH="$real_path" bash "$target" >/dev/null 2>&1
expect_status "go list failure fails closed" 1 "$?"
CHECK_LIST_PACKAGES_MODE=agent-only PATH="$real_path" bash "$target" >/dev/null 2>&1
expect_status "agent-only list fails closed" 1 "$?"
valid_output="$(CHECK_LIST_PACKAGES_MODE=valid PATH="$real_path" bash "$target")"
expect_status "valid list succeeds" 0 "$?"
case "$valid_output" in
*"github.com/PHPCraftdream/rush/internal/agent"*)
	printf 'FAIL: internal/agent must be excluded from the remainder list\n'
	failures=$((failures + 1))
	;;
esac
case "$valid_output" in
*"github.com/PHPCraftdream/rush/internal/app"*) printf 'PASS: valid list retains remainder packages\n' ;;
*) printf 'FAIL: valid list lost remainder packages\n'; failures=$((failures + 1)) ;;
esac

if [ "$failures" -gt 0 ]; then
	exit 1
fi
printf 'check_list_test_packages: all cases passed\n'
