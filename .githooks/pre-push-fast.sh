#!/usr/bin/env bash
# Reduced working-tree gate; see pre-push's header for dispatch and limits.
# Use LF and Git Bash; reduced PATH needs powershell.exe and cygpath on Windows.
# Formatting inspects only changed existing Go files from the pushed-range union.
# Missing go fails; missing golangci-lint loudly skips formatting (not full lint).
# IDE/--no-verify can bypass hooks; CI is the real gate.
# Checks inspect the working tree, not the pushed SHA; shared hooksPath must
# stay relative (.githooks) for worktree-local hooks.
# run_capped self-test is omitted except when hook/config files differ.
set -uo pipefail
HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HOOK_DIR/pre-push-budgets.sh"
cd "$(git rev-parse --show-toplevel)" || exit 1

fail() { printf '\n\033[31m✗ pre-push: %s\033[0m\n' "$1" >&2; exit 1; }
step() { printf '\033[36m▸ %s\033[0m\n' "$1"; }

# Same wrapper and budgets as full mode (hard caps on Windows only).
RUN_CAPPED="$HOOK_DIR/run_capped.ps1"
run_capped() {
	local size="$1" timeout="$2"
	shift 2
	if [ "$(go env GOHOSTOS)" = "windows" ]; then
		local wrapper="$RUN_CAPPED"
		if command -v cygpath >/dev/null 2>&1; then
			wrapper="$(cygpath -w "$wrapper")"
		fi
		powershell -NoProfile -ExecutionPolicy Bypass -File "$wrapper" "$size" "$timeout" "$@"
	else
		"$@"
	fi
}

