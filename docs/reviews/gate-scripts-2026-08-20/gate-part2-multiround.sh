#!/bin/bash
set -u
ROUNDS="${1:-15}"
N="${2:-6}"
for r in $(seq 1 "$ROUNDS"); do
  ~/gate-run/gate-part2-stress.sh "$r" "$N"
done
echo "ALL ROUNDS DONE"
