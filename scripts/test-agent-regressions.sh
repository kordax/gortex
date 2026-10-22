#!/usr/bin/env bash
set -euo pipefail

# Repeat deterministic fault and lifecycle cases without touching an installed
# daemon. Print the shuffle seed so a failed ordering can be reproduced.
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

repeat="${GORTEX_AGENT_REGRESSION_REPEAT:-5}"
seed="${GORTEX_AGENT_REGRESSION_SEED:-$(date +%s)}"
if [[ ! "$repeat" =~ ^[1-9][0-9]?$ ]]; then
  printf 'GORTEX_AGENT_REGRESSION_REPEAT must be an integer from 1 to 99.\n' >&2
  exit 2
fi
if [[ ! "$seed" =~ ^[0-9]{1,10}$ ]]; then
  printf 'GORTEX_AGENT_REGRESSION_SEED must contain 1 to 10 decimal digits.\n' >&2
  exit 2
fi

artifacts="${GORTEX_AGENT_REGRESSION_ARTIFACT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/gortex-agent-regressions.XXXXXX")}"
mkdir -p -- "$artifacts"
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.3}"
printf 'Agent regression tests: race detector, %s repetitions, shuffle seed %s\n' "$repeat" "$seed"
printf 'Artifacts: %s\n' "$artifacts"
printf 'Running startup admission, batch crash/retry and branch/worktree lifecycle cases...\n'
pattern='^Test(Dispatcher_Admission|AtomicBatch(RecoveryStateMatrix|ProcessCrashRecovery|CancelledRetriesDoNotCancelOwner)$|DedicatedSelector(SessionSurvives|StaysExact))'
go test -race -shuffle="$seed" -count="$repeat" -timeout=10m \
  ./cmd/gortex ./internal/mcp -run "$pattern" 2>&1 | tee "$artifacts/tests.log"
printf 'Agent regression tests passed. Reproduce with GORTEX_AGENT_REGRESSION_SEED=%s GORTEX_AGENT_REGRESSION_REPEAT=%s bash scripts/test-agent-regressions.sh\n' "$seed" "$repeat"