command -v go >/dev/null 2>&1 || fail "go is missing; refusing a false FAST pass"
changed_files=""
ref_lines=0
add_range() {
	local base="$1" tip="$2" files
	files="$(git diff --name-only --no-renames "$base" "$tip")" \
		|| fail "cannot diff push range; fetch, or RUSH_PREPUSH_FULL=1"
	changed_files+="$files"$'\n'
}
if [ ! -t 0 ]; then
	while read -r lref lsha rref rsha; do
		[ -z "$lref$lsha$rref$rsha" ] && continue
		ref_lines=$((ref_lines + 1))
		[ -n "$lref" ] && [ -n "$lsha" ] && [ -n "$rref" ] && [ -n "$rsha" ] \
			|| fail "invalid push ref line; fetch, or RUSH_PREPUSH_FULL=1"
		# Deletions have no local commit to inspect.
		[ -z "${lsha//0/}" ] && continue
		if git cat-file -e "$rsha^{commit}" 2>/dev/null; then
			base="$rsha"
		else
			[ $# -ge 1 ] || fail "missing push remote; fetch, or RUSH_PREPUSH_FULL=1"
			base="$(git merge-base "$lsha" "refs/remotes/$1/main")" \
				|| fail "push base unavailable; fetch, or RUSH_PREPUSH_FULL=1"
			[ -n "$base" ] || fail "push base unavailable; fetch, or RUSH_PREPUSH_FULL=1"
		fi
		add_range "$base" "$lsha"
	done
fi
if [ "$ref_lines" -eq 0 ]; then
	base="$(git merge-base HEAD origin/main)" \
		|| fail "manual base unavailable; fetch, or RUSH_PREPUSH_FULL=1"
	[ -n "$base" ] || fail "manual base unavailable; fetch, or RUSH_PREPUSH_FULL=1"
	add_range "$base" HEAD
fi
changed_files="$(printf '%s' "$changed_files" | sort -u)" || fail "cannot union changed files"

# Package selection follows the pushed ranges; checks use current files.
dirs=""
hook_checks=0
changed_go_files=()
while IFS= read -r file; do
	case "$file" in .githooks/*|.golangci.yml) hook_checks=1 ;; esac
	case "$file" in
	*.go)
		[ ! -f "$file" ] || changed_go_files+=("$file")
		dir="$(dirname "$file")"
		[ ! -d "$dir" ] || dirs+="./$dir"$'\n'
		;;
	esac
done <<< "$changed_files"
dirs="$(printf '%s' "$dirs" | sort -u)" || fail "cannot union touched directories"
touched_dirs=()
while IFS= read -r dir; do
	[ -z "$dir" ] || touched_dirs+=("$dir")
done <<< "$dirs"
packages=""
if [ "${#touched_dirs[@]}" -gt 0 ]; then
	packages="$(go list -e -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "${touched_dirs[@]}")" \
		|| fail "touched Go package extraction failed"
	packages="$(printf '%s\n' "$packages" | sort -u)" || fail "cannot union touched packages"
fi
touched_packages=()
while IFS= read -r pkg; do
	[ -z "$pkg" ] || touched_packages+=("$pkg")
done <<< "$packages"

# Keep this SQL ASCII check identical to full mode.
step "internal/db/sql is ASCII-only (sqlc miscounts spans around multi-byte comments)"
if offenders="$(grep -lP "[^[:ascii:]]" internal/db/sql/*.sql 2>/dev/null)" && [ -n "$offenders" ]; then
	printf '[31mnon-ASCII bytes in:[0m
%s
' "$offenders"
	fail "a non-ASCII character in a .sql file silently corrupts sqlc codegen -- replace it with an ASCII equivalent"
fi

step "no stale large/small model-slot spelling (renamed to smart/fast)"
bash "$HOOK_DIR/check_stale_model_slots.sh" || fail "stale large/small model-slot text found -- see output above"
step "test files pair db.Connect/db.ConnectRead with db.Release (ref-counted pool contract)"
bash "$HOOK_DIR/check_db_release_pairing.sh" || fail "unpaired db.Connect/db.ConnectRead in a test file -- see output above"
step "no hand-written source file over 1000 lines (ratcheted allowlist)"
bash "$HOOK_DIR/check_file_size.sh" || fail "file-size ratchet violated -- see output above"

step "go.mod/go.sum are tidy (go mod tidy -diff)"
go mod tidy -diff || fail "go.mod/go.sum not tidy -- run 'go mod tidy' and commit"
if [ "${#changed_go_files[@]}" -eq 0 ]; then
	printf '%s\n' 'no Go files changed'
else
	gcl=""
	if [ -x .rush/audit/bin/golangci-lint.exe ]; then
		gcl=".rush/audit/bin/golangci-lint.exe"
	elif command -v golangci-lint >/dev/null 2>&1; then
		gcl="golangci-lint"
	fi
	if [ -n "$gcl" ]; then
		step "golangci-lint fmt --diff changed Go files (nonempty output fails)"
		# Sentinel preserves trailing newlines: even newline-only output fails.
		fmt_output="$("$gcl" fmt --diff "${changed_go_files[@]}" 2>&1; status=$?; printf '.'; exit "$status")" \
			|| fail "formatting check failed: ${fmt_output%.}"
		fmt_output="${fmt_output%.}"
		[ -z "$fmt_output" ] || fail "formatting output is nonempty: $fmt_output"
	else
		printf '\033[33m⚠ FAST formatting SKIPPED: golangci-lint missing from .rush/audit/bin and PATH; CI remains the gate\033[0m\n'
	fi
fi

step "go build -p 1 ./..."
run_capped "$RUSH_BUILD_MEMORY" "$RUSH_BUILD_TIMEOUT" go build -p 1 ./... || fail "build failed"
if [ "${#touched_packages[@]}" -gt 0 ]; then
	step "go vet -p 1 touched packages"
	run_capped "$RUSH_BUILD_MEMORY" "$RUSH_BUILD_TIMEOUT" go vet -p 1 "${touched_packages[@]}" || fail "touched-package vet failed"
else
	printf '%s\n' 'no Go packages changed'
fi

# Unconditional: shell/config-only pushes must not drop spawn protection.
step "platform spawn guards (always)"
run_capped "$RUSH_PROCESS_MEMORY" "$RUSH_PROCESS_TIMEOUT" go test -short -count=1 -p 1 -parallel 1 \
	-run '^(TestNoUnhardenedProcessSpawns|TestSpawnGuardExemptionsAreCurrent)$' ./internal/platform/ \
	|| fail "platform spawn guards failed"

if [ "$hook_checks" -eq 1 ]; then
	for selftest in check_file_size_selftest.sh check_db_release_pairing_selftest.sh check_list_test_packages.sh check_run_test_segment_retry_flags.sh; do
		step "$selftest"
		bash "$HOOK_DIR/$selftest" || fail "$selftest failed"
	done
	step "run_capped timeout and exit-code semantics"
	wrapper="$RUN_CAPPED"
	if command -v cygpath >/dev/null 2>&1; then
		wrapper="$(cygpath -w "$wrapper")"
	fi
	powershell -NoProfile -ExecutionPolicy Bypass -File "$wrapper" -SelfTest \
		|| fail "run_capped wrapper self-test failed"
fi
printf '\033[33mFAST checks passed — lint/tests/cross-GOOS/web NOT run\033[0m\n'
