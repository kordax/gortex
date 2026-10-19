#!/bin/sh
# Repeated synthetic benchmarks and optional isolated multi-agent transport load.
set -eu
repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
exec python3 "$repo_dir/bench/performance/run.py" "$@"
