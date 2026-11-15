#!/usr/bin/env bash
# check_layers.sh: guard enforcing the internal layer direction of
# docs/plans/2026-11-15-sprint0-plan.md (stage A2):
#
#   kit < store < session < config < agent < app < serve
#
# A package under internal/<group>/ must not import a package of a
# strictly higher group. Data source: `go list -json ./...` fields
# Imports (production), TestImports and XTestImports (tests). Every
# package outside internal/ (cmd/, sdk/, the repository root) is exempt
# as an importer, as is everything under internal/app/itest -- the one
# place that may import everything. Import targets outside internal/
# are never violations.
#
# RATCHET (deliberately stricter than the sibling guards): the lists
#   .githooks/layer_exceptions.txt     "<package-import-path> <count>"
#   .githooks/layer_test_exceptions.txt "<repo-relative test file> <count>"
# must hold only live debt. An entry whose violation dropped to 0, or
# whose package/file no longer exists, FAILS the guard with a STALE
# EXCEPTION or RATCHET CLEANUP message -- it does not merely warn.
# An entry that grows past its recorded count fails with RATCHET
# VIOLATION. Blank lines and # comments are allowed; every live entry
# needs a one-line reason above it.
#
# Implementation: ONE awk process parses the whole `go list -json`
# stream by line shape (no JSON library), so there is no per-package
# process spawn. Test files are scanned only for packages whose test
# imports already point upward. Cache nothing: every run recomputes
# from go list. Deliberately NOT set -e: see check_stale_model_slots.sh
# for why.
set -uo pipefail

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf 'check_layers.sh: not inside a git work tree; refusing to guess\n' >&2
	exit 1
fi
if ! toplevel="$(git rev-parse --show-toplevel)"; then
	printf 'check_layers.sh: git rev-parse --show-toplevel failed\n' >&2
	exit 1
fi
cd "$toplevel" || exit 1

if ! command -v go >/dev/null 2>&1; then
	printf 'check_layers.sh: go not found on PATH; the layer guard needs the Go toolchain and must fail closed\n' >&2
	exit 1
fi

# Serialize go invocations through the machine-local lock wrapper when
# it exists (agent-shared worktrees); plain go everywhere else (Linux
# CI has no .rush/).
if [ -f .rush/tmp/golock.sh ]; then
	go_cmd() {
		bash .rush/tmp/golock.sh go "$@"
	}
else
	go_cmd() {
		go "$@"
	}
fi

if ! module="$(go_cmd list -m)"; then
	printf 'check_layers.sh: go list -m failed (see stderr above)\n' >&2
	exit 1
fi
# Strip a trailing \r defensively (Windows pipes).
module="${module%$'\r'}"

# awk -v values undergo backslash-escape processing, so the toplevel is
# passed forward-slashed only.
topf="$(printf '%s' "$toplevel" | tr '\\' '/')"

prodlist=".githooks/layer_exceptions.txt"
testlist=".githooks/layer_test_exceptions.txt"
for list in "$prodlist" "$testlist"; do
	if [ ! -f "$list" ]; then
		printf 'check_layers.sh: missing %s (the ratchet list; an empty file means "no upward import is allowed")\n' "$list" >&2
		exit 1
	fi
done

