#!/usr/bin/env bash
# check_dir_entries.sh: guard enforcing the operator's Sprint 0 rule "at
# most 7 elements per code directory" (docs/plans/2026-11-15-sprint0-plan.md,
# stage A1).
#
# A CODE DIRECTORY is any tracked directory that is an ancestor of at
# least one tracked *.go, *.ts or *.tsx file. Its ELEMENTS are its
# direct child code directories plus the non-generated, non-test
# source files sitting directly inside it. The repository root is
# excluded from the rule entirely, as is every path under a segment
# named testdata, node_modules or dist.
#
# Test files (*_test.go, *.test.ts(x), *.spec.ts(x)) do not count as
# elements unless DIR_ENTRIES_COUNT_TESTS=1 is set or --count-tests is
# passed (they still mark their ancestors as code directories).
#
# RATCHET: .githooks/dir_entries_allowlist.txt lists known violations
# as "<directory> <count>", one per line (repo-root relative, forward
# slashes; blank lines and # comments allowed). A listed directory may
# stay over the limit but fails the moment it GROWS; a directory at or
# under the limit must be removed from the list -- the list must hold
# only live debt, or it silently becomes an exemption list.
#
# Implementation: ONE awk process reads the filename list on stdin and
# the first 10 lines of candidate element files (getline, stopping
# early), so there is no per-file process spawn -- a shell loop over
# 1200+ files costs minutes on Windows. Deliberately NOT set -e: see
# check_stale_model_slots.sh for why.
set -uo pipefail

MAX_ENTRIES=7

count_tests=0
if [ "${DIR_ENTRIES_COUNT_TESTS:-0}" = "1" ]; then
	count_tests=1
