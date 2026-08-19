#!/usr/bin/env bash
# check_stale_model_slots.sh: guard against the old "large"/"small" model
# slot names reappearing in user-facing text after the large/small ->
# smart/fast rename.
#
# Background: the rename was done with a go/scanner tool that patched only
# Go IDENT tokens (deliberately -- this codebase's prose is full of
# legitimate English like "a large thinking budget" or "a small side LLM
# call", which a blind text rename would have mangled). Everything that
# is NOT a Go identifier -- CLI help/usage text, error message strings,
# --flag names, bundled skill markdown, agent-tool .md descriptions,
# config-key documentation, and the web UI's TypeScript -- was swept by
# hand, and the sweep followed the compiler and specific fixtures rather
# than being exhaustive. Two independent reviews both found live
# instances of "models.large", "--small", "smart | large" role tables,
# and a `config.models.small` access in web/src/store.ts still shipping
# months after the rename landed. Nothing else in the toolchain would
# have caught any of that: it is not a compile error, not a vet finding,
# and go test/pnpm typecheck don't read --help text or notice a field
# named "small" is simply not read by TS's structural typing.
#
# v1 of this guard (see git blame) matched only --large/--small,
# models.large/models.small, and quoted "large"/"small". A coordinator
# review ran it against the unfixed tree and found it missed three of
# the six files the fixing task named -- run.go's "smart | large" /
# "fast | small" role table, models_unset.go's "[large|small|both]" /
# "large + small" / "sub-agents use large" prose, and ping.go's
# "small/fast slot" / "large/smart slot" -- because none of those use a
# quote or a "--" prefix. That gap is fixed below by ALSO matching
# large/small directly adjacent (joined only by |, /, +, a comma-list, or
# the word "use") to smart/fast/worker/reviewer -- see ADJACENT below.
#
# Scope: this looks for SLOT-NAME usages of "large"/"small" via two
# tiers:
#   1. STRICT: a bare quoted "large"/"small" (JSON key or CLI-error
#      token), the "models.large"/"models.small" dotted config path (also
#      matching TypeScript's optional-chain spelling "models?.large" /
#      "models?.small", which is how web/src/store.ts's
#      `config?.models?.small` access -- the exact defect a coordinator
#      review found this guard's file globs couldn't even see, since it
#      didn't scan *.ts at all -- is written), and the "--large"/"--small"
#      CLI flag spellings. Word-bounded (\>) on the dotted-path forms so
#      "models.smallModelOverride" (a hypothetical future identifier that
#      merely starts with "small") would not false-positive; there is no
#      "models.large"/"models.small"-prefixed identifier in this repo
#      today, but the boundary costs nothing and the bare `models\.small`
#      form (no \>) was written first and only caught during review.
#   2. ADJACENT: large/small directly joined -- by |, /, +, a comma list,
#      or immediately after the word "use" or "unset" -- to one of
#      smart/fast/worker/reviewer. This catches role-table and enum-style
#      prose ("smart | large", "[large|small|both]", "large + small",
#      "sub-agents use large", "large/worker/reviewer untouched", "crush
#      models unset small --global") WITHOUT matching ordinary sentences
#      that merely mention both concepts with other words between them --
#      adjacency is the signal, not co-occurrence anywhere on the line.
#      A same-line "any vocabulary word near large/small" rule was tried
#      and rejected during review: smart/fast/worker/reviewer are common
#      enough words (39 files in internal/cmd alone) that "the smart
#      approach... buffer large enough" or "a fast retry... small
#      backoff" trip it while never mentioning a slot at all. Requiring
#      direct adjacency (no words between) keeps the real cases matched
#      and those sentences excluded -- verified against both corpora in
#      the commit that added this guard.
#
#      Residual risk, disclosed rather than hidden: the large/worker,
#      worker/small, etc. adjacency forms (added to catch
#      "large/worker/reviewer untouched" in models_use.go) can in
#      principle false-positive on a contrived sentence that literally
#      writes "worker/small" or "large/worker" as a slash-joined pair in
#      unrelated prose (e.g. describing a "worker/small queue" split) --
#      tested and confirmed possible, but such phrasing is not standard
#      English (real prose says "worker or small queue", not
#      "worker/small queue") and no instance of it exists anywhere in
#      this repository as of the commit that added this rule. If one
#      ever does appear and trips the guard, that is the guard doing its
#      job on genuinely ambiguous text -- fix the wording or extend the
#      allow-list, whichever is true.
#
# What this does NOT catch (know the gaps, honestly):
#   - Free-form prose with words between large/small and any slot name
#     ("the large model", "a large slot", "smart today, large tomorrow",
#     "Max thinking on large (1M ctx), fast on small" -- large and small
#     are on the same line but not adjacent to each other or to a slot
#     word). Generalizing the adjacency check to same-line co-occurrence
#     was tried and produces false positives on ordinary engineering
#     prose (see above) -- it was deliberately not shipped.
#   - "large (" / "small (" as a parenthetical-clarification opener (e.g.
#     `Set only the large ("smart") slot`). This form was tried too
#     (`large[[:space:]]*\(`) and rejected: it also matches completely
#     ordinary English -- "a large (but manageable) dataset", "small
#     (under 200 bytes)", "keep the diff small (a few lines)" -- which a
#     one-line grep cannot distinguish from the stale-slot case without
#     understanding the parenthetical's contents.
#     Both of the above are real, accepted gaps: a sufficiently
#     roundabout stale sentence can still slip through. Prose drift is a
#     code-review concern, not something a byte-pattern guard can safely
#     generalize to further without reintroducing exactly the
#     false-positive problem the original go/scanner rename tool was
#     built to avoid.
#   - Any file type outside *.go *.md *.tpl *.txt *.ts *.tsx.
#   - Anything inside an excluded path (see below) -- including a
#     mistake inside web/dist/** (build output, regenerated from source
#     so a hand-fix there wouldn't stick anyway) or inside
#     internal/config/load_files_unknown_slots_test.go (the one
#     allow-listed test fixture, see below).
#
# Excluded from scanning (allow-list, both by directory and by file):
#   - .git/, node_modules/, **/node_modules/, web/dist/**  -- generated/vendored/build output
#   - internal/config/load_files_unknown_slots_test.go     -- the ONE
#     test file allow-listed by exact path, not by a blanket *_test.go
#     rule. It legitimately constructs old-format input
#     ({"models":{"large":...,"small":...}}) to prove the unknown-slot
#     warning (load_files.go) names the offending key -- see the test
#     itself. Every OTHER *_test.go in the repo is scanned like any
#     other file: a test asserting on stale --help/error text is exactly
#     the kind of regression this guard exists to catch, so blanket-
#     exempting all tests (v1's mistake) would have hidden that.
#   - CHANGELOG.md, CHANGELOG.fork.md                      -- historical
#     record of what old commands/flags/columns were called before they
#     were removed; rewriting history here would make the changelog wrong.
#   - docs/checkpoints/**, docs/reviews/**, docs/plans/**  -- dated session
#     snapshots and planning docs describing past states of the repo, not
#     current behavior.
#   - internal/cmd/models_set.go                           -- the hidden
#     `crush models set` redirect stub; its one "large"/"small" mention is
#     a Go comment explaining why DisableFlagParsing exists (a legacy
#     caller may still pass --large/--small), not a live claim that those
#     flags work today. The command's actual output tells the user to use
#     `crush models use` instead.
#   - README.md's one documented "Removed in batch 11" note -- explicitly
#     labeled historical, describing the exact old command spelling that
#     was removed. (Not excluded by path -- excluded by requiring an
#     un-labeled match; see the label check below.)
#
# web/** is NOT excluded (v1's mistake -- excluded only because another
# concurrent agent owned that tree for the duration of one review round;
# that was a scheduling fact, not a permanent property of this guard).
# *.ts/*.tsx are scanned so `config?.models?.small` / `config.models.small`
# in web/src/store.ts is caught by the STRICT models\??\.small\> pattern.
#
# Implementation note: uses `git grep` (one process against git's index)
# rather than a per-file loop -- a per-file shell loop over every tracked
# file in this repo (2500+ files) measured well over a minute on this
# environment; `git grep` with the same pathspecs does the equivalent
# search in well under a second.
#
# Pattern: plain ASCII literal alternation plus POSIX character classes
# ([[:space:]]) for whitespace -- see the .sql ASCII guard in
# .githooks/pre-push for why hand-built character classes are dangerous in
# general (a POSIX class can silently match nothing, and hex-escaped byte
# ranges can turn into real control bytes if built by a lossy text
# pipeline). The one place this pattern DOES use a backslash escape is
# `\>` (GNU grep's word-boundary end-anchor, used in the "use large" /
# "unset small" forms below) -- and that escape was itself caught by the
# same "run it against a tree that should fail" discipline this comment
# preaches: an earlier version wrote a bare `>` (no backslash), which in
# POSIX ERE is just a literal greater-than character, not a boundary --
# so `use[[:space:]]+large>` silently matched nothing (it would need a
# literal ">" after the word "large", which never occurs), and
# "sub-agents use large)." in models_unset.go's pre-fix text sailed
# through undetected until a full per-file replay against da10303f caught
# the gap. `\>` (and `\<` if ever needed for a start-anchor) is the
# correct, deliberate escape here -- verified against both the violating
# and the false-positive corpus after the fix, see the commit that
# widened this guard.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

