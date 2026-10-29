#!/usr/bin/env bash
# Run the tests of the packages a change can affect: every package holding a
# changed Go file, plus every package in this module that imports one of them.
# Results come from the Go test cache wherever nothing changed, so a second run
# over the same tree costs seconds. The full suite stays the pre-push gate.
#
# Usage: scripts/test-changed.sh [base-ref] [go test flags...]
# BASE defaults to origin/main; uncommitted and untracked files count as changed.
# LIST=1 prints the affected packages without running them.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"
base=${1:-origin/main}
[ $# -gt 0 ] && shift

module=$(go list -m)
changed_dirs=$(
  {
    git diff --name-only "$base" -- '*.go'
    git ls-files --others --exclude-standard -- '*.go'
  } | xargs -r -n1 dirname | sort -u
)
if [ -z "$changed_dirs" ]; then
  echo "No Go files changed since $base."
  exit 0
fi

changed_pkgs=$(
  for dir in $changed_dirs; do
    [ -d "$dir" ] && go list "./$dir" 2>/dev/null || true
  done | sort -u
)
if [ -z "$changed_pkgs" ]; then
  echo "Changed Go files belong to no buildable package."
  exit 0
fi

# Reverse dependencies inside this module: a package is affected when it is
# changed or imports a changed package, directly or transitively.
affected=$(
  go list -f '{{.ImportPath}} {{join .Deps " "}}' ./... |
    awk -v changed="$(echo "$changed_pkgs" | tr '\n' ' ')" -v module="$module" '
      BEGIN { n = split(changed, list, " "); for (i = 1; i <= n; i++) want[list[i]] = 1 }
      index($1, module) == 1 {
        for (i = 1; i <= NF; i++) if ($i in want) { print $1; break }
      }'
)

count=$(echo "$affected" | wc -l)
if [ "${LIST:-}" = 1 ]; then
  echo "$affected"
  exit 0
fi
echo "Testing $count affected package(s) since $base."
# shellcheck disable=SC2086
go test -race -timeout=45m "$@" $affected
