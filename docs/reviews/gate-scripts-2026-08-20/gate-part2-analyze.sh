#!/bin/bash
set -u
BASE="$HOME/gate-run/p2-results"
for d in "$BASE"/round-*; do
  r=$(basename "$d")
  total=0
  winners=0
  losers=0
  other=0
  for f in "$d"/run-*.out; do
    total=$((total+1))
    if grep -q "already running in crush process" "$f" 2>/dev/null; then
      losers=$((losers+1))
    elif grep -q "connection refused\|context deadline exceeded\|Agent processing failed" "$f" 2>/dev/null; then
      winners=$((winners+1))
    else
      other=$((other+1))
    fi
  done
  echo "$r: total=$total winners=$winners losers=$losers other=$other"
done
