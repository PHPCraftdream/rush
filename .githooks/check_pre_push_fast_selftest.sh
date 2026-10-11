#!/usr/bin/env bash
# Fake tools only: no real Git repository, Go, PowerShell, or network.
# Optional arguments: candidate fast script, candidate dispatcher.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fast="${1:-$here/pre-push-fast.sh}"
hook="${2:-$here/pre-push}"
fast="$(cd "$(dirname "$fast")" && pwd)/$(basename "$fast")"
hook="$(cd "$(dirname "$hook")" && pwd)/$(basename "$hook")"
fixture="$(mktemp -d "$here/.fast-selftest.XXXXXX")"
fixture="$(cd "$fixture" && pwd)"
trap 'status=$?; if [ "$status" -ne 0 ]; then for log in "$fixture/out" "$fixture/trace"; do [ ! -f "$log" ] || sed -n "1,80p" "$log" >&2; done; fi; rm -rf "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/.githooks" "$fixture/pkg/a" "$fixture/pkg/b" "$fixture/pkg/empty"
for file in pkg/a/a.go pkg/a/again_test.go pkg/b/b.go pkg/a/unchanged.go; do
 printf 'package fixture\n' > "$fixture/$file"
done
cp "$fast" "$fixture/.githooks/pre-push-fast.sh"
cp "$here/pre-push-budgets.sh" "$fixture/.githooks/"
for helper in check_stale_model_slots.sh check_db_release_pairing.sh check_file_size.sh check_file_size_selftest.sh check_db_release_pairing_selftest.sh check_list_test_packages.sh check_run_test_segment_retry_flags.sh; do
 printf '#!/usr/bin/env bash\nprintf "helper %%s\\n" "${BASH_SOURCE[0]##*/}" >> "$TRACE"\n' > "$fixture/.githooks/$helper"
done
cat > "$fixture/bin/git" <<'EOF'
#!/usr/bin/env bash
printf 'git %s\n' "$*" >> "$TRACE"
case "$1" in
 rev-parse) printf '%s\n' "$FIXTURE" ;;
 cat-file) [ "$CASE" != fallback ] ;;
 merge-base)
  if [ "$CASE" = manual ]; then [ "$*" = 'merge-base HEAD origin/main' ] || exit 91
  else [ "$*" = 'merge-base TIP refs/remotes/upstream/main' ] || exit 92; fi
  printf 'BASE\n' ;;
 diff)
  [ "$1 $2 $3" = 'diff --name-only --no-renames' ] || exit 97
  if [ "$CASE" = union ] && [ "${*: -2}" = 'REMOTE2 TIP2' ]; then
   printf '%s\n' "$FILES2"
  else printf '%s\n' "$FILES"; fi ;;
 *) exit 93 ;;
esac
EOF
cat > "$fixture/bin/go" <<'EOF'
#!/usr/bin/env bash
printf 'go %s\n' "$*" >> "$TRACE"
case "$1" in
 env) [ "$*" = 'env GOHOSTOS' ] || exit 94; printf 'linux\n' ;;
 list)
  shift 4
  for dir in "$@"; do
   case "$dir" in ./pkg/a) printf 'fixture/a\n' ;; ./pkg/b) printf 'fixture/b\n' ;; ./pkg/empty) : ;; *) exit 95 ;; esac
  done ;;
 mod) [ "$*" = 'mod tidy -diff' ] ;;
 build) [ "$*" = 'build -p 1 ./...' ] ;;
 vet) [ "$*" = 'vet -p 1 fixture/a fixture/b' ] ;;
 test) [ "$*" = 'test -short -count=1 -p 1 -parallel 1 -run ^(TestNoUnhardenedProcessSpawns|TestSpawnGuardExemptionsAreCurrent)$ ./internal/platform/' ] ;;
 *) exit 96 ;;
