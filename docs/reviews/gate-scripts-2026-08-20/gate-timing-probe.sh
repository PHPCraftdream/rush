#!/bin/bash
set -u
export CRUSH_GLOBAL_DATA="$HOME/gate-run/p3c-global-data"
export CRUSH_GLOBAL_CONFIG="$HOME/gate-run/p3c-global-config"
BIN="$HOME/gate-run/crush-gate"
WORK="$HOME/gate-run/p3c-work"

for n in 1 2 3 4 5; do
  t0=$(date +%s%N)
  "$BIN" sessions inject seed2 --cwd "$WORK" -m "probe $n" >/dev/null 2>&1
  t1=$(date +%s%N)
  echo "probe $n: $(( (t1-t0)/1000000 ))ms"
done
