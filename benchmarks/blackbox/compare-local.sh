#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Scitrera LLC
# SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
# SPDX-License-Identifier: AGPL-3.0-only


set -euo pipefail

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
output_dir=""
duration="30s"
warmup="2s"
repeats=3
profiles="zero"
candidate_cpu="5"
upstream_cpu="15"
load_cpus="6-9,16-19"
envoy_image="envoyproxy/envoy:v1.38.1"
traefik_image="traefik:v3.7.10"

usage() {
  echo "usage: $0 --output-dir DIR [--duration D] [--warmup D] [--repeats N] [--profiles zero|zero,delayed] [--candidate-cpu CPU] [--upstream-cpu CPU] [--load-cpus LIST]"
}

while (($# > 0)); do
  case "$1" in
    --output-dir) output_dir="$2"; shift 2 ;;
    --duration) duration="$2"; shift 2 ;;
    --warmup) warmup="$2"; shift 2 ;;
    --repeats) repeats="$2"; shift 2 ;;
    --profiles) profiles="$2"; shift 2 ;;
    --candidate-cpu) candidate_cpu="$2"; shift 2 ;;
    --upstream-cpu) upstream_cpu="$2"; shift 2 ;;
    --load-cpus) load_cpus="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$output_dir" || ! "$repeats" =~ ^[1-9][0-9]*$ ]]; then
  usage >&2
  exit 2
fi
if [[ -e "$output_dir" ]]; then
  echo "output directory already exists: $output_dir" >&2
  exit 2
fi
for command in docker jq taskset curl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 2; }
done

cd "$repository"
mkdir -p "$output_dir/bin" "$output_dir/logs" "$output_dir/results" "$output_dir/trace-store"
chmod 700 "$output_dir/trace-store"
go_command="${GO_COMMAND:-go}"
"$go_command" build -o "$output_dir/bin/sparkroute" ./cmd/sparkroute
"$go_command" build -o "$output_dir/bin/mockupstream" ./benchmarks/blackbox/mockupstream
"$go_command" build -o "$output_dir/bin/loadgen" ./benchmarks/blackbox/loadgen
"$go_command" build -o "$output_dir/bin/traceverify" ./benchmarks/blackbox/traceverify
cp benchmarks/blackbox/sparkroute-config.json "$output_dir/sparkroute-config.json"

git rev-parse HEAD >"$output_dir/revision.txt"
git status --short >"$output_dir/worktree-status.txt"
uname -a >"$output_dir/uname.txt"
lscpu >"$output_dir/lscpu.txt"
docker version >"$output_dir/docker-version.txt"
docker images --digests --format '{{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}' >"$output_dir/images.txt"
{
  echo "duration=$duration"
  echo "warmup=$warmup"
  echo "repeats=$repeats"
  echo "profiles=$profiles"
  echo "candidate_cpu=$candidate_cpu"
  echo "upstream_cpu=$upstream_cpu"
  echo "load_cpus=$load_cpus"
  echo "envoy_image=$envoy_image"
  echo "traefik_image=$traefik_image"
} >"$output_dir/run.env"

upstream_pid=""
candidate_pid=""
candidate_container=""
candidate_log=""
run_token="sparkroute-blackbox-$$"

stop_candidate() {
  if [[ -n "$candidate_pid" ]]; then
    kill -TERM "$candidate_pid" 2>/dev/null || true
    wait "$candidate_pid" 2>/dev/null || true
    candidate_pid=""
  fi
  if [[ -n "$candidate_container" ]]; then
    docker logs "$candidate_container" >"$candidate_log" 2>&1 || true
    docker rm -f "$candidate_container" >/dev/null 2>&1 || true
    candidate_container=""
  fi
}

stop_upstream() {
  if [[ -n "$upstream_pid" ]]; then
    kill -TERM "$upstream_pid" 2>/dev/null || true
    wait "$upstream_pid" 2>/dev/null || true
    upstream_pid=""
  fi
}

cleanup() {
  stop_candidate
  stop_upstream
}
trap cleanup EXIT INT TERM

wait_get() {
  local url="$1"
  for _ in {1..200}; do
    if curl -fs -o /dev/null "$url"; then
      return 0
    fi
    sleep 0.05
  done
  echo "endpoint did not become ready: $url" >&2
  return 1
}

wait_inference() {
  local url="$1"
  for _ in {1..200}; do
    if curl -fs -o /dev/null -H 'Content-Type: application/json' \
      --data '{"model":"benchmark","messages":[{"role":"user","content":"ready"}]}' "$url"; then
      return 0
    fi
    sleep 0.05
  done
  echo "candidate did not become ready: $url" >&2
  return 1
}

start_upstream() {
  local profile="$1"
  local unary_delay="0s"
  local chunk_delay="0s"
  if [[ "$profile" == "delayed" ]]; then
    unary_delay="20ms"
    chunk_delay="5ms"
  elif [[ "$profile" != "zero" ]]; then
    echo "unknown profile: $profile" >&2
    return 2
  fi
  taskset -c "$upstream_cpu" env GOMAXPROCS=1 \
    "$output_dir/bin/mockupstream" -address 127.0.0.1:18000 \
    -unary-delay "$unary_delay" -stream-chunk-delay "$chunk_delay" -stream-chunks 4 \
    >"$output_dir/logs/upstream-$profile.log" 2>&1 &
  upstream_pid=$!
  wait_get http://127.0.0.1:18000/health
}