fi
if [ $# -gt 0 ]; then
	if [ "$1" = "--count-tests" ]; then
		count_tests=1
	else
		printf 'check_dir_entries.sh: unknown argument %s; usage: check_dir_entries.sh [--count-tests]\n' "$1" >&2
		exit 3
	fi
fi

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'check_dir_entries.sh: not inside a git work tree; refusing to guess\n' >&2
	exit 1
fi
if ! toplevel="$(git rev-parse --show-toplevel)"; then
	printf 'check_dir_entries.sh: git rev-parse --show-toplevel failed\n' >&2
	exit 1
fi
cd "$toplevel" || exit 1

allowlist=".githooks/dir_entries_allowlist.txt"
if [ ! -f "$allowlist" ]; then
	printf 'check_dir_entries.sh: missing %s (the ratchet list; an empty file means "no directory may exceed %d elements")\n' "$allowlist" "$MAX_ENTRIES" >&2
	exit 1
fi

# awk exit codes: 0 pass, 1 violations (report on stdout), 3 broken
# guard inputs -- malformed allowlist, unreadable tracked file (report
# already on stderr). Any other status means the pipeline itself failed
# (git ls-files, caught via pipefail). Every non-zero is fatal: this
# guard must fail closed.
report="$(git -c core.quotepath=off ls-files -- '*.go' '*.ts' '*.tsx' \
	| awk -v max_entries="$MAX_ENTRIES" -v count_tests="$count_tests" -v allowlist="$allowlist" '
	BEGIN {
		fatal = 0
		dirs = 0
		counted = 0
		genskip = 0
		kept = 0
		vn = 0
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
				printf "check_dir_entries.sh: malformed allowlist line (expected \"<directory> <count>\"): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path == "") {
				printf "check_dir_entries.sh: malformed allowlist line (empty path): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path in allowed) {
				printf "check_dir_entries.sh: duplicate allowlist entry for %s\n", path > "/dev/stderr"
				fatal = 1
				continue
			}
			allowed[path] = count + 0
		}
		close(allowlist)
	}
	fatal { next }
	{
		n = split($0, seg, "/")
		if (n < 2) {
			next
		}
		for (i = 1; i <= n; i++) {
			if (seg[i] == "testdata" || seg[i] == "node_modules" || seg[i] == "dist") {
				next
			}
		}
		# Mark every ancestor directory as a code directory; the first
		# time one appears, bump its parent child-code-dir counter
		# (parent "" is the root, which the rule excludes).
		parent = ""
		dir = ""
		for (i = 1; i <= n - 1; i++) {
			dir = (i == 1) ? seg[1] : dir "/" seg[i]
			if (!(dir in codedir)) {
				codedir[dir] = 1
				dirs++
				if (parent != "") {
					childdirs[parent]++
				}
			}
			parent = dir
		}
		base = seg[n]
		if (count_tests == 0 && \
			(base ~ /_test\.go$/ || base ~ /\.test\.tsx?$/ || base ~ /\.spec\.tsx?$/)) {
			next
		}
		# Generated detection on the first 10 lines only: read no
		# further than the marker can live.
		lines = 0
		gen = 0
		st = 0
		while (lines < 10 && (st = (getline srcline < $0)) > 0) {
			lines++
			if (srcline ~ /Code generated/ || srcline ~ /DO NOT EDIT/) {
				gen = 1
			}
		}
		close($0)
		if (st == -1) {
			printf "check_dir_entries.sh: cannot read tracked file %s\n", $0 > "/dev/stderr"
			fatal = 1
			exit
		}
		if (gen) {
			genskip++
			next
		}
		counted++
		countedfiles[parent]++
	}
	END {
		if (fatal) {
			exit 3
		}
		for (d in codedir) {
			total = ((d in childdirs) ? childdirs[d] : 0) \
				+ ((d in countedfiles) ? countedfiles[d] : 0)
			if (total > max_entries) {
				if (d in allowed) {
					if (total > allowed[d]) {
						vn++
						vdir[vn] = d
						vmsg[d] = sprintf("RATCHET VIOLATION: %s grew to %d elements (allowlist records %d); shrink it back under its recorded count", d, total, allowed[d])
					} else {
						kept++
					}
				} else {
					vn++
					vdir[vn] = d
					vmsg[d] = sprintf("TOO MANY ENTRIES: %s has %d elements (max %d) and is not in %s; split it into subdirectories, or record its CURRENT count as \"%s %d\" in the allowlist", d, total, max_entries, allowlist, d, total)
				}
			} else if (d in allowed) {
				vn++
				vdir[vn] = d
				vmsg[d] = sprintf("RATCHET CLEANUP: %s is now %d elements (max %d) -- remove it from %s. Thanks!", d, total, max_entries, allowlist)
			}
		}
		# Insertion sort by directory so the report is deterministic.
		for (i = 2; i <= vn; i++) {
			v = vdir[i]
			j = i - 1
			while (j >= 1 && vdir[j] > v) {
				vdir[j + 1] = vdir[j]
				j--
			}
			vdir[j + 1] = v
		}
		for (i = 1; i <= vn; i++) {
			print vmsg[vdir[i]]
		}
		stale = 0
		for (p in allowed) {
			if (!(p in codedir)) {
				if (stale == 0) {
					printf "warning: stale allowlist entries (directory no longer a code directory; delete the line):\n" > "/dev/stderr"
				}
				printf "  %s %d\n", p, allowed[p] > "/dev/stderr"
				stale++
			}
		}
		suffix = count_tests ? " (test files counted)" : ""
		if (vn > 0) {
			printf "\033[31mdir-entries guard: %d violation(s) (%d code dir(s) checked, %d element file(s) counted, %d generated skipped, %d allowlisted over %d, max %d)%s\033[0m\n", vn, dirs, counted, genskip, kept, max_entries, max_entries, suffix
			exit 1
		}
		printf "\033[32mdir-entries guard: %d code dir(s) checked, %d element file(s) counted, %d generated skipped, %d allowlisted over %d, max %d%s\033[0m\n", dirs, counted, genskip, kept, max_entries, max_entries, suffix
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
	printf 'check_dir_entries.sh: guard inputs are broken (see messages above); refusing to report a pass\n' >&2
	exit 1
fi
printf 'check_dir_entries.sh: git ls-files failed (exit %s); refusing to report a false pass\n' "$status" >&2
exit 1
