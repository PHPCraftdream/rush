#!/bin/bash
# Part 3b: SIGKILL `crush sessions inject` mid-write, repeatedly, against a
# pre-existing session. Checks for split-brain: a user message created
# without its corresponding pending_inject row (or vice versa).
set -u

BIN="$HOME/gate-run/crush-gate"
WORK="$HOME/gate-run/p3b-work"
export CRUSH_GLOBAL_DATA="$HOME/gate-run/p3b-global-data"
export CRUSH_GLOBAL_CONFIG="$HOME/gate-run/p3b-global-config"
mkdir -p "$WORK"

SESSION_ID="inject-kill-session"
ROUNDS="${1:-30}"
OUTDIR="$HOME/gate-run/p3b-results"
mkdir -p "$OUTDIR"
: > "$OUTDIR/summary.log"

# Pre-create the session with a plain (non-provider-touching) command: use
# `sessions inject` itself once, uninterrupted, to seed the session (inject
# creates the session? no -- it needs an existing session). Use a quick
# --timeout 1s run that will fail fast against the session id to create it.
"$BIN" run --cwd "$WORK" --role fast --session "$SESSION_ID" --timeout 3s "seed" \
  > "$OUTDIR/seed.out" 2>&1

for r in $(seq 1 "$ROUNDS"); do
  "$BIN" sessions inject "$SESSION_ID" --cwd "$WORK" -m "inject round $r" \
    > "$OUTDIR/inject-$r.out" 2>&1 &
  IPID=$!
  # Calibration: an uninterrupted `sessions inject` in this WSL2 environment
  # takes ~3.0-3.2s wall-clock end to end (mostly I/O wait from process
  # spawn + config/DB open on WSL2, not the writes themselves). Spread kills
  # across the whole lifecycle so some land near the two sequential writes
  # (messages.Create then CreatePendingInject) instead of only during startup.
  # Second calibration round (5 uninterrupted probes) measured 1.6-3.2s
  # end-to-end, clustering near 3.1-3.2s -- almost all of it fixed per-process
  # overhead (config discovery file walk), so the actual DB write is likely
  # in the final few hundred ms. Concentrate kills in that tail window.
  ms=$(( (RANDOM % 700) + 2700 ))
  echo "round $r chosen_delay_ms=$ms ipid=$IPID" >> "$OUTDIR/timing.log"
  perl -e "select(undef,undef,undef,0.001*$ms)" 2>/dev/null || sleep 3
  kill -9 "$IPID" 2>/dev/null
  wait "$IPID" 2>/dev/null
  ks=$?
  echo "round $r killstatus=$ks" >> "$OUTDIR/summary.log"
done

echo "INJECT KILL ROUNDS DONE" >> "$OUTDIR/summary.log"