esac
EOF
cat > "$fixture/bin/golangci-lint" <<'EOF'
#!/usr/bin/env bash
printf 'lint %s\n' "$*" >> "$TRACE"
expected=()
while IFS= read -r file; do expected+=("$file"); done < <(printf '%s\n' pkg/a/a.go pkg/a/again_test.go pkg/b/b.go | sort -u)
[ "$#" -eq 5 ] && [ "$1" = fmt ] && [ "$2" = --diff ] || exit 98
shift 2
for file in "${expected[@]}"; do [ "$1" = "$file" ] || exit 98; shift; done
case "${FMT_MODE:-clean}" in newline) printf '\n' ;; error) exit 99 ;; esac
EOF
cat > "$fixture/bin/powershell" <<'EOF'
#!/usr/bin/env bash
printf 'powershell %s\n' "$*" >> "$TRACE"
[ "${*: -1}" = '-SelfTest' ]
EOF
cat > "$fixture/bin/cygpath" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$2"
EOF
chmod +x "$fixture/bin/"*
export FIXTURE="$fixture" TRACE="$fixture/trace" LC_ALL=C
export PATH="$fixture/bin:/usr/bin:/bin"
unset RUSH_PREPUSH_FULL
expected_fmt="lint fmt --diff $(printf '%s\n' pkg/a/a.go pkg/a/again_test.go pkg/b/b.go | sort -u | paste -sd ' ' -)"
checks=0
check() {
 if "$@"; then checks=$((checks + 1)); else
  printf 'FAIL [%s]: %s\n' "$CASE" "$*" >&2
  [ ! -f "$TRACE" ] || sed -n '1,80p' "$TRACE" >&2
  exit 1
 fi
}
has() { grep -Fqx -- "$1" "$TRACE"; }
absent() { ! grep -Fq -- "$1" "$TRACE"; }
run_fast() {
 export CASE="$1" FILES="$2"
 : > "$TRACE"
 if [ "$CASE" = manual ]; then
  bash "$fixture/.githooks/pre-push-fast.sh" </dev/null > "$fixture/out" 2>&1
 elif [ "$CASE" = deletion ]; then
  printf 'refs/heads/topic 0000000000000000000000000000000000000000 refs/heads/topic REMOTE\n' | bash "$fixture/.githooks/pre-push-fast.sh" upstream url > "$fixture/out" 2>&1
 elif [ "$CASE" = union ]; then
  printf 'refs/heads/topic TIP refs/heads/topic REMOTE\nrefs/heads/other TIP2 refs/heads/other REMOTE2\n' | bash "$fixture/.githooks/pre-push-fast.sh" upstream url > "$fixture/out" 2>&1
 else
  printf 'refs/heads/topic TIP refs/heads/topic REMOTE\n' | bash "$fixture/.githooks/pre-push-fast.sh" upstream url > "$fixture/out" 2>&1
 fi
 check has 'go test -short -count=1 -p 1 -parallel 1 -run ^(TestNoUnhardenedProcessSpawns|TestSpawnGuardExemptionsAreCurrent)$ ./internal/platform/'
 check has 'go mod tidy -diff'
 check has 'go build -p 1 ./...'
 if [ "$CASE" = range ] || [ "$CASE" = union ]; then
  check has "$expected_fmt"
  check test "$(grep -c '^lint ' "$TRACE")" -eq 1
 else
  check grep -Fxq 'no Go files changed' "$fixture/out"
  check absent 'lint '
  check bash -c '! grep -Fq "formatting SKIPPED" "$FIXTURE/out"'
 fi
}
run_fast range $'pkg/b/b.go\npkg/a/a.go\npkg/a/again_test.go\npkg/b/b.go\npkg/deleted/gone.go\npkg/empty/gone.go'
check has 'git diff --name-only --no-renames REMOTE TIP'
check has "go list -e -f {{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}} ./pkg/a ./pkg/b ./pkg/empty"
check has 'go vet -p 1 fixture/a fixture/b'
check absent './pkg/deleted'
check absent '-SelfTest'
export FILES2=$'pkg/a/again_test.go\npkg/b/b.go\npkg/empty/gone.go'
run_fast union $'pkg/b/b.go\npkg/a/a.go\npkg/deleted/gone.go'
check has 'git diff --name-only --no-renames REMOTE TIP'
check has 'git diff --name-only --no-renames REMOTE2 TIP2'
check has 'go vet -p 1 fixture/a fixture/b'
export FILES=$'pkg/b/b.go\npkg/a/a.go\npkg/a/again_test.go'
for mode in newline error; do
 export FMT_MODE="$mode" CASE=range
 : > "$TRACE"
 if printf 'refs/heads/topic TIP refs/heads/topic REMOTE\n' | bash "$fixture/.githooks/pre-push-fast.sh" upstream url > "$fixture/out" 2>&1; then
  printf 'FAIL: formatter %s accepted\n' "$mode" >&2; exit 1
 fi
 check has "$expected_fmt"
 check absent 'go build'
done
unset FMT_MODE
run_fast fallback README.md
check has 'git merge-base TIP refs/remotes/upstream/main'
check has 'git diff --name-only --no-renames BASE TIP'
check grep -Fxq 'no Go packages changed' "$fixture/out"
check absent 'go list'
check absent 'go vet'
run_fast manual README.md
check has 'git merge-base HEAD origin/main'
check has 'git diff --name-only --no-renames BASE HEAD'
run_fast deletion pkg/a/a.go
check absent 'git diff'
check absent 'git merge-base'
check absent 'go list'
check grep -Fxq 'no Go packages changed' "$fixture/out"
for config in .githooks/pre-push-fast.sh .golangci.yml; do
 run_fast config "$config"
 check has 'helper check_file_size_selftest.sh'
 check has 'helper check_db_release_pairing_selftest.sh'
 check has 'helper check_list_test_packages.sh'
 check has 'helper check_run_test_segment_retry_flags.sh'
 check grep -Fq -- '-SelfTest' "$TRACE"
done
# Extract only through the dispatcher. Never execute the real full mirror.
# Fail closed if the known full-mode boundary disappears.
awk '/^fail\(\)/ {found=1; exit} {print} END {if (!found) exit 1}' "$hook" > "$fixture/.githooks/dispatch"
printf 'printf "FULL_MARKER\\n"\n' >> "$fixture/.githooks/dispatch"
printf '#!/usr/bin/env bash\nprintf "FAST_MARKER\\n"\n' > "$fixture/.githooks/pre-push-fast.sh"
CASE=dispatch
check bash -c 'bash "$FIXTURE/.githooks/dispatch" </dev/null > "$FIXTURE/dispatch.out"'
check grep -Fxq FULL_MARKER "$fixture/dispatch.out"
check bash -c 'bash "$FIXTURE/.githooks/dispatch" upstream url </dev/null > "$FIXTURE/dispatch.out"'
check grep -Fxq FAST_MARKER "$fixture/dispatch.out"
check bash -c 'RUSH_PREPUSH_FULL=1 bash "$FIXTURE/.githooks/dispatch" upstream url </dev/null > "$FIXTURE/dispatch.out"'
check grep -Fxq FULL_MARKER "$fixture/dispatch.out"
printf 'PASS: %s assertions; 7 fast cases, 2 formatter failures, 3 safe dispatcher cases; fake tools only\n' "$checks"