STRICT='(--large|--small|models\??\.large\>|models\??\.small\>|"large"|"small")'
ADJACENT='(smart[[:space:]]*[|/][[:space:]]*large|large[[:space:]]*[|/][[:space:]]*smart|fast[[:space:]]*[|/][[:space:]]*small|small[[:space:]]*[|/][[:space:]]*fast|large[[:space:]]*[+|/][[:space:]]*small|small[[:space:]]*[+|/][[:space:]]*large|large[[:space:]]*/[[:space:]]*worker|worker[[:space:]]*/[[:space:]]*large|small[[:space:]]*/[[:space:]]*worker|worker[[:space:]]*/[[:space:]]*small|large[[:space:]]*/[[:space:]]*reviewer|reviewer[[:space:]]*/[[:space:]]*large|small[[:space:]]*/[[:space:]]*reviewer|reviewer[[:space:]]*/[[:space:]]*small|large[,[:space:]]+small[,[:space:]]+worker[,[:space:]]+reviewer|use[[:space:]]+large\>|use[[:space:]]+small\>|unset[[:space:]]+large\>|unset[[:space:]]+small\>)'
PATTERN="${STRICT}|${ADJACENT}"

matches="$(git grep -n -E "$PATTERN" -- \
	'*.go' '*.md' '*.tpl' '*.txt' '*.ts' '*.tsx' \
	':!:web/dist/**' \
	':!:node_modules/**' ':!:**/node_modules/**' \
	':!:CHANGELOG.md' ':!:CHANGELOG.fork.md' \
	':!:docs/checkpoints/**' ':!:docs/reviews/**' ':!:docs/plans/**' \
	':!:internal/cmd/models_set.go' \
	':!:internal/config/load_files_unknown_slots_test.go' \
	2>/dev/null)"