# awk exit codes: 0 pass, 1 violations (report on stdout), 3 broken
# guard inputs -- unknown internal group, malformed ratchet list
# (report on stderr). Any other status means go list itself failed
# (caught via pipefail); go's stderr is passed through unredirected.
# Every non-zero is fatal: this guard must fail closed.
report="$(go_cmd list -json ./... \
	| awk -v module="$module" -v topf="$topf" \
		-v prodexc="$prodlist" -v testexc="$testlist" '
	function val(line,   s) {
		s = line
		sub(/^\t"[^"]*": "/, "", s)
		sub(/",?$/, "", s)
		gsub(/\\\\/, "\\", s)
		gsub(/\\/, "/", s)
		return s
	}
	function el(line,   s) {
		s = line
		sub(/^\t\t"/, "", s)
		sub(/",?$/, "", s)
		gsub(/\\\\/, "\\", s)
		gsub(/\\/, "/", s)
		return s
	}
	# rankof returns -1 (outside internal/), -2 (internal, unknown
	# group -- broken guard input) or the group rank; it sets RG to
	# the group name or "".
	function rankof(p, quiet,   rel, i) {
		RG = ""
		if (substr(p, 1, mlen) != module "/internal/") {
			return -1
		}
		rel = substr(p, mlen + 1)
		# internal/app/itest is the one place that may import
		# everything; treat it as exempt like any non-internal
		# package.
		if (rel == "app/itest" || substr(rel, 1, 10) == "app/itest/") {
			return -1
		}
		i = index(rel, "/")
		RG = (i > 1) ? substr(rel, 1, i - 1) : rel
		if (!(RG in grp)) {
			if (!quiet) {
				printf "check_layers.sh: broken guard input: package %s is under internal/%s/ which is not one of the seven layer groups (kit store session config agent app serve)\n", p, RG > "/dev/stderr"
				fatal = 1
			}
			return -2
		}
		return grp[RG] + 0
	}
	# addimp records one import of the package being parsed; only
	# internal importers with a strictly higher internal target
	# count.
	function addimp(kind, imp,   tr, key) {
		if (currank < 0) {
			return
		}
		tr = rankof(imp, 1)
		if (tr <= currank) {
			return
		}
		if (kind == "p") {
			key = curip SUBSEP imp
			if (!(key in pset)) {
				pset[key] = 1
				pcnt[curip]++
				pn[curip]++
				pl[curip, pn[curip]] = imp
				plg[curip, pn[curip]] = RG
			}
			return
		}
		key = curip SUBSEP imp
		if (!(key in tset)) {
			tset[key] = 1
			tcnt[curip]++
			tflag[curip] = 1
			tn[curip]++
			tl[curip, tn[curip]] = imp
			tlg[curip, tn[curip]] = RG
		}
	}
	# flush binds the pending Dir to the finished package (Dir is
	# emitted before ImportPath) and validates its group.
	function flush() {
		if (curip == "") {
			return
		}
		seen[curip] = 1
		npkg++
		pint[curip] = currank
		pgrp[curip] = curgrp
		pdir[curip] = penddir
		if (currank == -2) {
			# stderr already written by rankof
		} else if (currank >= 0) {
			nint++
			reld = curdir
			if (substr(reld, 1, length(topf)) == topf) {
				reld = substr(reld, length(topf) + 2)
			}
			pkgdirseen[reld] = 1
		}
		curip = ""
		penddir = ""
		currank = -1
		curgrp = ""
	}
	# scanpkgtests parses every *_test.go of one flagged package
	# and records, per file, the DISTINCT higher-group imports.
	function scanpkgtests(ip,   d, dirf, cmd, f, ln, inb, imp, key, relf, fi, fi2, gr, cnt, fn) {
		d = pdir[ip]
		if (substr(d, 1, length(topf)) == topf) {
			dirf = substr(d, length(topf) + 2)
		} else {
			dirf = d
		}
		cmd = "ls \047" dirf "\047/*_test.go 2>/dev/null"
		while ((cmd | getline f) > 0) {
			sub(/\r$/, "", f)
			relf = f
			if (substr(relf, 1, length(topf)) == topf) {
				relf = substr(relf, length(topf) + 2)
			}
			seentestf[relf] = 1
			fgrp[relf] = pgrp[ip]
			inb = 0
			fi = 0
			delete fileimp
			while ((getline ln < f) > 0) {
				sub(/\r$/, "", ln)
				if (inb) {
					if (ln ~ /^[ \t]*\)/) {
						inb = 0
						continue
					}
					if (ln ~ /^[ \t]*\/\//) {
						continue
					}
					if (match(ln, /"[^"]+"/)) {
						imp = substr(ln, RSTART + 1, RLENGTH - 2)
						if (!((f SUBSEP imp) in fileimp)) {
							fileimp[f SUBSEP imp] = 1
							fi++
							fileimpi[f, fi] = imp
						}
					}
					continue
				}
				if (ln ~ /^[ \t]*import[ \t]*\(/) {
					inb = 1
					continue
				}
				if (ln ~ /^[ \t]*import/ && match(ln, /"[^"]+"/)) {
					imp = substr(ln, RSTART + 1, RLENGTH - 2)
					if (!((f SUBSEP imp) in fileimp)) {
						fileimp[f SUBSEP imp] = 1
						fi++
						fileimpi[f, fi] = imp
					}
				}
			}
			close(f)
			cnt = 0
			fn = 0
			for (fi2 = 1; fi2 <= fi; fi2++) {
				imp = fileimpi[f, fi2]
				gr = rankof(imp, 1)
				if (gr < 0 || gr <= pint[ip]) {
					continue
				}
				fn++
				fimp[f, fn] = imp
				fimpg[f, fn] = RG
				if (!((f SUBSEP imp) in fset)) {
					fset[f SUBSEP imp] = 1
					cnt++
				}
			}
			if (cnt > 0) {
				fvioln[relf] = cnt
				fvn[relf] = fn
			}
		}
		close(cmd)
	}
	BEGIN {
		fatal = 0
		npkg = 0
		nint = 0
		mlen = length(module) + length("/internal/")
		grp["kit"] = 0
		grp["store"] = 1
		grp["session"] = 2
		grp["config"] = 3
		grp["agent"] = 4
		grp["app"] = 5
		grp["serve"] = 6
		inarr = ""
		curip = ""
		penddir = ""
		currank = -1
		curgrp = ""
		while ((getline line < prodexc) > 0) {
			sub(/\r$/, "", line)
			if (line == "" || line ~ /^#/) {
				continue
			}
			if (match(line, /[ \t]+[0-9]+$/)) {
				count = substr(line, RSTART, RLENGTH)
				sub(/^[ \t]+/, "", count)
				path = substr(line, 1, RSTART - 1)
			} else {
				printf "check_layers.sh: malformed exception line (expected \"<package-import-path> <count>\"): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path == "") {
				printf "check_layers.sh: malformed exception line (empty path): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path in pexc) {
				printf "check_layers.sh: duplicate exception entry for %s\n", path > "/dev/stderr"
				fatal = 1
				continue
			}
			pexc[path] = count + 0
		}
		close(prodexc)
		while ((getline line < testexc) > 0) {
			sub(/\r$/, "", line)
			if (line == "" || line ~ /^#/) {
				continue
			}
			if (match(line, /[ \t]+[0-9]+$/)) {
				count = substr(line, RSTART, RLENGTH)
				sub(/^[ \t]+/, "", count)
				path = substr(line, 1, RSTART - 1)
			} else {
				printf "check_layers.sh: malformed exception line (expected \"<repo-relative test file> <count>\"): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path == "") {
				printf "check_layers.sh: malformed exception line (empty path): %s\n", line > "/dev/stderr"
				fatal = 1
				continue
			}
			if (path in texc) {
				printf "check_layers.sh: duplicate exception entry for %s\n", path > "/dev/stderr"
				fatal = 1
				continue
			}
			texc[path] = count + 0
		}
		close(testexc)
	}
	fatal { exit 3 }
	{
		line = $0
		sub(/\r$/, "", line)
		if (inarr != "") {
			if (line ~ /^\t\],?$/) {
				inarr = ""
				next
			}
			if (line ~ /^\t\t"/) {
				addimp(inarr, el(line))
			}
			next
		}
		if (line ~ /^\t"Imports": \[/) {
			inarr = "p"
			next
		}
		if (line ~ /^\t"TestImports": \[/) {
			inarr = "t"
			next
		}
		if (line ~ /^\t"XTestImports": \[/) {
			inarr = "t"
			next
		}
		if (line ~ /^\t"ImportPath": /) {
			flush()
			curip = val(line)
			currank = rankof(curip, 0)
			curgrp = RG
			next
		}
		if (line ~ /^\t"Dir": /) {
			penddir = val(line)
			next
		}
		if (line ~ /^}/) {
			flush()
			next
		}
	}
	END {
		flush()
		if (fatal) {
			exit 3
		}
		vn = 0
		keptp = 0
		keptt = 0
		for (ip in seen) {
			if (pint[ip] < 0) {
				continue
			}
			pv = (ip in pcnt) ? pcnt[ip] : 0
			if (ip in pexc) {
				if (pv == 0) {
					vn++
					vnkey[vn] = ip
					vmsg[ip] = sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from %s; the list must hold only live debt", ip, prodexc)
				} else if (pv > pexc[ip]) {
					vn++
					vnkey[vn] = ip
					vmsg[ip] = sprintf("RATCHET VIOLATION: %s now violates with %d import(s) (recorded %d)", ip, pv, pexc[ip])
				} else {
					keptp += pv
				}
			} else if (pv > 0) {
				for (i = 1; i <= pn[ip]; i++) {
					vn++
					vnkey[vn] = ip SUBSEP pl[ip, i]
					vmsg[ip SUBSEP pl[ip, i]] = sprintf("LAYER VIOLATION: %s (group %s) imports %s (group %s)", ip, pgrp[ip], pl[ip, i], plg[ip, i])
				}
				vn++
				vnkey[vn] = ip SUBSEP "fix"
				vmsg[ip SUBSEP "fix"] = sprintf("fix the import, or record the CURRENT count in %s as \"%s %d\" with a one-line reason", prodexc, ip, pv)
			}
		}
		for (p in pexc) {
			if (!(p in seen)) {
				vn++
				vnkey[vn] = p
				vmsg[p] = sprintf("STALE EXCEPTION: %s no longer exists -- remove it from %s; the list must hold only live debt", p, prodexc)
			}
		}
		for (ip in seen) {
			if (pint[ip] >= 0 && (ip in tflag)) {
				scanpkgtests(ip)
			}
		}
		for (f in fvioln) {
			if (f in texc) {
				continue
			}
			for (i = 1; i <= fvn[f]; i++) {
				vn++
				vnkey[vn] = f SUBSEP fimp[f, i]
				vmsg[f SUBSEP fimp[f, i]] = sprintf("TEST LAYER VIOLATION: %s imports %s (group %s test may not import group %s)", f, fimp[f, i], fgrp[f], fimpg[f, i])
			}
			vn++
			vnkey[vn] = f SUBSEP "fix"
			vmsg[f SUBSEP "fix"] = sprintf("fix the test, or record it in %s as \"%s %d\"", testexc, f, fvioln[f])
		}
		for (f in texc) {
			if (f in seentestf) {
				c = (f in fvioln) ? fvioln[f] : 0
				if (c == 0) {
					vn++
					vnkey[vn] = f
					vmsg[f] = sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from %s; the list must hold only live debt", f, testexc)
				} else if (c > texc[f]) {
					vn++
					vnkey[vn] = f
					vmsg[f] = sprintf("RATCHET VIOLATION: %s now violates with %d import(s) (recorded %d)", f, c, texc[f])
				} else {
					keptt++
				}
				continue
			}
			i = length(f)
			while (i >= 1 && substr(f, i, 1) != "/") {
				i--
			}
			d = (i > 1) ? substr(f, 1, i - 1) : ""
			if (d in pkgdirseen) {
				vn++
				vnkey[vn] = f
				vmsg[f] = sprintf("RATCHET CLEANUP: %s no longer violates -- remove it from %s; the list must hold only live debt", f, testexc)
			} else {
				vn++
				vnkey[vn] = f
				vmsg[f] = sprintf("STALE EXCEPTION: %s no longer exists -- remove it from %s; the list must hold only live debt", f, testexc)
			}
		}
		# Insertion sort so the report is deterministic.
		for (i = 2; i <= vn; i++) {
			v = vnkey[i]
			j = i - 1
			while (j >= 1 && vnkey[j] > v) {
				vnkey[j + 1] = vnkey[j]
				j--
			}
			vnkey[j + 1] = v
		}
		for (i = 1; i <= vn; i++) {
			print vmsg[vnkey[i]]
		}
		chain = "kit < store < session < config < agent < app < serve"
		if (vn > 0) {
			printf "\033[31mlayer guard: %d violation(s) (%d package(s) checked, %d internal package(s) checked, %d production edge(s) allowlisted (kept), %d test file(s) allowlisted (kept); %s)\033[0m\n", vn, npkg, nint, keptp, keptt, chain
			exit 1
		}
		printf "\033[32mlayer guard: %d package(s) checked, %d internal package(s) checked, %d production edge(s) allowlisted (kept), %d test file(s) allowlisted (kept); %s\033[0m\n", npkg, nint, keptp, keptt, chain
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
	printf 'check_layers.sh: guard inputs are broken (see messages above); refusing to report a pass\n' >&2
	exit 3
fi
printf 'check_layers.sh: go list failed (exit %s); refusing to report a false pass\n' "$status" >&2
exit 1
