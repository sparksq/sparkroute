#!/usr/bin/env bash

set -euo pipefail

candidate=""
target_url=""
scenario="unary"
concurrency="1"
duration="30s"
warmup="2s"
repeat="3"
output_dir=""
process_pid=""
profile_url=""
body_file=""
expect=""
headers=()

usage() {
  echo "usage: $0 --candidate NAME --url URL --output-dir DIR [--scenario unary|stream] [--concurrency N] [--duration D] [--warmup D] [--repeat N] [--process-pid PID] [--pprof-url URL] [--body-file PATH] [--expect TEXT] [--header 'Name: value']"
}

while (($# > 0)); do
  case "$1" in
    --candidate) candidate="$2"; shift 2 ;;
    --url) target_url="$2"; shift 2 ;;
    --scenario) scenario="$2"; shift 2 ;;
    --concurrency) concurrency="$2"; shift 2 ;;
    --duration) duration="$2"; shift 2 ;;
    --warmup) warmup="$2"; shift 2 ;;
    --repeat) repeat="$2"; shift 2 ;;
    --output-dir) output_dir="$2"; shift 2 ;;
    --process-pid) process_pid="$2"; shift 2 ;;
    --pprof-url) profile_url="$2"; shift 2 ;;
    --body-file) body_file="$2"; shift 2 ;;
    --expect) expect="$2"; shift 2 ;;
    --header) headers+=("$2"); shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$candidate" || -z "$target_url" || -z "$output_dir" ]]; then
  usage >&2
  exit 2
fi
if [[ "$scenario" != "unary" && "$scenario" != "stream" ]]; then
  echo "--scenario must be unary or stream" >&2
  exit 2
fi
if [[ ! "$concurrency" =~ ^[1-9][0-9]*$ || ! "$repeat" =~ ^[1-9][0-9]*$ ]]; then
  echo "--concurrency and --repeat must be positive integers" >&2
  exit 2
fi
if [[ -n "$process_pid" && ! "$process_pid" =~ ^[1-9][0-9]*$ ]]; then
  echo "--process-pid must be a positive integer" >&2
  exit 2
fi
if [[ -n "$body_file" && ! -f "$body_file" ]]; then
  echo "request body file does not exist: $body_file" >&2
  exit 2
fi

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
go_command="${GO_COMMAND:-go}"
work_dir="$(mktemp -d)"
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

mkdir -p "$output_dir"
"$go_command" build -o "$work_dir/loadgen" ./benchmarks/blackbox/loadgen

for ((iteration=1; iteration<=repeat; iteration++)); do
  run_dir="$output_dir/run-$(printf '%03d' "$iteration")"
  mkdir -p "$run_dir"
  args=(
    -candidate "$candidate"
    -url "$target_url"
    -scenario "$scenario"
    -concurrency "$concurrency"
    -duration "$duration"
    -warmup "$warmup"
    -requests-output "$run_dir/requests.jsonl"
    -summary-output "$run_dir/summary.json"
  )
  if [[ -n "$process_pid" ]]; then
    args+=(-process-pid "$process_pid")
  fi
  if [[ -n "$profile_url" ]]; then
    args+=(-pprof-url "$profile_url" -pprof-output "$run_dir/cpu.pprof")
  fi
  if [[ -n "$body_file" ]]; then
    args+=(-body-file "$body_file")
  fi
  if [[ -n "$expect" ]]; then
    args+=(-expect "$expect")
  elif [[ "$scenario" == "stream" ]]; then
    args+=(-expect '[DONE]')
  fi
  for header in "${headers[@]}"; do
    args+=(-header "$header")
  done
  "$work_dir/loadgen" "${args[@]}" >"$run_dir/stdout.json"
done

echo "$output_dir"
