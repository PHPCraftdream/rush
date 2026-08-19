#!/bin/bash
set -u
cd ~/gate
for i in $(seq 1 10); do
  $HOME/sdk/go/bin/go test ./internal/session/... -run TestP1_1_WatchdogCancelsAtTTLMinusMargin -count=1 -v 2>&1 | grep -E "^--- (PASS|FAIL)"
done
