# OSS implementation status

The reshuffled OSS module currently builds and passes its full unit/integration
suite independently with `GOWORK=off`.

Implemented OSS surfaces include:

- file and current-state-only SQLite managed configuration with atomic runtime reload;
- static and Sparkrun-discovered/activatable deployments;
- OpenAI Chat Completions, Responses, Conversations, Files, and Embeddings;
- native Anthropic Messages, Gemini, and Bedrock paths plus supported protocol
  translations through `github.com/scitrera/go-llm v0.2.0`;
- virtual-model routing, capabilities, retries/fallback, health/circuits,
  prefix affinity, selector policies, guardrails, MMBridge projection, and PII
  provider/protocol extension hooks;
- SQLite usage/client-credential stores;
- bounded asynchronous SQLite or filesystem saved traces and filtered CLI
  export;
- OpenTelemetry metrics/traces and optional OTLP export;
- basic embedded React administration; and
- exported enterprise extension seams, including `modelcatalog.Resolver`.

Not part of the OSS runtime composition:

- PostgreSQL/TimescaleDB stores and multi-replica coordination;
- tenant-selectable trusted-network/mTLS authentication;
- Aether model-catalog implementation;
- MLflow relay and cluster-only admin APIs; and
- concrete PII detection, reversible substitution, encrypted mapping storage,
  and PII admin controls; and
- the Kubernetes Helm deployment.

Before publishing the reshuffled OSS tree, repeat live Sparkrun managed-config,
periodic discovery, cold-load, unload, and admin-token acceptance against this
new path and validate the standalone multi-architecture container build.
