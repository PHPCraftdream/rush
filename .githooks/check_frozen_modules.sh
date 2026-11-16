#!/usr/bin/env bash
# check_frozen_modules.sh: module guard enforcing the frozen-module rule
# recorded in docs/modules.md.
#
# docs/modules.md holds the module registry table. Its rows are table
# lines whose first cell is a module path in backticks, with the
# columns: `module` | `status` | `purpose` | `public API` | `layer`.
# Parsing is strict and fail-closed:
#   - a backtick line that is not a valid table row fails;
#   - a parseable row whose status is not exactly one of
#     frozen|stable|active|hot fails;
#   - a module path not starting with internal/ fails.
# Every failure prints the offending line. A missing docs/modules.md is
# itself a loud failure, never a silent pass.
#
# Changed files come from `git diff --name-only <base>..HEAD`, where
# base is the first CLI argument, else $FROZEN_BASE, else
# `git merge-base origin/main HEAD`. Read-only git only.
#
# Rule per changed file, attributed to the LONGEST matching registered
# module prefix:
#   frozen: the range must contain a commit-message line
#           `Unfreeze: <module path> - <reason>` naming the owning
#           module (an em dash or a hyphen is accepted as the
#           separator, the reason must be non-empty); otherwise fail,
#           listing the files and their owning module.
#   stable: a warning to stderr, then exit 0.
#   active/hot: nothing.
#
# Pure bash, no network, works on Windows Git Bash. Deliberately NOT
# set -e: see check_stale_model_slots.sh for why.
set -uo pipefail

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'check_frozen_modules.sh: not inside a git work tree; refusing to guess\n' >&2
	exit 1
fi
if ! toplevel="$(git rev-parse --show-toplevel)"; then
	printf 'check_frozen_modules.sh: git rev-parse --show-toplevel failed\n' >&2
	exit 1
fi
cd "$toplevel" || exit 1

registry="docs/modules.md"
if [ ! -f "$registry" ]; then
	printf 'check_frozen_modules.sh: missing %s (the module registry; the frozen-module guard cannot run without it)\n' "$registry" >&2
	exit 1
fi

