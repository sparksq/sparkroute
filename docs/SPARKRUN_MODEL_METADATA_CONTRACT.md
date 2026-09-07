# Sparkrun bridge model-metadata extension

Status: implemented in SparkRoute; optional Sparkrun producer support may be
added without changing gateway-bridge protocol version 1.

## Purpose

Sparkrun already discovers the models served by each local runtime. This
extension lets it return the public strategy metadata from that same model card
so SparkRoute selectors do not require duplicate hand-authored size, price,
context, or tag values.

The extension is advisory. It cannot register an endpoint, add a logical model,
enable a disabled model, change routing weight/priority, configure multimedia
projection, or deliver credentials.

## JSON shape

`discover` and `ensure_ready` endpoint objects may include `model_metadata`:

```json
{
  "state": "ready",
  "cluster_id": "sparkrun_...",
  "job_id": "sparkrun_...",
  "host": "10.0.0.12",
  "port": 8000,
  "protocol": "openai",
  "served_models": ["Qwen/Qwen3-32B"],
  "recipe": "@local/qwen3-32b",
  "recipe_revision": "abc123abc123",
  "runtime": "vllm",
  "model_metadata": {
    "Qwen/Qwen3-32B": {
      "size_b": 32,
      "input_price": 0,
      "output_price": 0,
      "context": 65536,
      "tags": ["local", "vllm"]
    }
  }
}
```

The map key is the exact upstream model identity and must also appear in
`served_models`. Every field inside its value is optional:

| Field | Contract |
| --- | --- |
| `size_b` | finite number greater than zero, in billions of parameters |
| `input_price` | finite non-negative number per million input tokens |
| `output_price` | finite non-negative number per million output tokens |
| `context` | positive integer token limit |
| `tags` | at most 128 unique presentation-safe strings, each at most 128 bytes |

An endpoint may publish at most 256 metadata entries. The complete bridge
response remains subject to the existing 1 MiB stdout limit. Secret values,
credential references, endpoint URLs, arbitrary model-card JSON, and error text
must not appear in `model_metadata`.

## Producer behavior

Sparkrun should populate only values it can determine from runtime/recipe model
metadata. Omit an unknown field; do not infer a model-family default. In
particular, for context length use the effective runtime limit rather than a
larger nominal family limit. A known free price is represented by numeric zero;
an unknown price is omitted.

Ordering is not significant. SparkRoute normalizes tags and snapshots the
entire source atomically on each reconciliation.

## Consumer behavior

SparkRoute validates every contribution before publishing it to the immutable
routing snapshot. Invalid optional metadata is discarded while the valid
inference endpoint remains registered.

One upstream deployment may back multiple logical virtual models; its metadata
is copied to each configured logical model. Multiple reports are reduced
conservatively:

- context: smallest positive value;
- size and prices: maximum reported value;
- tags: sorted union.

Operator-authored positive numeric fields and non-empty tags take precedence.
`discovery_disabled: true` on a logical model suppresses all enrichment. A
refresh neither edits persisted configuration nor changes its revision.

The authenticated read-only projection is
`GET /v1/model-routing/discovered-metadata` for principals with `config_read`.
It exposes logical model names, public values, source identifiers, and observed
timestamps only.

## Compatibility and rollout

No new bridge operation is required and protocol version remains 1. SparkRoute
Route's strict decoder now recognizes the optional field. Therefore rollout can
be gateway-first:

1. deploy a SparkRoute build containing this contract;
2. add `model_metadata` to Sparkrun `discover` and `ensure_ready` results;
3. verify the admin discovery projection and a `smallest`, `largest`, or
   `lowest_cost` simulation;
4. retain omission as the fallback for runtimes that cannot report metadata.