offenders=""
if [ -n "$matches" ]; then
	while IFS= read -r line; do
		[ -z "$line" ] && continue
		file="${line%%:*}"
		rest="${line#*:}"
		lineno="${rest%%:*}"
		# An in-place opt-out, for text that must SPELL the old names to
		# say something true about them: a regression test explaining the
		# bug it pins, or a doc noting what a removed flag used to be
		# called. Put the marker on the offending line or on either of the
		# two lines above it. It is deliberately per-line, not per-file:
		# a blanket path exclusion would also hide a genuine
		# reintroduction sitting three lines further down in the same file.
		#
		# This replaced a bespoke README.md-only check for one specific
		# blockquote label. The general form covers that note (which now
		# carries the marker) and the web regression tests from task #570,
		# whose comments quote `config?.models?.small` precisely because
		# reading that key was the bug.
		context="$(sed -n "$((lineno > 2 ? lineno - 2 : 1)),${lineno}p" "$file" 2>/dev/null)"
		if printf '%s' "$context" | grep -q 'stale-slot-ok'; then
			continue
		fi
		offenders="${offenders}${line}"$'\n'
	done <<<"$matches"
fi

if [ -n "$offenders" ]; then
	printf '\033[31mstale large/small model-slot spelling found (renamed to smart/fast):\033[0m\n'
	printf '%s' "$offenders"
	printf '\033[33mIf this is genuinely historical text (changelog/checkpoint/plan) or a test fixture proving old-format detection, extend the allow-list in .githooks/check_stale_model_slots.sh with a one-line reason -- do not just delete the match.\033[0m\n'
	exit 1
fi

exit 0
