#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root. No installed binary or default daemon is used.
export GORTEX_AGENT_PROJECT_ROOT="$(pwd -P)"
artifact_dir="${GORTEX_AGENT_ARTIFACT_DIR:-$(mktemp -d /tmp/gortex-agent-transport.XXXXXX)}"
mkdir -p "$artifact_dir"
artifact_dir="$(cd "$artifact_dir" && pwd -P)"
export GORTEX_AGENT_TEST_BINARY="$artifact_dir/gortex"
export GORTEX_ISSUE767_ARTIFACT_DIR="$artifact_dir"
scenario="${GORTEX_AGENT_SCENARIO:-transport}"
case "$scenario" in
  transport) test_name=TestAgentTransportIntegration; default_race=1 ;;
  mixed) test_name=TestAgentMixedTransportIntegration; default_race=0 ;;
  *) printf 'Unknown GORTEX_AGENT_SCENARIO: %s\n' "$scenario" >&2; exit 2 ;;
esac
race="${GORTEX_AGENT_RACE:-$default_race}"
race_flags=()
case "$race" in
  0) ;;
  1) race_flags=(-race) ;;
  *) printf 'GORTEX_AGENT_RACE must be 0 or 1\n' >&2; exit 2 ;;
esac
printf 'Transport artifacts: %s (scenario=%s, race=%s)\n' "$artifact_dir" "$scenario" "$race"
go build "${race_flags[@]}" -o "$GORTEX_AGENT_TEST_BINARY" ./cmd/gortex
go test "${race_flags[@]}" -v -count=1 -timeout=20m ./cmd/gortex \
  -run "^${test_name}$" 2>&1 | tee "$artifact_dir/test.log"