if [ $# -gt 1 ]; then
	printf 'check_frozen_modules.sh: usage: check_frozen_modules.sh [<base>]\n' >&2
	exit 3
fi
if [ $# -eq 1 ]; then
	base="$1"
elif [ -n "${FROZEN_BASE:-}" ]; then
	base="$FROZEN_BASE"
else
	if ! base="$(git merge-base origin/main HEAD)"; then
		printf 'check_frozen_modules.sh: git merge-base origin/main HEAD failed; pass an explicit <base> argument or set FROZEN_BASE\n' >&2
		exit 1
	fi
fi
if [ -z "$base" ]; then
	printf 'check_frozen_modules.sh: resolved base is empty\n' >&2
	exit 1
fi

if ! changed="$(git diff --name-only "$base..HEAD")"; then
	printf 'check_frozen_modules.sh: git diff --name-only %s..HEAD failed\n' "$base" >&2
	exit 1
fi
if ! messages="$(git log "$base..HEAD" --pretty=%B)"; then
	printf 'check_frozen_modules.sh: git log %s..HEAD --pretty=%%B failed\n' "$base" >&2
	exit 1
fi

# Parse the registry into parallel arrays. Any bad input fails loudly
# with the offending line. (mod_n/module/status arrays stay in sync.)
mod_n=0
bad=0
while IFS= read -r line || [ -n "$line" ]; do
	line="${line%$'\r'}"
	case "$line" in
		*'`'*)
			;;
		*)
			continue # no backtick: header/separator/prose, skip
			;;
	esac
	trimmed="${line#"${line%%[![:space:]]*}"}"
	rest="$trimmed"
	case "$rest" in
		'`'*)
			;;
		*)
			printf 'check_frozen_modules.sh: malformed registry row (first cell must be a backtick-quoted module path): %s\n' "$line" >&2
			bad=1
			continue
			;;
	esac
	bt='`'
	rest2="${rest#"$bt"}"       # drop the opening backtick
	module="${rest2%%"$bt"*}"   # path up to the closing backtick
	case "$rest2" in
		*"$bt"*) ;;
		*)
			printf 'check_frozen_modules.sh: malformed registry row (unterminated backtick-quoted module path): %s\n' "$line" >&2
			bad=1
			continue
			;;
	esac
	rest="${rest2#*"$bt"}"      # everything after the closing backtick
	rest="${rest#"${rest%%[![:space:]]*}"}" # leading spaces before the pipe
	case "$rest" in
		'|'*) ;;
		*)
			printf 'check_frozen_modules.sh: malformed registry row (expected " | " after the module path): %s\n' "$line" >&2
			bad=1
			continue
			;;
	esac
	rest="${rest#|}"
	case "$rest" in
		*'|'*) ;;
		*)
			printf 'check_frozen_modules.sh: malformed registry row (expected 5 pipe-separated cells): %s\n' "$line" >&2
			bad=1
			continue
			;;
	esac
	status="${rest%%|*}"
	rest="${rest#*|}"
	case "$rest" in
		*'|'*) ;;
		*)
			printf 'check_frozen_modules.sh: malformed registry row (expected 5 pipe-separated cells): %s\n' "$line" >&2
			bad=1
			continue
			;;
	esac
	cell3="${rest%%|*}"
	rest="${rest#*|}"
	cell4="${rest%%|*}"
	cell5="${rest#*|}"
	# Trim spaces/tabs from the cells we validate.
	status="${status#"${status%%[![:space:]]*}"}"
	status="${status%"${status##*[![:space:]]}"}"
	module="${module#"${module%%[![:space:]]*}"}"
	module="${module%"${module##*[![:space:]]}"}"
	case "$status" in
		frozen|stable|active|hot) ;;
		*)
			printf 'check_frozen_modules.sh: invalid status "%s" in registry row (must be frozen|stable|active|hot): %s\n' "$status" "$line" >&2
			bad=1
			continue
			;;
	esac
	case "$module" in
		internal/*) ;;
		*)
			printf 'check_frozen_modules.sh: module path "%s" does not start with internal/ in registry row: %s\n' "$module" "$line" >&2
			bad=1
			continue
			;;
	esac
	i=0
	while [ "$i" -lt "$mod_n" ]; do
		if [ "${modules[$i]}" = "$module" ]; then
			printf 'check_frozen_modules.sh: duplicate registry entry for %s in row: %s\n' "$module" "$line" >&2
			bad=1
			break
		fi
		i=$((i + 1))
	done
	[ "$bad" -eq 1 ] && continue
	modules[$mod_n]="$module"
	statuses[$mod_n]="$status"
	mod_n=$((mod_n + 1))
done <"$registry"

if [ "$bad" -ne 0 ]; then
	printf 'check_frozen_modules.sh: %s is broken (see offending lines above); refusing to report a pass\n' "$registry" >&2
	exit 1
fi
if [ "$mod_n" -eq 0 ]; then
	printf 'check_frozen_modules.sh: no parseable module rows in %s; refusing to report a pass\n' "$registry" >&2
	exit 1
fi

# longest_owner <file>: echoes "<owner> <status>" for the LONGEST
# registered module prefix of the file, or nothing if no module owns it.
longest_owner() {
	file="$1"
	best_len=-1
	i=0
	while [ "$i" -lt "$mod_n" ]; do
		m="${modules[$i]}"
		case "$file" in
			"$m") len=${#m} ;;
			"$m"/*) len=${#m} ;;
			*) i=$((i + 1)); continue ;;
		esac
		if [ "$len" -gt "$best_len" ]; then
			best_len="$len"
			best_i="$i"
		fi
		i=$((i + 1))
	done
	if [ "$best_len" -ge 0 ]; then
		printf '%s %s\n' "${modules[$best_i]}" "${statuses[$best_i]}"
	fi
}

# Unfreeze lines: `Unfreeze: <module> - <reason>` (em dash or hyphen
# separator, reason non-empty). Collected once.
unfreeze_modules=()
unfreeze_n=0
while IFS= read -r uline || [ -n "$uline" ]; do
	uline="${uline%$'\r'}"
	case "$uline" in
		Unfreeze:*|unfreeze:*) ;;
		*) continue ;;
	esac
	after="${uline#*:}"
	after="${after#"${after%%[![:space:]]*}"}"
	sep=""
	case "$after" in
		*" - "*) sep=" - " ;;
		*" — "*) sep=" — " ;;
		*" – "*) sep=" – " ;;
		*) continue # no separator: not an Unfreeze line
	esac
	umod="${after%%"$sep"*}"
	reason="${after#*"$sep"}"
	reason="${reason#"${reason%%[![:space:]]*}"}"
	if [ -z "$reason" ]; then
		continue
	fi
	unfreeze_modules[$unfreeze_n]="$umod"
	unfreeze_n=$((unfreeze_n + 1))
done <<<"$messages"

violations=0
warned=0
covered=0
if [ -n "$changed" ]; then
	while IFS= read -r file || [ -n "$file" ]; do
		file="${file%$'\r'}"
		[ -z "$file" ] && continue
		owner_info="$(longest_owner "$file")"
		[ -z "$owner_info" ] && continue
		owner="${owner_info% *}"
		ostatus="${owner_info#* }"
		case "$ostatus" in
			frozen)
				ok=0
				j=0
				while [ "$j" -lt "$unfreeze_n" ]; do
					if [ "${unfreeze_modules[$j]}" = "$owner" ]; then
						ok=1
						break
					fi
					j=$((j + 1))
				done
				if [ "$ok" -eq 1 ]; then
					covered=$((covered + 1))
				else
					printf 'check_frozen_modules.sh: FROZEN module %s has changed files without an "Unfreeze: %s - <reason>" commit-message line:\n' "$owner" "$owner" >&2
					printf '  %s\n' "$file" >&2
					violations=$((violations + 1))
				fi
				;;
			stable)
				printf 'check_frozen_modules.sh: warning: changed file %s is under stable module %s (not frozen; no Unfreeze line required)\n' "$file" "$owner" >&2
				warned=$((warned + 1))
				;;
		esac
	done <<<"$changed"
fi

if [ "$violations" -gt 0 ]; then
	printf '\033[31mfrozen-modules guard: %d frozen-file violation(s) (%d frozen file(s) covered by Unfreeze lines, %d stable warning(s), base %s)\033[0m\n' "$violations" "$covered" "$warned" "$base"
	exit 1
fi
printf '\033[32mfrozen-modules guard: %d frozen file(s) covered by Unfreeze lines, %d stable warning(s), %d registered module(s), base %s\033[0m\n' "$covered" "$warned" "$mod_n" "$base"
exit 0
