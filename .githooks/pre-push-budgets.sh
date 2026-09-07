#!/usr/bin/env bash
# Per-gate resource budgets for .githooks/pre-push.
#
# The timeout values are intentionally command-specific. Build and lint may
# populate a cold Go/module cache; tests run after that work and use smaller
# budgets. The memory values match the -p/-parallel throttles documented in
# pre-push and are enforced on Windows by run_capped.ps1.

RUSH_BUILD_MEMORY="3g"
RUSH_BUILD_TIMEOUT=600
RUSH_LINT_MEMORY="3g"
RUSH_LINT_TIMEOUT=900
RUSH_AGENT_MEMORY="2g"
RUSH_AGENT_TIMEOUT=600
RUSH_REMAINDER_MEMORY="3g"
RUSH_REMAINDER_TIMEOUT=900
RUSH_PROCESS_MEMORY="2g"
RUSH_PROCESS_TIMEOUT=300
