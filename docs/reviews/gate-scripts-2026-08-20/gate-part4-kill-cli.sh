#!/bin/bash
# Part 4 (CLI, black-box): start a real `crush run` that will hang for a
# while (long --timeout, fake provider hangs on connect-refused + would
# retry), then verify `crush sessions kill <id>` from a SEPARATE process
# actually frees the lock, and a follow-up `crush run` on the same session
# id can then proceed without the P0 #594 regression (new owner harmed by
# a stale sweep).
set -u

BIN="$HOME/gate-run/crush-gate"
WORK="$HOME/gate-run/p4-work"
export CRUSH_GLOBAL_DATA="$HOME/gate-run/p4-global-data"
export CRUSH_GLOBAL_CONFIG="$HOME/gate-run/p4-global-config"
mkdir -p "$WORK"
SESSION_ID="p4-kill-session"
OUT="$HOME/gate-run/p4-results"
mkdir -p "$OUT"

# Start a long-running holder (long timeout so it stays "live" for us to kill).
"$BIN" run --cwd "$WORK" --role fast --session "$SESSION_ID" --timeout 60s "hold the lock" \
  > "$OUT/holder.out" 2>&1 &
HOLDER_PID=$!

# Give it time to acquire the session lock (config load + DB open + lock file write).
sleep 2

# Confirm the lock is actually held before we try to kill it.
"$BIN" sessions locks --cwd "$WORK" > "$OUT/locks-before.out" 2>&1

# Kill it via the real subcommand from a SEPARATE process (not our own PID).
"$BIN" sessions kill "$SESSION_ID" --cwd "$WORK" --wait 10s > "$OUT/kill.out" 2>&1
echo $? > "$OUT/kill.exit"

# The holder process itself should now be gone (killed by `sessions kill`,
# not by us) -- confirm via wait.
wait "$HOLDER_PID" 2>/dev/null
echo $? > "$OUT/holder.exit"

# Confirm the lock is now free.
"$BIN" sessions locks --cwd "$WORK" > "$OUT/locks-after.out" 2>&1

# A brand new owner should be able to acquire the session immediately,
# unharmed by any stale-generation sweep (P0 #594).
"$BIN" run --cwd "$WORK" --role fast --session "$SESSION_ID" --timeout 10s "new owner after kill" \
  > "$OUT/new-owner.out" 2>&1
echo $? > "$OUT/new-owner.exit"

echo "PART4 CLI KILL TEST DONE" > "$OUT/DONE"
