#!/bin/bash
# Part 3: repeated SIGKILL mid-run against the same session, then verify
# the next `crush run` sees consistent state (never half-written).
set -u

BIN="$HOME/gate-run/crush-gate"
WORK="$HOME/gate-run/p3-work"
export CRUSH_GLOBAL_DATA="$HOME/gate-run/p3-global-data"
export CRUSH_GLOBAL_CONFIG="$HOME/gate-run/p3-global-config"

mkdir -p "$WORK"
SESSION_ID="sigkill-session"
ROUNDS="${1:-20}"
OUTDIR="$HOME/gate-run/p3-results"
mkdir -p "$OUTDIR"
: > "$OUTDIR/summary.log"

for r in $(seq 1 "$ROUNDS"); do
  # Vary the kill delay to hit different points in the run's lifecycle
  # (startup/session-create, provider dial, retry backoff, shutdown).
  delay="0.$((RANDOM % 9 + 1))"

  "$BIN" run --cwd "$WORK" --role fast --session "$SESSION_ID" --timeout 30s "round $r message" \
    > "$OUTDIR/round-$r.out" 2>&1 &
  RUNPID=$!

  sleep "$delay"
  kill -9 "$RUNPID" 2>/dev/null
  wait "$RUNPID" 2>/dev/null
  killstatus=$?

  # Also try to kill any lingering child processes this run may have spawned
  # (defensive — fake provider means no CLI subprocess children expected here).

  echo "round $r delay=$delay killstatus=$killstatus" >> "$OUTDIR/summary.log"
done

echo "SIGKILL ROUNDS DONE" >> "$OUTDIR/summary.log"
