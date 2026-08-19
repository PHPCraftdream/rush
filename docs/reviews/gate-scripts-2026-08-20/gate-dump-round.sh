#!/bin/bash
set -u
D="$HOME/gate-run/p2-results/$1"
for f in "$D"/run-*.out; do
  echo "=== $f ==="
  cat "$f"
  echo
done
