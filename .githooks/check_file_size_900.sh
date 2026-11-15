#!/usr/bin/env bash
# check_file_size_900.sh: guard making the 901..1000-line band BLOCKING
# via a ratchet, on the way to the Sprint 0 goal "no hand-written source
# file over 900 lines".
#
# The operator's rule: the existing 1000-line guard
# (.githooks/check_file_size.sh) is a hard limit with its own ratchet
# and keeps working unchanged above this one; this guard covers the band
# just under it. A hand-written file above 900 lines that is not in the
# ratchet list fails, a listed file that GROWS fails, and a listed file
# that has dropped to 900 lines or fewer fails with a "remove it from"
# message. The 901..1000 band therefore stops growing.
#
# What it checks: every TRACKED *.go, *.ts, *.tsx file (git ls-files --
# the index, so worktrees, node_modules, web/dist and .claude are
# excluded by construction, and untracked scratch files never block).
#
# Generated files are skipped by CONTENT, not by name: any "Code
# generated" / "DO NOT EDIT" marker within the first 10 lines (same
# convention as the sibling guard).
#
# RATCHET: .githooks/file_size_900_allowlist.txt lists known violations
# as "<path> <lines>", one per line (git-style forward-slash, repo-root
# relative paths; blank lines and # comments allowed; paths may not
# contain spaces, so the count is the field after the LAST space).
# There is NO warning band here: the sibling's warn band already
# reports this range, and duplicating it would only add noise.
#
# This guard does NOT talk to .golangci.yml; the 1000-line rule, its
# allowlist and its yml bijection are untouched and stay the single
# source of truth for that layer.
#
# Implementation: ONE awk process reads the filename list on stdin and
# every source file itself (getline), so there is no per-file process
# spawn (a shell loop over 1200+ files costs minutes on Windows -- see
# check_stale_model_slots.sh), no temp files, and no xargs (this
# repository's Windows shells ship an xargs without -d, and plain xargs
# mangles paths). Line counting matches wc -l for newline-terminated
# files and revive's bufio.Scanner counting otherwise. Deliberately NOT
# set -e: see check_stale_model_slots.sh for why (-e would abort on
# legitimate non-zero exits inside the pipeline).
set -uo pipefail

MAX_LINES=900

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'check_file_size_900.sh: not inside a git work tree; refusing to guess\n' >&2
	exit 1
fi
if ! toplevel="$(git rev-parse --show-toplevel)"; then
	printf 'check_file_size_900.sh: git rev-parse --show-toplevel failed\n' >&2
	exit 1
fi
cd "$toplevel" || exit 1

allowlist=".githooks/file_size_900_allowlist.txt"
if [ ! -f "$allowlist" ]; then
	printf 'check_file_size_900.sh: missing %s (the ratchet list; an empty file means "no file may exceed %d lines")\n' "$allowlist" "$MAX_LINES" >&2
	exit 1
fi

# awk exit codes: 0 pass, 1 size violations (report on stdout), 3 broken
# guard inputs -- malformed allowlist, unreadable tracked file (report
# already on stderr). Any other status means the pipeline itself failed
# (git ls-files, caught via pipefail). Every non-zero is fatal: this
# guard must fail closed.
report="$(git -c core.quotepath=off ls-files -- '*.go' '*.ts' '*.tsx' \
	| awk -v max_lines="$MAX_LINES" -v allowlist="$allowlist" '
	BEGIN {
		fatal = 0
		checked = 0
		generated = 0
		kept = 0
		violations = 0
		while ((getline line < allowlist) > 0) {
			sub(/\r$/, "", line)
			if (line == "" || line ~ /^#/) {
				continue
			}
			if (match(line, /[ \t]+[0-9]+$/)) {
				count = substr(line, RSTART, RLENGTH)
				sub(/^[ \t]+/, "", count)
				path = substr(line, 1, RSTART - 1)
			} else {
				printf "check_file_size_900.sh: malformed allowlist line (expected \"<path> <lines>\"): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path == "") {
				printf "check_file_size_900.sh: malformed allowlist line (empty path): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path in allowed) {
				printf "check_file_size_900.sh: duplicate allowlist entry for %s\n", path > "/dev/stderr"
				fatal = 1
				continue
			}
			allowed[path] = count + 0
		}
		close(allowlist)
	}
	fatal { next }
	{
		file = $0
		lines = 0
		gen = 0
		st = 0
		while ((st = getline srcline < file) > 0) {
			lines++
			if (lines <= 10 && (srcline ~ /Code generated/ || srcline ~ /DO NOT EDIT/)) {
				gen = 1
			}
		}
		close(file)
		if (st == -1) {
			printf "check_file_size_900.sh: cannot read tracked file %s\n", file > "/dev/stderr"
			fatal = 1
			exit
		}
		seen[file] = 1
		checked++
		if (gen) {
			generated++
			next
		}
		if (lines > max_lines) {
			if (file in allowed) {
				if (lines > allowed[file]) {
					printf "RATCHET VIOLATION: %s grew to %d lines (allowlist records %d); shrink or decompose it back under its recorded count\n", file, lines, allowed[file]
					violations++
				} else {
					kept++
				}
			} else {
				printf "OVER 900: %s is %d lines (max %d) and is not in %s; decompose it, or record its CURRENT count as \"<path> <lines>\" in the allowlist\n", file, lines, max_lines, allowlist
				violations++
			}
		} else if (file in allowed) {
			printf "RATCHET CLEANUP: %s is now %d lines (max %d) -- remove it from %s. Thanks!\n", file, lines, max_lines, allowlist
			violations++
		}
	}
	END {
		if (fatal) {
			exit 3
		}
		stale = 0
		for (p in allowed) {
			if (!(p in seen)) {
				if (stale == 0) {
					printf "warning: stale allowlist entries (path no longer tracked; delete the line):\n" > "/dev/stderr"
				}
				printf "  %s %d\n", p, allowed[p] > "/dev/stderr"
				stale++
			}
		}
		if (violations > 0) {
			printf "\033[31mfile-size-900 guard: %d violation(s) (%d file(s) checked, %d generated skipped, %d allowlisted over 900 lines, max %d)\033[0m\n", violations, checked, generated, kept, max_lines
			exit_status = 1
		} else {
			printf "\033[32mfile-size-900 guard: %d file(s) checked, %d generated skipped, %d allowlisted over 900 lines, max %d\033[0m\n", checked, generated, kept, max_lines
			exit_status = 0
		}
		exit exit_status
	}')"
status=$?

if [ "$status" -eq 0 ]; then
	printf '%s\n' "$report"
	exit 0
fi
if [ "$status" -eq 1 ]; then
	printf '%s\n' "$report"
	exit 1
fi
if [ "$status" -eq 3 ]; then
	printf 'check_file_size_900.sh: guard inputs are broken (see messages above); refusing to report a pass\n' >&2
	exit 1
fi
printf 'check_file_size_900.sh: git ls-files failed (exit %s); refusing to report a false pass\n' "$status" >&2
exit 1