start_candidate() {
  local candidate="$1"
  local profile="$2"
  local repeat="$3"
  candidate_log="$output_dir/logs/$profile-repeat-$repeat-$candidate.log"
  case "$candidate" in
    direct)
      candidate_pid="$upstream_pid"
      candidate_url="http://127.0.0.1:18000/v1/chat/completions"
      ;;
    sparkroute-disabled|sparkroute-trace)
      local trace_args=(-trace-storage disabled)
      if [[ "$candidate" == "sparkroute-trace" ]]; then
        trace_args=(
          -trace-storage filesystem
          -trace-filesystem "$output_dir/trace-store"
          -trace-overflow-policy block
          -trace-queue-capacity 4096
          -trace-write-workers 1
          -trace-batch-size 256
          -trace-batch-interval 2ms
          -trace-session-interval 250ms
        )
      fi
      taskset -c "$candidate_cpu" env GOMAXPROCS=1 OTEL_SDK_DISABLED=true \
        "$output_dir/bin/sparkroute" -config "$output_dir/sparkroute-config.json" \
        -data-address 127.0.0.1:18091 -admin-address= -operations-address 127.0.0.1:18099 \
        "${trace_args[@]}" >"$candidate_log" 2>&1 &
      candidate_pid=$!
      candidate_url="http://127.0.0.1:18091/v1/chat/completions"
      wait_get http://127.0.0.1:18099/health/ready
      ;;
    envoy)
      candidate_container="$run_token-envoy"
      docker run -d --name "$candidate_container" --network host --cpuset-cpus "$candidate_cpu" \
        -v "$repository/benchmarks/blackbox/fixtures/envoy.yaml:/etc/envoy/envoy.yaml:ro" \
        "$envoy_image" -c /etc/envoy/envoy.yaml --concurrency 1 --disable-hot-restart >/dev/null
      candidate_pid="$(docker inspect --format '{{.State.Pid}}' "$candidate_container")"
      candidate_url="http://127.0.0.1:18095/v1/chat/completions"
      wait_get http://127.0.0.1:18195/ready
      ;;
    traefik)
      candidate_container="$run_token-traefik"
      docker run -d --name "$candidate_container" --network host --cpuset-cpus "$candidate_cpu" \
        -e GOMAXPROCS=1 \
        -v "$repository/benchmarks/blackbox/fixtures/traefik-static.yaml:/etc/traefik/benchmark-static.yaml:ro" \
        -v "$repository/benchmarks/blackbox/fixtures/traefik-dynamic.yaml:/etc/traefik/benchmark-dynamic.yaml:ro" \
        "$traefik_image" --configFile=/etc/traefik/benchmark-static.yaml >/dev/null
      candidate_pid="$(docker inspect --format '{{.State.Pid}}' "$candidate_container")"
      candidate_url="http://127.0.0.1:18096/v1/chat/completions"
      wait_inference "$candidate_url"
      ;;
    *)
      echo "unknown candidate: $candidate" >&2
      return 2
      ;;
  esac
}

run_candidate() {
  local profile="$1"
  local repeat="$2"
  local candidate="$3"
  start_candidate "$candidate" "$profile" "$repeat"
  echo "candidate_start profile=$profile repeat=$repeat candidate=$candidate pid=$candidate_pid"
  for scenario in unary stream; do
    for concurrency in 1 8 32 128; do
      local cell="$output_dir/results/$profile/repeat-$(printf '%02d' "$repeat")/$candidate/$scenario-c$concurrency"
      mkdir -p "$cell"
      local expected="chatcmpl-benchmark"
      if [[ "$scenario" == "stream" ]]; then
        expected="[DONE]"
      fi
      taskset -c "$load_cpus" "$output_dir/bin/loadgen" \
        -candidate "$candidate" -url "$candidate_url" -scenario "$scenario" \
        -concurrency "$concurrency" -duration "$duration" -warmup "$warmup" \
        -process-pid "$candidate_pid" -expect "$expected" \
        -requests-output "$cell/requests.jsonl" -summary-output "$cell/summary.json" \
        >"$cell/stdout.json"
      jq -e '.measurement.failed == 0 and .warmup.failed == 0' "$cell/summary.json" >/dev/null
      jq -r '"cell_done candidate=\(.candidate) scenario=\(.scenario) concurrency=\(.concurrency) rps=\(.measurement.requests_per_second) p99_us=\(.measurement.latency_us.p99) cpu_cores=\(.process.average_cpu_cores) rss=\(.process.peak_rss_bytes)"' "$cell/summary.json"
    done
  done
  if [[ "$candidate" != "direct" ]]; then
    stop_candidate
  else
    candidate_pid=""
  fi
}

IFS=',' read -r -a profile_list <<<"$profiles"
orders=(
  "direct sparkroute-disabled envoy traefik sparkroute-trace"
  "envoy sparkroute-trace direct sparkroute-disabled traefik"
  "sparkroute-trace traefik sparkroute-disabled direct envoy"
)
for profile in "${profile_list[@]}"; do
  start_upstream "$profile"
  for ((repeat=1; repeat<=repeats; repeat++)); do
    order_index=$(((repeat - 1) % ${#orders[@]}))
    read -r -a candidates <<<"${orders[$order_index]}"
    for candidate in "${candidates[@]}"; do
      run_candidate "$profile" "$repeat" "$candidate"
    done
  done
  stop_upstream
done

echo "comparison_complete output=$output_dir"
