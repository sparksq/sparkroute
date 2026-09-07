# Sparkrun integration readiness review

Status: gateway and Sparkrun source implementations are interoperable and
their automated contract suites pass. Release packaging and a real-cluster
cold-start exercise remain before general availability.

## Reviewed revisions

- SparkRoute: `3751022` plus the later routing commits on `main` when this
  review was performed.
- Sparkrun: `0fc9711` on `feature/llm-gateway-integration`.
- Sparkrun worktree: clean at review time.

The authoritative wire and metadata contracts are:

- [Sparkrun gateway-bridge acceptance contract](SPARKRUN_GATEWAY_BRIDGE_ACCEPTANCE.md)
- [Sparkrun bridge model-metadata extension](SPARKRUN_MODEL_METADATA_CONTRACT.md)
- Sparkrun's `docs/LLM_GATEWAY_BRIDGE.md`
- Sparkrun's `docs/LLM_GATEWAY_ADMIN_CONTRACT.md`
- Sparkrun's `docs/LLM_GATEWAY_MANAGED_CONFIG_RESPONSE.md`
- Sparkrun's `docs/LLM_GATEWAY_PROTOCOL_CAPABILITIES.md`

## Implemented shape

Sparkrun ships an in-tree, feature-gated integration with both directions of
the boundary:

- it supervises a local SparkRoute process in SQLite managed-configuration
  mode and reconciles only the `sparkrun` owner set through authenticated CAS;
- it exposes a hidden `sparkrun gateway-bridge` one-shot JSON command used by
  SparkRoute to discover, activate, inspect, and stop local inference
  workloads; and
- it projects one provider named `sparkrun`, revision-keyed deployments,
  unprefixed virtual models, explicitly declared capabilities, permissive
  unknown capability handling, and fail-closed native protocol declarations.

The integration is disabled unless the `gateway.scitrera_oss` feature flag is
enabled. The hidden command does not appear in ordinary `sparkrun --help` but
is directly invocable by the gateway.

## Wire compatibility

Both implementations speak bridge schema version 1. SparkRoute invokes one
executable path as:

```text
<sparkrun executable> gateway-bridge
```

and supplies one bounded JSON object on stdin. Sparkrun returns one correlated
JSON object on stdout. The Go decoder rejects unknown fields, so result key
sets are a hard compatibility boundary.

The reviewed live command returned:

```json
{
  "schema_version": 1,
  "request_id": "readiness-1",
  "ok": true,
  "result": {
    "operations": [
      "capabilities",
      "resolve",
      "ensure_ready",
      "discover",
      "status",
      "stop"
    ],
    "protocol_version": 1,
    "sparkrun_version": "0.3.4"
  }
}
```

SparkRoute currently requires `discover`, `ensure_ready`, and `stop`; the
additional operations are compatible. Endpoint projections, including the
optional bounded `model_metadata`, match the Go structs.

## Verification performed

On the Sparkrun feature branch:

- 129 focused bridge, gateway engine, projection, admin, release, and in-tree
  plugin tests passed;
- the full test suite completed with 5,487 passes;
- three browser-login tests initially failed only because the restricted test
  sandbox denied creation of a localhost socket; all three passed when rerun
  with localhost socket access; and
- the real hidden CLI capabilities request returned a correlated valid
  response.

On the SparkRoute side, the real subprocess acceptance fixture covers warm
Chat and Embeddings, concurrent cold activation, selector-pinned Responses,
bounded waiter count/body bytes, cancellation and activation timeout, recipe
revision and fencing mismatch, discovery metadata publication/removal,
endpoint disappearance, and idle stop. It has passed normal, repeated, and
race-enabled runs.

## Remaining release blockers

### Published gateway artifacts

Sparkrun's `plugins/llm_gateway/release.py` intentionally has an empty
`RELEASE_CHECKSUMS` table. Source development works through
`SPARKRUN_LLM_GATEWAY_BINARY`, but normal installation fails closed until the
gateway publishes versioned assets and Sparkrun pins their SHA-256 values.

At minimum publish and pin:

- Linux amd64; and
- Linux arm64.

Darwin/Windows entries should be added only when those binaries are supported
and tested. An unpinned platform must remain a hard error.

### Real-cluster lifecycle exercise

Run one acceptance scenario on a small local cluster with a real recipe and
runtime:

1. enable `gateway.scitrera_oss`;
2. reconcile an offline activatable binding;
3. issue concurrent inference requests and prove one activation;
4. verify Chat or Embeddings reaches the launched runtime;
5. verify discovery and public model metadata;
6. allow the idle lease to stop the workload; and
7. confirm a later request can activate it again.

The test should capture gateway and Sparkrun versions, recipe revision,
activation latency, selected cluster, and cleanup result without recording
credentials.

## Non-blocking follow-up

Bridge version negotiation is still effectively pinned to version 1. Sparkrun
already models a supported-version range, while the Go client sends and
requires exactly version 1. This does not block the current integration, but a
gateway-first protocol negotiation extension should land before introducing
schema version 2.

## Readiness conclusion

The source-level integration is ready for development and controlled cluster
acceptance. It should not be described as turnkey release-ready until
multi-architecture release assets/checksums are pinned and the real-cluster
cold-start scenario passes.
