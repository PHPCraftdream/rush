#!/bin/bash
set -u
D="$HOME/gate-run/p3b-results"
for i in 1 5 10 15 20 25 30; do
  echo "=== inject-$i ==="
  cat "$D/inject-$i.out" 2>&1
  echo
done
