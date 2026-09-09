<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Black-box gateway benchmark

The first matched-one-CPU run using this harness is recorded in
[`docs/history/BENCHMARK_RESULTS_MATCHED_CPU_2026-08-14.md`](../../docs/history/BENCHMARK_RESULTS_MATCHED_CPU_2026-08-14.md).
The follow-up A/B for pooled upstream transports and target-aware protocol
translation is recorded in
[`docs/history/BENCHMARK_RESULTS_TRANSPORT_TRANSLATION_OPTIMIZATIONS_2026-08-14.md`](../../docs/history/BENCHMARK_RESULTS_TRANSPORT_TRANSLATION_OPTIMIZATIONS_2026-08-14.md).

This harness measures a real HTTP data path without embedding the gateway in a
Go benchmark. It is intended for dedicated-node comparisons of SparkRoute,
Envoy, Traefik, and other candidates. The checked-in regression matrix remains
the fast correctness/performance gate; this harness supplies the feature-aware
latency, throughput, CPU, RSS, and trace evidence that microbenchmarks cannot.

## Evidence contract

Every run writes:

- `requests.jsonl`, including warm-up and measured request timestamps, status,
  latency, TTFB, response bytes, gateway request ID, and normalized failure;
- `summary.json`, including p50/p95/p99 latency and TTFB, throughput, status and
  failure counts, exact request-ID coverage, host architecture, and optional
  candidate CPU/RSS measurements; and
- optionally `cpu.pprof` when the candidate exposes a Go CPU-profile endpoint.

The load generator uses closed-loop workers, reuses connections, reads complete
responses, and waits for in-flight requests to finish after the scheduled cell
end. A request succeeds only after a 2xx response and any configured content
check. Warm-up requests are retained so a trace verifier can account for every
gateway-handled request rather than silently excluding setup traffic.

## Deterministic upstream

Start the included OpenAI-compatible upstream on the same host:

```sh
go run ./benchmarks/blackbox/mockupstream \
  -address 127.0.0.1:18000 \
  -unary-delay 20ms \
  -stream-chunk-delay 5ms \
  -stream-chunks 4
```

It prints one ready JSON object, serves Chat Completions at
`http://127.0.0.1:18000/v1/chat/completions`, and exposes content-free counters
at `/stats`. `sparkroute-config.json` is a minimal SparkRoute configuration for
that endpoint. Configure every other candidate to route the same benchmark
virtual model there.

The `fixtures/` directory contains pinned-port Envoy and Traefik transport
baselines for the same upstream. Run those containers with host networking;
the listeners are `127.0.0.1:18095` and `127.0.0.1:18096`, respectively.

## Run one repeated cell

```sh
./benchmarks/blackbox/run.sh \
  --candidate sparkroute-trace-block \
  --url http://127.0.0.1:8080/v1/chat/completions \
  --header 'X-SparkRoute-Tenant: benchmark-a' \
  --scenario unary \
  --concurrency 128 \
  --duration 30s \
  --warmup 2s \
  --repeat 3 \
  --process-pid 12345 \
  --output-dir .work/blackbox/sparkroute-unary-c128
```

For streaming, select `--scenario stream`; the wrapper automatically requires
the `[DONE]` terminal marker. `--body-file` permits large-body, translation,
tool, multimedia, retry, and guardrail cases without changing the load tool.
The checked-in `requests/chat-unary.json` and `requests/chat-stream.json`
fixtures target `sparkroute-config.json`.

`--process-pid` samples Linux `/proc` and records CPU seconds, average cores,
CPU microseconds per successful request, and peak resident memory. It must be
the candidate process as seen by the load-generator host. For containers, use
the host PID or collect equivalent cgroup counters and retain them beside the
run. Do not compare a host PID for one candidate with cgroup totals for another.

`--pprof-url http://127.0.0.1:9090` captures
`/debug/pprof/profile?seconds=<cell>` into each run directory when the candidate
exposes that endpoint. Absence or failure is explicit in `summary.json`; it does
not invalidate request evidence. CPU profiling has overhead; collect a separate
diagnostic repeat rather than mixing a profiled SparkRoute cell with unprofiled
comparison candidates.

## Prove trace completeness

Export canonical SparkRoute traces for a run's complete time window, including
warm-up, and reconcile them by gateway request ID:

```sh
./benchmarks/blackbox/verify-traces.sh \
  --requests .work/blackbox/sparkroute-unary-c128/run-001/requests.jsonl \
  --traces .work/blackbox/sparkroute-unary-c128/run-001/traces.jsonl \
  --output .work/blackbox/sparkroute-unary-c128/run-001/trace-verification.json
```

The verifier fails on missing or duplicate traces and, by default, on traces
outside the request evidence. Use `--allow-extra` only when a shared export
window necessarily includes unrelated traffic; expected request IDs must still
be present exactly once. A SparkRoute performance cell with trace capture is
invalid until this verification passes.

For a matrix stored in one trace journal, repeat `--requests` for every cell's
request JSONL and verify the combined evidence against one canonical export.
This avoids concatenating large evidence files and keeps duplicate IDs visible.

Envoy and Traefik transport baselines normally have no analogous trace capture.
Mark those cells `trace=not_applicable`; never report them as feature-equivalent
to SparkRoute trace-enabled cells.

## Required comparison matrix

On the intended Kubernetes node class, use identical CPU/memory limits and
cpusets, rotate candidate order, and retain at least three 30-second repeats:

1. zero-delay unary and SSE transport baselines;
2. 20 ms unary and 5 ms/chunk SSE provider-delay profiles;
3. concurrency 1, 8, 32, and 128;
4. connection reuse and churn, HTTP/1.1 and HTTP/2, TLS/mTLS, large bodies,
   cancellations, and slow readers;
5. SparkRoute trace disabled, block/backpressure, and future strict durability;
6. retries/fallback, protocol translation, prompt-prefix affinity, guardrails,
   and warm/cold activation as separate AI-aware profiles.

Record the candidate revision, image digest, configuration, CPU/memory limit,
cpuset, kernel, Go/compiler version, upstream settings, and raw run directories.
Compare transport-only candidates only in transport-only profiles. A profile
that validates, routes, accounts, captures traces, or activates a model is a
different workload and must be labeled accordingly.

For a controlled single-host comparison, `compare-local.sh` automates the
zero-delay and optional delayed profiles with a matched one-CPU candidate
cpuset, a separate upstream CPU, rotated candidate order, raw request evidence,
and pinned Envoy/Traefik fixtures. It refuses to overwrite an existing output
directory:

```sh
GO_COMMAND=/path/to/go ./benchmarks/blackbox/compare-local.sh \
  --output-dir .work/blackbox-comparison \
  --duration 30s --warmup 2s --repeats 3 --profiles zero,delayed
```
