#!/bin/bash
# Part 2: multi-process inject/drain stress test.
# Launches N real, separate `crush run` processes against the SAME session id,
# plus one `crush sessions inject`, and checks:
#   - exactly one run process actually "owns" the session and does work
#   - the rest get a clean busy-rejection (nonzero exit), not silent success
#   - no corrupted state
set -u

BIN="$HOME/gate-run/crush-gate"
WORK="$HOME/gate-run/p2-work"
export CRUSH_GLOBAL_DATA="$HOME/gate-run/p2-global-data"
export CRUSH_GLOBAL_CONFIG="$HOME/gate-run/p2-global-config"

ROUND="$1"
SESSION_ID="stress-session-$ROUND"
N="${2:-6}"
OUTDIR="$HOME/gate-run/p2-results/round-$ROUND"
mkdir -p "$OUTDIR"

rm -rf "$WORK/.crush/locks" 2>/dev/null

pids=()
for i in $(seq 1 "$N"); do
  (
    "$BIN" run --cwd "$WORK" --role fast --session "$SESSION_ID" --timeout 10s "message from runner $i" \
      > "$OUTDIR/run-$i.out" 2>&1
    echo $? > "$OUTDIR/run-$i.exit"
  ) &
  pids+=($!)
done

# Fire an inject roughly concurrently with the runs.
(
  sleep 0.3
  "$BIN" sessions inject "$SESSION_ID" --cwd "$WORK" -m "injected message" \
    > "$OUTDIR/inject.out" 2>&1
  echo $? > "$OUTDIR/inject.exit"
) &
pids+=($!)

for p in "${pids[@]}"; do
  wait "$p"
done

echo "ROUND $ROUND DONE" > "$OUTDIR/DONE"
