#!/usr/bin/env bash
# Emit every Go package covered by the pre-push remainder segment.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir/.." || exit 1

package_list=""
cleanup() {
	if [ -n "${package_list:-}" ]; then
		rm -f -- "$package_list" || :
	fi
}
trap cleanup EXIT

if ! package_list="$(mktemp)"; then
	printf 'list_test_packages.sh: could not create a temporary package list\n' >&2
	exit 1
fi
if ! go list ./... >"$package_list"; then
	printf 'list_test_packages.sh: go list ./... failed\n' >&2
	exit 1
fi

packages=()
while IFS= read -r pkg; do
	[ -z "$pkg" ] && continue
	[ "$pkg" = "github.com/PHPCraftdream/rush/internal/agent" ] && continue
	packages+=("$pkg")
done <"$package_list"

if [ "${#packages[@]}" -eq 0 ]; then
	printf 'list_test_packages.sh: go list ./... produced no remainder packages\n' >&2
	exit 1
fi
printf '%s\n' "${packages[@]}"
