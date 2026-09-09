<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Sparkrun gateway-bridge acceptance contract

Current bridge contract: [schema v2 named-cluster metadata](SPARKRUN_CLUSTER_METADATA_CONTRACT.md).
The schema-v1 qualification below is historical; both current components require v2.

This document is the interoperability handoff for SparkRoute's consumer and
Sparkrun's producer implementations of the hidden one-shot
`sparkrun gateway-bridge` protocol.

## Invocation boundary

For each operation the gateway launches:

```text
<configured sparkrun executable> gateway-bridge
```

It writes exactly one protocol-version-1 JSON request to stdin and expects
exactly one correlated JSON response on stdout. Diagnostic output belongs on
stderr. The process exits after the operation; no bridge process is retained on
the inference path.

Both repositories should exercise the same operation sequence:

1. `capabilities` advertises protocol version 1 and `discover`, `ensure_ready`,
   and `stop`.
2. `discover` without a binding returns the bounded set of ready endpoints.
3. `ensure_ready` receives an operator-authored recipe, its immutable revision,
   optional cluster candidates/overrides, and a bounded timeout. It adopts or
   starts the exact binding and returns one ready OpenAI endpoint.
4. A subsequent filtered or global `discover` reports the same cluster, job,
   recipe revision, served model, and endpoint authority.
5. `stop` addresses the binding and returned cluster ID, is idempotent, and
   returns the affected cluster IDs without leaking scheduler logs or secrets.
6. A later `discover` no longer reports that stopped binding.

Every response must echo `schema_version` and `request_id`. Errors use a
non-empty bounded code and may mark retryability; raw command output, API keys,
and stored upstream credentials must never be returned.

## Endpoint invariants

The gateway accepts only endpoints which are ready, use the declared protocol,
serve the configured upstream model, and have a valid host/port plus bounded
cluster/job identity. Activatable endpoints must return the exact configured
recipe revision. SparkRoute supplies and validates the activation fencing
token internally; the bridge cannot choose or override it. Optional
`model_metadata` follows
[the metadata extension](SPARKRUN_MODEL_METADATA_CONTRACT.md) and cannot make a
valid inference endpoint ineligible when the extension itself is malformed.

## Test ownership

SparkRoute owns the consumer-side hermetic suite in:

- `cmd/sparkroute/sparkrun_bridge_fixture_test.go`; and
- `cmd/sparkroute/sparkrun_acceptance_test.go`.

It runs the compiled Go test executable as the one-shot bridge and proves
process correlation, warm discovery, Chat/Embeddings, concurrent activation,
state-pinned Responses, bounded queues, cancellation/timeout, revision/fencing,
metadata removal, disappearance, and idle stop. Registry expiry and stale
activation claims use clock-controlled package tests.

Sparkrun should own producer-side tests using an ephemeral local recipe and the
same six-operation sequence. Its fast CI tier may cover capabilities, strict
JSON/correlation, invalid bindings, and read-only discovery. A lifecycle tier
may create an ephemeral runtime, assert adoption/idempotency, and always stop it
in cleanup. The SparkRoute client in `pkg/sparkrun` is the canonical consumer
decoder and can be used by an external Go interoperability test without running
the gateway server.

Run the gateway-owned suite with:

```sh
go test -count=1 -run 'TestSparkrunRuntime' ./cmd/sparkroute
```
