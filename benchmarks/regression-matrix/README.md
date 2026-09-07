# Activation, catalog, and trace regression matrix

This matrix turns three correctness-sensitive hot paths into repeatable tests
and benchmarks. It complements black-box gateway/provider load tests; it does
not replace them or claim production SLOs from shared CI runners.

Run the correctness gates locally:

```sh
./benchmarks/regression-matrix/run.sh --mode smoke
```

The smoke run uses the race detector and covers:

- cold activation ownership/fencing, concurrent waiter coalescing, queue bounds,
  and ready-path lease release;
- exact tenant/model catalog caching, concurrent cold-lookup singleflight,
  distinct-lookup bounds, negative caching, and policy validation; and
- forced saved-trace queue saturation with 32,768 unique records, exact
  accepted/persisted counts, zero queue/store loss, group-commit storage tests,
  and fail-before-write dataset completeness checks.

Collect timing/allocation samples:

```sh
mkdir -p .work
./benchmarks/regression-matrix/run.sh \
  --mode benchmark \
  --benchtime 2s \
  --count 5 \
  --output .work/regression-bench.txt
```

Use `--procs N` to pin `GOMAXPROCS` for comparisons. `GO_COMMAND` can select an
explicit Go binary. The result includes revision, dirty-worktree state, UTC
time, Go version, kernel/host description, and the following benchmark families:

| Family | Unit |
|---|---|
| activation ready path | acquire plus release per operation, parallel |
| activation cold coalescing | one activation with 64 waiters per operation |
| catalog positive cache | one cached lookup per operation, parallel |
| catalog cold validation | one distinct resolve/policy/materialization per operation, parallel |
| async trace backpressure | one validated/enqueued/persisted record per operation, parallel |
| SQLite trace group commit | one durable 256-record transaction per operation |
| filesystem trace group commit | one synchronized 256-record journal append per operation |

Do not make PRs fail on nanosecond thresholds from shared runners. The scheduled
workflow uploads raw repeated samples for trend review. Compare results only
when Go version, architecture, `GOMAXPROCS`, storage, and host class match. A
performance result is invalid if the smoke gate does not pass at the same
revision.

For meaningful resource/throughput conclusions, also run the black-box
loopback/provider load matrix on a dedicated node and record request latency,
TTFT, CPU seconds, peak RSS, exact trace counts, and storage mode. In particular,
Envoy/Traefik transport baselines remain incomparable to SparkRoute runs with
validation, routing, accounting, and saved-trace capture enabled unless those
feature differences are stated explicitly.
