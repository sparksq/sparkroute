#!/usr/bin/env bash

set -euo pipefail

mode="smoke"
benchtime="2s"
count="5"
output="-"
procs=""

usage() {
  echo "usage: $0 [--mode smoke|benchmark] [--benchtime DURATION] [--count N] [--procs N] [--output PATH|-]"
}

while (($# > 0)); do
  case "$1" in
    --mode) mode="$2"; shift 2 ;;
    --benchtime) benchtime="$2"; shift 2 ;;
    --count) count="$2"; shift 2 ;;
    --procs) procs="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$mode" != "smoke" && "$mode" != "benchmark" ]]; then
  echo "--mode must be smoke or benchmark" >&2
  exit 2
fi
if [[ ! "$count" =~ ^[1-9][0-9]*$ ]]; then
  echo "--count must be a positive integer" >&2
  exit 2
fi
if [[ -n "$procs" && ! "$procs" =~ ^[1-9][0-9]*$ ]]; then
  echo "--procs must be a positive integer" >&2
  exit 2
fi

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repository"

go_command="${GO_COMMAND:-go}"
packages=(
  ./pkg/lifecycle
  ./pkg/modelcatalog
  ./pkg/savedtrace
  ./pkg/savedtrace/filesystem
  ./pkg/ledger/sqlite
)

if [[ "$mode" == "smoke" ]]; then
  "$go_command" test -race -count=1 "${packages[@]}"
  exit 0
fi

emit_benchmarks() {
  echo "# sparkroute regression benchmark matrix"
  echo "# revision: $(git rev-parse HEAD)"
  echo "# worktree_dirty: $(if git diff --quiet && git diff --cached --quiet; then echo false; else echo true; fi)"
  echo "# timestamp_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# go: $("$go_command" version)"
  echo "# uname: $(uname -a)"
  echo "# gomaxprocs: ${procs:-runtime-default}"
  if [[ -n "$procs" ]]; then
    GOMAXPROCS="$procs" "$go_command" test -run '^$' \
      -bench '^BenchmarkRegressionMatrix' -benchmem \
      -benchtime "$benchtime" -count "$count" "${packages[@]}"
  else
    "$go_command" test -run '^$' \
      -bench '^BenchmarkRegressionMatrix' -benchmem \
      -benchtime "$benchtime" -count "$count" "${packages[@]}"
  fi
}

if [[ "$output" == "-" ]]; then
  emit_benchmarks
else
  if [[ ! -d "$(dirname "$output")" ]]; then
    echo "output parent directory does not exist: $(dirname "$output")" >&2
    exit 2
  fi
  emit_benchmarks | tee "$output"
fi
