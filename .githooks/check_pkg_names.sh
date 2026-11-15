#!/usr/bin/env bash
# check_pkg_names.sh: guard enforcing the Sprint 0 rule "every non-test,
# non-main Go package directory's declared package name must be unique
# across the repository" (docs/plans/2026-11-15-sprint0-plan.md). Two
# directories declaring the same package name is a violation: the name
# stops being a reliable address for the code, and importers cannot tell
# the twins apart in error messages or docs.
#
# Skipped on purpose: *_test.go files (their package clause is a
# convention, not an identity), package main (entry points may share
# the name by design), and anything under a testdata segment (go
# tooling ignores testdata entirely).
#
# RATCHET: .githooks/pkg_names_allowlist.txt lists known collisions as
# "<package-name> <count-of-declaring-directories>". A listed name may
# stay duplicated but fails the moment it GROWS; a name back down to a
# single declaring directory must be removed from the list -- entries
# may only be removed.
#
# Implementation mirrors check_file_size.sh: ONE awk process over git
# ls-files, package clause found by reading at most the first 200
# lines of each tracked .go file (getline, stopping at the match), so
# long leading doc comments are tolerated but unbounded scans are not
# (the largest package clause offset in this repo is line 105); no
# per-file process spawn. Deliberately NOT set -e: see
# check_stale_model_slots.sh for why.
set -uo pipefail

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'check_pkg_names.sh: not inside a git work tree; refusing to guess\n' >&2
	exit 1
fi
if ! toplevel="$(git rev-parse --show-toplevel)"; then
	printf 'check_pkg_names.sh: git rev-parse --show-toplevel failed\n' >&2
	exit 1
fi
cd "$toplevel" || exit 1

allowlist=".githooks/pkg_names_allowlist.txt"
if [ ! -f "$allowlist" ]; then
	printf 'check_pkg_names.sh: missing %s (the ratchet list; an empty file means "every package name must be unique")\n' "$allowlist" >&2
	exit 1
fi

# awk exit codes: 0 pass, 1 collisions (report on stdout), 3 broken
# guard inputs -- malformed allowlist, unreadable tracked file, file
# without a package clause in its first 200 lines, one directory
# declaring two names (report
# already on stderr). Any other status means the pipeline itself failed
# (git ls-files, caught via pipefail). Every non-zero is fatal: this
# guard must fail closed.
report="$(git -c core.quotepath=off ls-files -- '*.go' \
	| awk -v allowlist="$allowlist" -v max_package_line=200 '
	BEGIN {
		fatal = 0
		dirscount = 0
		nlist = 0
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
				printf "check_pkg_names.sh: malformed allowlist line (expected \"<package-name> <count>\"): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path == "") {
				printf "check_pkg_names.sh: malformed allowlist line (empty name): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path in allowed) {
				printf "check_pkg_names.sh: duplicate allowlist entry for %s\n", path > "/dev/stderr"
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
		if (seg[n] ~ /_test\.go$/) {
			next
		}
		for (i = 1; i <= n; i++) {
			if (seg[i] == "testdata") {
				next
			}
		}
		# The first package clause within the first 200 lines wins.
		name = ""
		st = 0
		for (k = 1; k <= max_package_line && (st = (getline srcline < $0)) > 0; k++) {
			if (srcline ~ /^package[ \t]+[A-Za-z_]/) {
				sub(/\r$/, "", srcline)
				split(srcline, f, /[ \t]+/)
				name = f[2]
				break
			}
		}
		close($0)
		if (name == "") {
			if (st == -1) {
				printf "check_pkg_names.sh: cannot read tracked file %s\n", $0 > "/dev/stderr"
			} else {
				printf "check_pkg_names.sh: cannot find a package clause in %s\n", $0 > "/dev/stderr"
			}
			fatal = 1
			exit
		}
		if (name == "main") {
			next
		}
		dir = ""
		for (i = 1; i <= n - 1; i++) {
			dir = (i == 1) ? seg[1] : dir "/" seg[i]
		}
		label = (dir == "") ? "(root)" : dir
		if (label in pkgname && pkgname[label] != name) {
			printf "check_pkg_names.sh: directory %s declares both %s and %s\n", label, pkgname[label], name > "/dev/stderr"
			fatal = 1
			exit
		}
		if (!(label in pkgname)) {
			pkgname[label] = name
			dirscount++
		}
		if (!((name, label) in member)) {
			member[name, label] = 1
			if (!(name in ndirs)) {
				nlist++
				names[nlist] = name
			}
			ndirs[name]++
		}
	}
	END {
		if (fatal) {
			exit 3
		}
		for (i = 1; i <= nlist; i++) {
			name = names[i]
			c = ndirs[name]
			if (name in allowed) {
				if (c > 1) {
					if (c > allowed[name]) {
						vn++
						vdir[vn] = name
						vmsg[name] = sprintf("RATCHET VIOLATION: \"%s\" is now declared by %d directories (allowlist records %d)", name, c, allowed[name])
					} else {
						kept++
					}
				} else {
					vn++
					vdir[vn] = name
					vmsg[name] = sprintf("RATCHET CLEANUP: \"%s\" is now unique -- remove it from %s. Thanks!", name, allowlist)
				}
			} else if (c > 1) {
				# Sorted declaring-directory list (insertion sort).
				m = 0
				for (label in pkgname) {
					if (pkgname[label] == name) {
						dn[++m] = label
					}
				}
				for (a = 2; a <= m; a++) {
					v = dn[a]
					b = a - 1
					while (b >= 1 && dn[b] > v) {
						dn[b + 1] = dn[b]
						b--
					}
					dn[b + 1] = v
				}
				where = dn[1]
				for (j = 2; j <= m; j++) {
					where = where " " dn[j]
				}
				vn++
				vdir[vn] = name
				vmsg[name] = sprintf("DUPLICATE PACKAGE NAME: \"%s\" is declared by %d directories: %s\nrename one of them, or record the CURRENT count as \"%s %d\" in %s", name, c, where, name, c, allowlist)
			}
		}
		# Insertion sort by package name so the report is deterministic.
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
			if (!(p in ndirs)) {
				if (stale == 0) {
					printf "warning: stale allowlist entries (package name no longer declared; delete the line):\n" > "/dev/stderr"
				}
				printf "  %s %d\n", p, allowed[p] > "/dev/stderr"
				stale++
			}
		}
		if (vn > 0) {
			printf "\033[31mpkg-names guard: %d violation(s) (%d package dir(s) checked, %d unique name(s), %d allowlisted collision(s))\033[0m\n", vn, dirscount, nlist, kept
			exit 1
		}
		printf "\033[32mpkg-names guard: %d package dir(s) checked, %d unique name(s), %d allowlisted collision(s)\033[0m\n", dirscount, nlist, kept
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
	printf 'check_pkg_names.sh: guard inputs are broken (see messages above); refusing to report a pass\n' >&2
	exit 1
fi
printf 'check_pkg_names.sh: git ls-files failed (exit %s); refusing to report a false pass\n' "$status" >&2
exit 1
